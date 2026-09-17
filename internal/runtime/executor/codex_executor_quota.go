package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/util"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/sjson"
)

const (
	codexQuotaUsageURL         = "https://chatgpt.com/backend-api/wham/usage"
	codexAccountsURL           = "https://chatgpt.com/backend-api/accounts"
	codexFiveHourWindowSeconds = 5 * 60 * 60
	codexWeeklyWindowSeconds   = 7 * 24 * 60 * 60
	codexAccountsMaxBodyBytes  = 1 << 20
)

func (e *CodexExecutor) RefreshQuota(ctx context.Context, auth *cliproxyauth.Auth) (*cliproxyauth.QuotaInfo, error) {
	apiKey, _ := codexCreds(auth)
	if strings.TrimSpace(apiKey) == "" {
		return nil, fmt.Errorf("codex quota refresh: missing access token")
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, codexQuotaUsageURL, nil)
	if err != nil {
		return nil, err
	}
	applyCodexQuotaUsageHeaders(httpReq, auth, apiKey)
	httpClient := helps.NewProxyAwareHTTPClient(ctx, e.cfg, auth, 0)
	httpResp, err := httpClient.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer func() {
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("codex executor: close quota response body error: %v", errClose)
		}
	}()
	data, err := io.ReadAll(httpResp.Body)
	if err != nil {
		return nil, err
	}
	quota, _ := parseCodexQuotaInfo(data, time.Now())
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		return quota, fmt.Errorf("codex quota refresh: status %d: %s", httpResp.StatusCode, helps.SummarizeErrorBody(httpResp.Header.Get("Content-Type"), data))
	}
	return quota, nil
}

func (e *CodexExecutor) RefreshWorkspaceMetadata(ctx context.Context, auth *cliproxyauth.Auth) (map[string]string, error) {
	apiKey, _ := codexCreds(auth)
	if strings.TrimSpace(apiKey) == "" {
		return nil, fmt.Errorf("codex workspace metadata refresh: missing access token")
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, codexAccountsURL, nil)
	if err != nil {
		return nil, err
	}
	applyCodexQuotaUsageHeaders(httpReq, auth, apiKey)
	httpReq.Header.Set("Accept", "application/json")
	httpClient := helps.NewUtlsHTTPClient(ctx, e.cfg, auth, 0)
	httpResp, err := httpClient.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer func() {
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("codex executor: close workspace metadata response body error: %v", errClose)
		}
	}()
	data, err := io.ReadAll(io.LimitReader(httpResp.Body, codexAccountsMaxBodyBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > codexAccountsMaxBodyBytes {
		return nil, fmt.Errorf("codex workspace metadata refresh: response exceeds %d bytes", codexAccountsMaxBodyBytes)
	}
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		return nil, fmt.Errorf("codex workspace metadata refresh: status %d: %s", httpResp.StatusCode, helps.SummarizeErrorBody(httpResp.Header.Get("Content-Type"), data))
	}
	workspaceNames, err := parseCodexWorkspaceNames(data)
	if err != nil {
		return nil, fmt.Errorf("codex workspace metadata refresh: %w", err)
	}
	return workspaceNames, nil
}

func parseCodexWorkspaceNames(data []byte) (map[string]string, error) {
	var payload struct {
		Items []struct {
			ID   string  `json:"id"`
			Name *string `json:"name"`
		} `json:"items"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	workspaceNames := make(map[string]string, len(payload.Items))
	for _, item := range payload.Items {
		accountID := strings.TrimSpace(item.ID)
		if accountID == "" || item.Name == nil {
			continue
		}
		workspaceName := strings.TrimSpace(*item.Name)
		if workspaceName != "" {
			workspaceNames[accountID] = workspaceName
		}
	}
	return workspaceNames, nil
}

func (e *CodexExecutor) ProbeQuotaCountdown(ctx context.Context, auth *cliproxyauth.Auth) (*cliproxyauth.QuotaInfo, error) {
	apiKey, baseURL := codexCreds(auth)
	if strings.TrimSpace(apiKey) == "" {
		return nil, fmt.Errorf("codex quota probe: missing access token")
	}
	if baseURL == "" {
		baseURL = "https://chatgpt.com/backend-api/codex"
	}
	model := codexQuotaProbeModel(auth)
	if model == "" {
		return nil, fmt.Errorf("codex quota probe: no supported model")
	}
	body := codexQuotaProbePayload(model)
	requestedAt := time.Now()
	url := strings.TrimSuffix(baseURL, "/") + "/responses"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	applyCodexHeaders(httpReq, auth, apiKey, false, e.cfg)
	httpClient := helps.NewProxyAwareHTTPClient(ctx, e.cfg, auth, 0)
	httpResp, err := httpClient.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer func() {
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("codex executor: close quota probe response body error: %v", errClose)
		}
	}()
	data, err := io.ReadAll(httpResp.Body)
	// This internal POST generates tokens too, but has no billable client key.
	// Account it directly so quota probes cannot bypass a credential's dollar cap.
	detail := helps.ParseOpenAIUsage(data)
	if parsed, ok := helps.ParseCodexUsage(data); ok && (parsed.TotalTokens > 0 || detail.TotalTokens == 0) {
		detail = parsed
	}
	for _, line := range bytes.Split(data, []byte("\n")) {
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		if parsed, ok := helps.ParseCodexUsage(bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))); ok && (parsed.TotalTokens > 0 || detail.TotalTokens == 0) {
			detail = parsed
		}
	}
	usage.RecordAuthUsage(ctx, usage.Record{
		AuthIndex: auth.Clone().EnsureIndex(), Provider: "codex", ExecutorType: "CodexExecutor",
		Model: model, RequestedAt: requestedAt, Detail: detail, ResponseServiceTier: detail.ResponseServiceTier,
		ServiceTier: usage.DefaultServiceTier, ResponseHeaders: httpResp.Header,
		Failed: err != nil || httpResp.StatusCode < 200 || httpResp.StatusCode >= 300,
	})
	if err != nil {
		return nil, err
	}
	quota, _ := parseCodexQuotaInfo(data, time.Now())
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		return quota, fmt.Errorf("codex quota probe: status %d: %s", httpResp.StatusCode, helps.SummarizeErrorBody(httpResp.Header.Get("Content-Type"), data))
	}
	return quota, nil
}

func codexQuotaProbePayload(model string) []byte {
	payload := []byte(fmt.Sprintf(`{"model":%q,"instructions":"Answer briefly.","input":"hello","stream":false}`, model))
	body := sdktranslator.TranslateRequest(sdktranslator.FromString("openai-response"), sdktranslator.FromString("codex"), model, payload, false)
	body, _ = sjson.SetBytes(body, "model", model)
	body, _ = sjson.DeleteBytes(body, "previous_response_id")
	body, _ = sjson.DeleteBytes(body, "prompt_cache_retention")
	body, _ = sjson.DeleteBytes(body, "safety_identifier")
	body, _ = sjson.DeleteBytes(body, "stream_options")
	return normalizeCodexInstructions(body)
}

func applyCodexQuotaUsageHeaders(req *http.Request, auth *cliproxyauth.Auth, apiKey string) {
	if req == nil {
		return
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", codexUserAgent)
	if auth != nil && auth.Metadata != nil {
		if accountID, ok := auth.Metadata["account_id"].(string); ok && strings.TrimSpace(accountID) != "" {
			req.Header.Set("Chatgpt-Account-Id", strings.TrimSpace(accountID))
		}
	}
	var attrs map[string]string
	if auth != nil {
		attrs = auth.Attributes
	}
	util.ApplyCustomHeadersFromAttrs(req, attrs)
}

func codexQuotaProbeModel(auth *cliproxyauth.Auth) string {
	models := registry.GetGlobalRegistry().GetModelsForClient(authID(auth))
	if len(models) == 0 {
		models = registry.GetCodexPlusModels()
	}
	var fallback string
	for _, model := range models {
		if model == nil {
			continue
		}
		id := strings.TrimSpace(model.ID)
		if id == "" {
			continue
		}
		lower := strings.ToLower(id)
		if lower == "gpt-5.5" {
			return id
		}
		if fallback == "" && !strings.Contains(lower, "image") && !strings.Contains(lower, "review") {
			fallback = id
		}
	}
	return fallback
}

func authID(auth *cliproxyauth.Auth) string {
	if auth == nil {
		return ""
	}
	return auth.ID
}

func parseCodexQuotaInfo(data []byte, now time.Time) (*cliproxyauth.QuotaInfo, bool) {
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, false
	}
	var root any
	if err := json.Unmarshal(data, &root); err != nil {
		return nil, false
	}
	var quota cliproxyauth.QuotaInfo
	hasTopLevelRateLimit := false
	if rootMap, ok := root.(map[string]any); ok {
		if topLevelQuota, okQuota := parseCodexTopLevelRateLimitQuota(rootMap, now); okQuota {
			quota = *topLevelQuota
			hasTopLevelRateLimit = true
		}
		if resetCredits, okCredits := parseCodexQuotaResetCredits(rootMap); okCredits {
			quota.RateLimitResetCredits = resetCredits
		}
	}
	if !hasTopLevelRateLimit {
		parseCodexQuotaNode(root, "", &quota, now)
	}
	if !quota.HasAny() {
		return nil, false
	}
	return &quota, true
}

func parseCodexQuotaResetCredits(root map[string]any) (*cliproxyauth.QuotaResetCredits, bool) {
	values, ok := firstMap(root, "rate_limit_reset_credits", "rateLimitResetCredits")
	if !ok {
		return nil, false
	}
	availableCount, hasAvailable := firstInt64(values, "available_count", "availableCount")
	applicableCount, hasApplicable := firstInt64(values, "applicable_available_count", "applicableAvailableCount")
	if !hasAvailable && !hasApplicable {
		return nil, false
	}
	return &cliproxyauth.QuotaResetCredits{AvailableCount: availableCount, ApplicableAvailableCount: applicableCount}, true
}

func parseCodexTopLevelRateLimitQuota(root map[string]any, now time.Time) (*cliproxyauth.QuotaInfo, bool) {
	raw, ok := root["rate_limit"]
	if !ok {
		raw, ok = root["rateLimit"]
	}
	if !ok {
		return nil, false
	}
	rateLimit, ok := raw.(map[string]any)
	if !ok {
		return nil, false
	}
	var quota cliproxyauth.QuotaInfo
	for _, candidate := range []struct {
		keys []string
		path string
	}{
		{keys: []string{"primary_window", "primaryWindow"}, path: "rate_limit.primary_window"},
		{keys: []string{"secondary_window", "secondaryWindow"}, path: "rate_limit.secondary_window"},
	} {
		values, okWindowMap := firstMap(rateLimit, candidate.keys...)
		if !okWindowMap {
			continue
		}
		window, okWindow := codexQuotaWindowFromMap(values, candidate.path, now)
		if !okWindow {
			continue
		}
		assignCodexQuotaWindow(&quota, codexQuotaWindowKind(values, candidate.path), window)
	}
	if !codexQuotaWindowKnown(quota.FiveHour) && !codexQuotaWindowKnown(quota.Weekly) {
		return nil, false
	}
	return &quota, true
}

func parseCodexQuotaNode(node any, path string, quota *cliproxyauth.QuotaInfo, now time.Time) {
	switch typed := node.(type) {
	case map[string]any:
		if window, ok := codexQuotaWindowFromMap(typed, path, now); ok {
			assignCodexQuotaWindow(quota, codexQuotaWindowKind(typed, path), window)
		}
		for key, value := range typed {
			childPath := key
			if path != "" {
				childPath = path + "." + key
			}
			parseCodexQuotaNode(value, childPath, quota, now)
		}
	case []any:
		for _, value := range typed {
			parseCodexQuotaNode(value, path, quota, now)
		}
	}
}

func assignCodexQuotaWindow(quota *cliproxyauth.QuotaInfo, kind string, window cliproxyauth.QuotaWindow) {
	if quota == nil {
		return
	}
	switch kind {
	case "five_hour":
		quota.FiveHour = window
	case "weekly":
		quota.Weekly = window
	}
}

func codexQuotaWindowFromMap(values map[string]any, path string, now time.Time) (cliproxyauth.QuotaWindow, bool) {
	if codexQuotaWindowKind(values, path) == "" {
		return cliproxyauth.QuotaWindow{}, false
	}
	window := cliproxyauth.QuotaWindow{}
	window.LimitWindowSeconds, _ = firstInt64(values, "limit_window_seconds", "limitWindowSeconds")
	window.Used, _ = firstInt64(values, "used", "used_tokens", "tokens_used", "current", "consumed")
	window.Limit, _ = firstInt64(values, "limit", "token_limit", "tokens_limit", "max", "total", "capacity")
	if usedPercent, ok := firstFloat64(values, "used_percent"); ok {
		window.UsedPercent = clampCodexUsedPercent(usedPercent)
		window.UsedPercentKnown = true
	} else if window.Limit > 0 && window.Used >= 0 {
		window.UsedPercent = clampCodexUsedPercent(float64(window.Used) * 100 / float64(window.Limit))
		window.UsedPercentKnown = true
	}
	if next, ok := firstTime(values, now, "next_fresh_at", "nextFreshAt", "resets_at", "resetsAt", "reset_at", "resetAt", "next_reset_at", "nextResetAt", "fresh_at", "freshAt"); ok {
		window.NextFreshAt = next
	} else if resetAfter, ok := firstFloat64(values, "reset_after_seconds", "resetAfterSeconds"); ok && resetAfter > 0 {
		window.NextFreshAt = now.Add(time.Duration(resetAfter) * time.Second)
	}
	if refreshed, ok := firstTime(values, now, "refreshed_at", "refreshedAt", "updated_at", "updatedAt", "created_at", "createdAt"); ok {
		window.RefreshedAt = refreshed
	} else if codexQuotaWindowKnown(window) {
		window.RefreshedAt = now
	}
	return window, codexQuotaWindowKnown(window)
}

func clampCodexUsedPercent(value float64) float64 {
	if value < 0 {
		return 0
	}
	if value > 100 {
		return 100
	}
	return value
}

func codexQuotaWindowKind(values map[string]any, path string) string {
	if seconds, ok := firstInt64(values, "limit_window_seconds", "limitWindowSeconds"); ok && seconds > 0 {
		switch seconds {
		case codexFiveHourWindowSeconds:
			return "five_hour"
		case codexWeeklyWindowSeconds:
			return "weekly"
		default:
			return ""
		}
	}
	needle := strings.ToLower(strings.ReplaceAll(path, "-", "_"))
	for _, key := range []string{"window", "name"} {
		if raw, ok := values[key]; ok {
			needle += " " + strings.ToLower(strings.ReplaceAll(fmt.Sprint(raw), "-", "_"))
		}
	}
	switch {
	case strings.Contains(needle, "five_hour"), strings.Contains(needle, "5_hour"), strings.Contains(needle, "5h"), strings.Contains(needle, "primary"):
		return "five_hour"
	case strings.Contains(needle, "weekly"), strings.Contains(needle, "week"), strings.Contains(needle, "seven_day"), strings.Contains(needle, "7_day"), strings.Contains(needle, "7d"), strings.Contains(needle, "secondary"):
		return "weekly"
	default:
		return ""
	}
}

func codexQuotaWindowKnown(window cliproxyauth.QuotaWindow) bool {
	return window.Used != 0 || window.Limit != 0 || window.UsedPercentKnown || window.UsedPercent != 0 || window.LimitWindowSeconds != 0 || !window.NextFreshAt.IsZero() || !window.RefreshedAt.IsZero()
}

func firstInt64(values map[string]any, keys ...string) (int64, bool) {
	for _, key := range keys {
		if raw, ok := values[key]; ok {
			if value, okValue := anyToInt64(raw); okValue {
				return value, true
			}
		}
	}
	return 0, false
}

func firstMap(values map[string]any, keys ...string) (map[string]any, bool) {
	for _, key := range keys {
		if raw, ok := values[key]; ok {
			if value, okValue := raw.(map[string]any); okValue {
				return value, true
			}
		}
	}
	return nil, false
}

func firstFloat64(values map[string]any, keys ...string) (float64, bool) {
	for _, key := range keys {
		if raw, ok := values[key]; ok {
			if value, okValue := anyToFloat64(raw); okValue {
				return value, true
			}
		}
	}
	return 0, false
}

func firstTime(values map[string]any, now time.Time, keys ...string) (time.Time, bool) {
	for _, key := range keys {
		if raw, ok := values[key]; ok {
			if value, okValue := anyToTime(raw, now); okValue {
				return value, true
			}
		}
	}
	return time.Time{}, false
}

func anyToInt64(raw any) (int64, bool) {
	switch value := raw.(type) {
	case float64:
		return int64(value), true
	case string:
		if parsed, ok := anyToFloat64(value); ok {
			return int64(parsed), true
		}
	}
	return 0, false
}

func anyToFloat64(raw any) (float64, bool) {
	switch value := raw.(type) {
	case float64:
		return value, true
	case string:
		trimmed := strings.TrimSpace(strings.TrimSuffix(value, "%"))
		if trimmed == "" {
			return 0, false
		}
		var parsed float64
		if _, err := fmt.Sscanf(trimmed, "%f", &parsed); err != nil {
			return 0, false
		}
		return parsed, true
	default:
		return 0, false
	}
}

func anyToTime(raw any, now time.Time) (time.Time, bool) {
	switch value := raw.(type) {
	case float64:
		if value > 0 {
			return time.Unix(int64(value), 0), true
		}
	case string:
		trimmed := strings.TrimSpace(value)
		if trimmed == "" {
			return time.Time{}, false
		}
		if parsed, err := time.Parse(time.RFC3339Nano, trimmed); err == nil {
			return parsed, true
		}
		var seconds float64
		if _, err := fmt.Sscanf(trimmed, "%f", &seconds); err == nil && seconds > 0 {
			return time.Unix(int64(seconds), 0), true
		}
	case bool:
		if value {
			return now, true
		}
	}
	return time.Time{}, false
}
