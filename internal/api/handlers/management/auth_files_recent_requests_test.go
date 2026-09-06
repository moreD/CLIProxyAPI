package management

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestListAuthFiles_IncludesRuntimeState(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")

	now := time.Date(2026, 6, 3, 12, 0, 0, 0, time.UTC)
	nextFresh := now.Add(2 * time.Hour)
	manager := coreauth.NewManager(nil, nil, nil)
	record := &coreauth.Auth{
		ID:                      "runtime-only-auth-1",
		Provider:                "codex",
		LastQuotaSeenAt:         now,
		QuotaRefreshError:       "codex quota refresh: status 401: invalidated",
		QuotaRefreshErrorStatus: http.StatusUnauthorized,
		Attributes: map[string]string{
			"runtime_only": "true",
			"account_type": "oauth",
		},
		Metadata: map[string]any{
			"type":                         "codex",
			coreauth.MetadataWorkspaceName: "Team Workspace",
		},
		Quota: coreauth.QuotaState{
			ObservedAt: time.Date(2026, 8, 22, 0, 0, 0, 0, time.UTC),
			Signals: map[string]string{
				"X-Codex-Primary-Used-Percent": "58",
			},
		},
		ModelStates: map[string]*coreauth.ModelState{
			"gpt-5": {
				Quota: coreauth.QuotaState{
					ObservedAt: time.Date(2026, 8, 22, 0, 1, 0, 0, time.UTC),
					Signals: map[string]string{
						"Retry-After": "120",
					},
				},
			},
		},
		RuntimeQuota: &coreauth.QuotaInfo{
			FiveHour: coreauth.QuotaWindow{
				UsedPercent:      75,
				UsedPercentKnown: true,
				NextFreshAt:      nextFresh,
				RefreshedAt:      now,
			},
			Weekly: coreauth.QuotaWindow{
				UsedPercent:      40,
				UsedPercentKnown: true,
				NextFreshAt:      now.Add(24 * time.Hour),
				RefreshedAt:      now,
			},
			RateLimitResetCredits: &coreauth.QuotaResetCredits{
				AvailableCount:           3,
				ApplicableAvailableCount: 1,
			},
		},
	}
	if _, errRegister := manager.Register(context.Background(), record); errRegister != nil {
		t.Fatalf("failed to register auth record: %v", errRegister)
	}

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, manager)
	h.tokenStore = &memoryAuthStore{}

	rec := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodGet, "/v0/management/auth-files", nil)
	ginCtx.Request = req

	h.ListAuthFiles(ginCtx)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected list status %d, got %d with body %s", http.StatusOK, rec.Code, rec.Body.String())
	}

	var payload map[string]any
	if errUnmarshal := json.Unmarshal(rec.Body.Bytes(), &payload); errUnmarshal != nil {
		t.Fatalf("failed to decode list payload: %v", errUnmarshal)
	}
	filesRaw, ok := payload["files"].([]any)
	if !ok {
		t.Fatalf("expected files array, payload: %#v", payload)
	}
	if len(filesRaw) != 1 {
		t.Fatalf("expected 1 auth entry, got %d", len(filesRaw))
	}

	fileEntry, ok := filesRaw[0].(map[string]any)
	if !ok {
		t.Fatalf("expected file entry object, got %#v", filesRaw[0])
	}

	if _, ok := fileEntry["success"].(float64); !ok {
		t.Fatalf("expected success number, got %#v", fileEntry["success"])
	}
	if _, ok := fileEntry["failed"].(float64); !ok {
		t.Fatalf("expected failed number, got %#v", fileEntry["failed"])
	}

	quota, ok := fileEntry["quota"].(map[string]any)
	if !ok {
		t.Fatalf("expected quota observation object, got %#v", fileEntry["quota"])
	}
	if _, ok := quota["observed_at"].(string); !ok {
		t.Fatalf("expected quota observed_at string, got %#v", quota["observed_at"])
	}
	quotaSignals, ok := quota["signals"].(map[string]any)
	if !ok || quotaSignals["X-Codex-Primary-Used-Percent"] != "58" {
		t.Fatalf("expected auth quota signals, got %#v", quota["signals"])
	}

	modelQuotas, ok := fileEntry["model_quotas"].(map[string]any)
	if !ok {
		t.Fatalf("expected model_quotas object, got %#v", fileEntry["model_quotas"])
	}
	modelQuota, ok := modelQuotas["gpt-5"].(map[string]any)
	if !ok {
		t.Fatalf("expected gpt-5 quota observation, got %#v", modelQuotas["gpt-5"])
	}
	modelSignals, ok := modelQuota["signals"].(map[string]any)
	if !ok || modelSignals["Retry-After"] != "120" {
		t.Fatalf("expected model quota signals, got %#v", modelQuota["signals"])
	}

	recentRaw, ok := fileEntry["recent_requests"].([]any)
	if !ok {
		t.Fatalf("expected recent_requests array, got %#v", fileEntry["recent_requests"])
	}
	if len(recentRaw) != 20 {
		t.Fatalf("expected 20 recent_requests buckets, got %d", len(recentRaw))
	}
	for idx, item := range recentRaw {
		bucket, ok := item.(map[string]any)
		if !ok {
			t.Fatalf("expected bucket object at %d, got %#v", idx, item)
		}
		if _, ok := bucket["time"].(string); !ok {
			t.Fatalf("expected bucket time string at %d, got %#v", idx, bucket["time"])
		}
		if _, ok := bucket["success"].(float64); !ok {
			t.Fatalf("expected bucket success number at %d, got %#v", idx, bucket["success"])
		}
		if _, ok := bucket["failed"].(float64); !ok {
			t.Fatalf("expected bucket failed number at %d, got %#v", idx, bucket["failed"])
		}
	}
	if got := fileEntry["last_quota_seen_at"]; got == nil {
		t.Fatalf("expected last_quota_seen_at in file entry")
	}
	if got := fileEntry["quota_refresh_error"]; got != "codex quota refresh: status 401: invalidated" {
		t.Fatalf("expected quota refresh error, got %#v", got)
	}
	if got := fileEntry["quota_refresh_error_status"]; got != float64(http.StatusUnauthorized) {
		t.Fatalf("expected quota refresh error status 401, got %#v", got)
	}
	if got := fileEntry[coreauth.MetadataWorkspaceName]; got != "Team Workspace" {
		t.Fatalf("workspace_name = %#v, want Team Workspace", got)
	}
	runtimeQuota, ok := fileEntry["runtime_quota"].(map[string]any)
	if !ok {
		t.Fatalf("expected runtime_quota object, got %#v", fileEntry["runtime_quota"])
	}
	fiveHour, ok := runtimeQuota["five_hour"].(map[string]any)
	if !ok {
		t.Fatalf("expected five_hour object, got %#v", runtimeQuota["five_hour"])
	}
	if got := fiveHour["used_percent"]; got != float64(75) {
		t.Fatalf("expected five_hour used_percent 75, got %#v", got)
	}
	if got := fiveHour["next_fresh_at"]; got == nil {
		t.Fatalf("expected five_hour next_fresh_at")
	}
	resetCredits, ok := runtimeQuota["rate_limit_reset_credits"].(map[string]any)
	if !ok {
		t.Fatalf("expected rate_limit_reset_credits object, got %#v", runtimeQuota["rate_limit_reset_credits"])
	}
	if got := resetCredits["available_count"]; got != float64(3) {
		t.Fatalf("expected available_count 3, got %#v", got)
	}
	if got := resetCredits["applicable_available_count"]; got != float64(1) {
		t.Fatalf("expected applicable_available_count 1, got %#v", got)
	}
}

func TestListAuthFiles_SurfacesUnauthorizedAuthAsQuotaError(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")

	now := time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC)
	manager := coreauth.NewManager(nil, nil, nil)
	record := &coreauth.Auth{
		ID:            "runtime-only-auth-1",
		Provider:      "codex",
		Status:        coreauth.StatusError,
		StatusMessage: "authentication token has been invalidated",
		Unavailable:   true,
		LastError: &coreauth.Error{
			Code:       "unauthorized",
			Message:    "authentication token has been invalidated",
			HTTPStatus: http.StatusUnauthorized,
		},
		Attributes: map[string]string{
			"runtime_only": "true",
			"account_type": "oauth",
		},
		Metadata: map[string]any{"type": "codex"},
		RuntimeQuota: &coreauth.QuotaInfo{
			Weekly: coreauth.QuotaWindow{
				UsedPercent:      40,
				UsedPercentKnown: true,
				NextFreshAt:      now.Add(24 * time.Hour),
				RefreshedAt:      now,
			},
		},
	}
	if _, errRegister := manager.Register(context.Background(), record); errRegister != nil {
		t.Fatalf("failed to register auth record: %v", errRegister)
	}

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, manager)
	h.tokenStore = &memoryAuthStore{}

	rec := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(rec)
	ginCtx.Request = httptest.NewRequest(http.MethodGet, "/v0/management/auth-files", nil)
	h.ListAuthFiles(ginCtx)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected list status %d, got %d with body %s", http.StatusOK, rec.Code, rec.Body.String())
	}
	var payload struct {
		Files []map[string]any `json:"files"`
	}
	if errUnmarshal := json.Unmarshal(rec.Body.Bytes(), &payload); errUnmarshal != nil {
		t.Fatalf("failed to decode list payload: %v", errUnmarshal)
	}
	if len(payload.Files) != 1 {
		t.Fatalf("expected 1 auth entry, got %d", len(payload.Files))
	}
	if got := payload.Files[0]["quota_refresh_error"]; got != "authentication token has been invalidated" {
		t.Fatalf("quota refresh error = %#v, want auth failure", got)
	}
	if got := payload.Files[0]["quota_refresh_error_status"]; got != float64(http.StatusUnauthorized) {
		t.Fatalf("quota refresh error status = %#v, want 401", got)
	}
	if _, ok := payload.Files[0]["runtime_quota"]; !ok {
		t.Fatal("expected cached runtime quota to remain available for diagnostics")
	}
}
