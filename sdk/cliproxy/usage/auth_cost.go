package usage

import (
	"context"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/authusage"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/billing"
	log "github.com/sirupsen/logrus"
)

// RecordAuthUsage synchronously accounts credential costs without publishing a
// client usage event. Internal billable probes use this path; ordinary requests
// are accounted automatically by Manager.Publish. Never call both for one request.
func RecordAuthUsage(ctx context.Context, record Record) {
	if strings.TrimSpace(record.AuthIndex) == "" {
		return
	}
	now := time.Now()
	// Without a response Date, request start is a conservative observation time.
	// Stream completion can be much later and must not make old headers newer
	// than a quota refresh that already confirmed the next upstream cycle.
	observedAt := record.RequestedAt
	if observedAt.IsZero() {
		observedAt = now
	}
	if date, err := http.ParseTime(authHeader(record.ResponseHeaders, "Date")); err == nil {
		observedAt = date
	}
	if reset, used, known := authWeeklyQuota(record.Provider, record.ResponseHeaders); !reset.IsZero() {
		if err := authusage.ObserveWeeklyQuota(record.AuthIndex, reset, observedAt, now, used, known); err != nil {
			log.Errorf("auth quota reset persistence failed: %v", err)
		}
	}
	if err := authusage.Record(record.AuthIndex, authRecordCost(ctx, record), record.RequestedAt); err != nil {
		log.Errorf("auth dollar accounting failed; credential routing is blocked: %v", err)
	}
}

func authRecordCost(ctx context.Context, record Record) billing.USD {
	detail := EnsureTokenBreakdownForProvider(record.Detail, record.Provider, record.ExecutorType)
	var tokens billing.Tokens
	if b := detail.TokenBreakdown; b.Valid() && b.UnclassifiedTokens == 0 {
		tokens = billing.Tokens{Input: b.Input.TotalTokens, Output: b.Output.TotalTokens, CacheRead: b.Input.CacheReadTokens, CacheWrite: b.Input.CacheWriteTokens}
	} else {
		// Match client billing's fallback for legacy, incomplete token records.
		tokens = billing.Tokens{Input: max(detail.InputTokens, 0), Output: max(detail.OutputTokens, 0), CacheRead: max(detail.CacheReadTokens, detail.CachedTokens, 0), CacheWrite: max(detail.CacheCreationTokens, 0)}
		provider := strings.ToLower(record.Provider)
		switch {
		case strings.Contains(provider, "claude") || strings.Contains(provider, "anthropic"):
			tokens.Input = addAuthTokens(tokens.Input, tokens.CacheRead, tokens.CacheWrite)
			tokens.Output = addAuthTokens(tokens.Output, detail.ReasoningTokens)
		case strings.Contains(provider, "gemini") || strings.Contains(provider, "vertex") || strings.Contains(provider, "antigravity") || strings.Contains(provider, "aistudio"):
			tokens.Output = addAuthTokens(tokens.Output, detail.ReasoningTokens)
		default:
			tokens.Output = max(tokens.Output, detail.ReasoningTokens)
		}
		if tokens.Input == 0 && tokens.Output == 0 {
			tokens.Input = max(detail.TotalTokens, detail.TokenBreakdown.TotalTokens, 0)
		}
		tokens.Input = max(tokens.Input, addAuthTokens(tokens.CacheRead, tokens.CacheWrite))
	}
	tier := strings.TrimSpace(record.ServiceTier)
	if tier == "" {
		tier = strings.TrimSpace(record.RequestServiceTier)
	}
	if tier == "" {
		tier = ServiceTierFromContext(ctx)
	}
	responseTier := strings.TrimSpace(record.ResponseServiceTier)
	if responseTier == "" {
		responseTier = strings.TrimSpace(detail.ResponseServiceTier)
	}
	if responseTier != "" && !strings.EqualFold(responseTier, "auto") {
		tier = responseTier
	}
	return billing.Price(record.Model, tier, tokens, true)
}

func addAuthTokens(values ...int64) int64 {
	var result int64
	for _, value := range values {
		value = max(value, 0)
		if result > (1<<63-1)-value {
			return 1<<63 - 1
		}
		result += value
	}
	return result
}

func authWeeklyReset(provider string, headers http.Header) time.Time {
	reset, _, _ := authWeeklyQuota(provider, headers)
	return reset
}

// authWeeklyQuota reads the reset and utilization from the same provider window.
// A short or feature-specific window cannot establish an account-wide reset.
func authWeeklyQuota(provider string, headers http.Header) (time.Time, float64, bool) {
	provider = strings.ToLower(strings.TrimSpace(provider))
	switch provider {
	case "claude", "anthropic":
		reset := authResetTime(authHeader(headers, "Anthropic-Ratelimit-Unified-7d-Reset"))
		used, known := authHeaderUtilization(authHeader(headers, "Anthropic-Ratelimit-Unified-7d-Utilization"), 1)
		return reset, used, known
	case "codex":
		for _, window := range []string{"primary", "secondary"} {
			if strings.TrimSpace(authHeader(headers, "X-Codex-"+window+"-Window-Minutes")) == "10080" {
				used, known := authHeaderUtilization(authHeader(headers, "X-Codex-"+window+"-Used-Percent"), 100)
				if reset := authResetTime(authHeader(headers, "X-Codex-"+window+"-Reset-At")); !reset.IsZero() {
					return reset, used, known
				}
				// Relative headers describe the response's receipt time, not the
				// eventual completion of a potentially long running stream.
				date, errDate := http.ParseTime(authHeader(headers, "Date"))
				after, errAfter := strconv.ParseInt(strings.TrimSpace(authHeader(headers, "X-Codex-"+window+"-Reset-After-Seconds")), 10, 64)
				if errDate == nil && errAfter == nil && after > 0 && after <= 7*24*60*60 {
					return date.Add(time.Duration(after) * time.Second).UTC(), used, known
				}
			}
		}
	}
	return time.Time{}, 0, false
}

func authResetTime(raw string) time.Time {
	seconds, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil || seconds <= 0 {
		return time.Time{}
	}
	return time.Unix(seconds, 0).UTC()
}

func authHeaderUtilization(raw string, scale float64) (float64, bool) {
	used, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
	if err != nil || math.IsNaN(used) || math.IsInf(used, 0) || used < 0 || used > scale {
		return 0, false
	}
	return used / scale * 100, true
}

func authHeader(headers http.Header, name string) string {
	if value := headers.Get(name); value != "" {
		return value
	}
	for key, values := range headers {
		if strings.EqualFold(key, name) && len(values) > 0 {
			return values[0]
		}
	}
	return ""
}
