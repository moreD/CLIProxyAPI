package executor

import (
	"reflect"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	_ "github.com/router-for-me/CLIProxyAPI/v7/internal/translator"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/tidwall/gjson"
)

func TestParseCodexWorkspaceNames(t *testing.T) {
	t.Parallel()

	payload := []byte(`{
		"items": [
			{"id": "acct-team", "name": " Team Workspace "},
			{"id": "acct-personal", "name": null},
			{"id": "acct-empty", "name": ""},
			{"id": "", "name": "Ignored"}
		]
	}`)
	got, err := parseCodexWorkspaceNames(payload)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"acct-team": "Team Workspace"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parseCodexWorkspaceNames() = %#v, want %#v", got, want)
	}
}

func TestParseCodexWorkspaceNamesRejectsInvalidJSON(t *testing.T) {
	t.Parallel()

	if _, err := parseCodexWorkspaceNames([]byte(`not-json`)); err == nil {
		t.Fatal("parseCodexWorkspaceNames() error = nil, want decode error")
	}
}

func TestParseCodexQuotaInfoWhamUsagePayload(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 5, 18, 12, 0, 0, 0, time.UTC)
	payload := []byte(`{
		"plan_type": "plus",
		"rate_limit": {
			"allowed": true,
			"primary_window": {
				"used_percent": 25,
				"limit_window_seconds": 18000,
				"reset_at": 1779127200
			},
			"secondary_window": {
				"used_percent": "60",
				"limit_window_seconds": 604800,
				"reset_after_seconds": 7200
			}
		},
		"code_review_rate_limit": {
			"primary_window": {
				"used_percent": 99,
				"limit_window_seconds": 18000,
				"reset_at": 1779130800
			},
			"secondary_window": {
				"used_percent": 99,
				"limit_window_seconds": 604800,
				"reset_at": 1779134400
			}
		},
		"rate_limit_reset_credits": {
			"available_count": 3,
			"applicable_available_count": 1
		}
	}`)

	quota, ok := parseCodexQuotaInfo(payload, now)
	if !ok || quota == nil {
		t.Fatalf("parseCodexQuotaInfo() = (%#v, %v), want quota", quota, ok)
	}
	if got := quota.FiveHour.UsedPercent; got != 25 {
		t.Fatalf("five-hour used percent = %v, want 25", got)
	}
	if got := quota.Weekly.UsedPercent; got != 60 {
		t.Fatalf("weekly used percent = %v, want 60", got)
	}
	if got, want := quota.FiveHour.NextFreshAt, time.Unix(1779127200, 0); !got.Equal(want) {
		t.Fatalf("five-hour next fresh = %v, want %v", got, want)
	}
	if got, want := quota.Weekly.NextFreshAt, now.Add(2*time.Hour); !got.Equal(want) {
		t.Fatalf("weekly next fresh = %v, want %v", got, want)
	}
	if quota.RateLimitResetCredits == nil {
		t.Fatal("rate limit reset credits are nil")
	}
	if got := quota.RateLimitResetCredits.AvailableCount; got != 3 {
		t.Fatalf("available reset credits = %d, want 3", got)
	}
	if got := quota.RateLimitResetCredits.ApplicableAvailableCount; got != 1 {
		t.Fatalf("applicable reset credits = %d, want 1", got)
	}
}

func TestParseCodexQuotaInfoCanonicalUsedPercent(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 5, 18, 12, 0, 0, 0, time.UTC)
	payload := []byte(`{
		"rate_limit": {
			"primary_window": {
				"used_percent": 80,
				"limit_window_seconds": 18000
			},
			"secondary_window": {
				"used_percent": 60,
				"limit_window_seconds": 604800
			}
		}
	}`)

	quota, ok := parseCodexQuotaInfo(payload, now)
	if !ok || quota == nil {
		t.Fatalf("parseCodexQuotaInfo() = (%#v, %v), want quota", quota, ok)
	}
	if got := quota.FiveHour.UsedPercent; got != 80 {
		t.Fatalf("five-hour used percent = %v, want 80", got)
	}
	if got := quota.Weekly.UsedPercent; got != 60 {
		t.Fatalf("weekly used percent = %v, want 60", got)
	}
}

func TestParseCodexQuotaInfoClassifiesWeeklyOnlyPrimaryByDuration(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	payload := []byte(`{
		"plan_type": "team",
		"rate_limit": {
			"primary_window": {
				"used_percent": 42,
				"limit_window_seconds": 604800,
				"reset_after_seconds": 3600
			}
		}
	}`)

	quota, ok := parseCodexQuotaInfo(payload, now)
	if !ok || quota == nil {
		t.Fatalf("parseCodexQuotaInfo() = (%#v, %v), want quota", quota, ok)
	}
	if codexQuotaWindowKnown(quota.FiveHour) {
		t.Fatalf("five-hour quota = %#v, want absent", quota.FiveHour)
	}
	if got := quota.Weekly.UsedPercent; got != 42 {
		t.Fatalf("weekly used percent = %v, want 42", got)
	}
	if got := quota.Weekly.LimitWindowSeconds; got != 604800 {
		t.Fatalf("weekly limit window seconds = %d, want 604800", got)
	}
}

func TestParseCodexQuotaInfoComputesUsedPercentFromUsedAndLimit(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 5, 18, 12, 0, 0, 0, time.UTC)
	payload := []byte(`{
		"rate_limit": {
			"primary_window": {
				"used": 900,
				"limit": 1000,
				"limit_window_seconds": 18000
			}
		}
	}`)

	quota, ok := parseCodexQuotaInfo(payload, now)
	if !ok || quota == nil {
		t.Fatalf("parseCodexQuotaInfo() = (%#v, %v), want quota", quota, ok)
	}
	if !quota.FiveHour.UsedPercentKnown {
		t.Fatal("expected five-hour used percent to be known")
	}
	if got := quota.FiveHour.UsedPercent; got != 90 {
		t.Fatalf("five-hour used percent = %v, want 90", got)
	}
}

func TestCodexQuotaProbePayloadUsesCodexTranslatorShape(t *testing.T) {
	t.Parallel()

	payload := codexQuotaProbePayload("gpt-5.5")
	root := gjson.ParseBytes(payload)
	if got := root.Get("model").String(); got != "gpt-5.5" {
		t.Fatalf("model = %q, want gpt-5.5; payload=%s", got, string(payload))
	}
	if got := root.Get("instructions").String(); got == "" {
		t.Fatalf("instructions missing; payload=%s", string(payload))
	}
	if !root.Get("stream").Bool() {
		t.Fatalf("stream = false, want true from Codex translator; payload=%s", string(payload))
	}
	if root.Get("store").Bool() {
		t.Fatalf("store = true, want false from Codex translator; payload=%s", string(payload))
	}
	if got := root.Get("input.0.type").String(); got != "message" {
		t.Fatalf("input.0.type = %q, want message; payload=%s", got, string(payload))
	}
	if got := root.Get("input.0.role").String(); got != "user" {
		t.Fatalf("input.0.role = %q, want user; payload=%s", got, string(payload))
	}
	if got := root.Get("input.0.content.0.type").String(); got != "input_text" {
		t.Fatalf("input.0.content.0.type = %q, want input_text; payload=%s", got, string(payload))
	}
	if got := root.Get("input.0.content.0.text").String(); got != "hello" {
		t.Fatalf("input.0.content.0.text = %q, want hello; payload=%s", got, string(payload))
	}
}

func TestCodexQuotaProbeModelPrefersGPT55(t *testing.T) {
	t.Parallel()

	models := []*registry.ModelInfo{
		{ID: "gpt-5.4"},
		{ID: "gpt-5.5"},
	}
	clientID := "quota-probe-test-client"
	registry.GetGlobalRegistry().RegisterClient(clientID, "codex", models)
	t.Cleanup(func() {
		registry.GetGlobalRegistry().UnregisterClient(clientID)
	})

	if got := codexQuotaProbeModel(&cliproxyauth.Auth{ID: clientID}); got != "gpt-5.5" {
		t.Fatalf("probe model = %q, want gpt-5.5", got)
	}
}
