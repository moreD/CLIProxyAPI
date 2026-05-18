package executor

import (
	"testing"
	"time"

	_ "github.com/router-for-me/CLIProxyAPI/v7/internal/translator"
	"github.com/tidwall/gjson"
)

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
		}
	}`)

	quota, ok := parseCodexQuotaInfo(payload, now)
	if !ok || quota == nil {
		t.Fatalf("parseCodexQuotaInfo() = (%#v, %v), want quota", quota, ok)
	}
	if got := quota.FiveHour.UsagePercent; got != 75 {
		t.Fatalf("five-hour remaining percent = %v, want 75", got)
	}
	if got := quota.Weekly.UsagePercent; got != 40 {
		t.Fatalf("weekly remaining percent = %v, want 40", got)
	}
	if got, want := quota.FiveHour.NextFreshAt, time.Unix(1779127200, 0); !got.Equal(want) {
		t.Fatalf("five-hour next fresh = %v, want %v", got, want)
	}
	if got, want := quota.Weekly.NextFreshAt, now.Add(2*time.Hour); !got.Equal(want) {
		t.Fatalf("weekly next fresh = %v, want %v", got, want)
	}
}

func TestCodexQuotaProbePayloadUsesCodexTranslatorShape(t *testing.T) {
	t.Parallel()

	payload := codexQuotaProbePayload("gpt-5.3-codex")
	root := gjson.ParseBytes(payload)
	if got := root.Get("model").String(); got != "gpt-5.3-codex" {
		t.Fatalf("model = %q, want gpt-5.3-codex; payload=%s", got, string(payload))
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
