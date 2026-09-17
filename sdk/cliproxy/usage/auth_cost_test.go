package usage

import (
	"context"
	"net/http"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/authusage"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/billing"
)

func TestAuthRecordCost(t *testing.T) {
	tests := []struct {
		name   string
		record Record
		want   billing.USD
	}{
		{"codex cache and reasoning subsets", Record{Provider: "codex", Model: "gpt-5.6-sol", Detail: Detail{InputTokens: 100, OutputTokens: 20, ReasoningTokens: 10, CacheReadTokens: 50}}, billing.Price("gpt-5.6-sol", "default", billing.Tokens{Input: 100, Output: 20, CacheRead: 50}, true)},
		{"claude independent buckets", Record{Provider: "claude", Model: "claude-sonnet-4", Detail: Detail{InputTokens: 100, OutputTokens: 20, ReasoningTokens: 10, CacheReadTokens: 50, CacheCreationTokens: 30}}, billing.Price("claude-sonnet-4", "default", billing.Tokens{Input: 180, Output: 30, CacheRead: 50, CacheWrite: 30}, true)},
		{"gemini separate reasoning", Record{Provider: "gemini", Model: "gemini-2.5-pro", Detail: Detail{InputTokens: 100, OutputTokens: 20, ReasoningTokens: 10, CacheReadTokens: 50}}, billing.Price("gemini-2.5-pro", "default", billing.Tokens{Input: 100, Output: 30, CacheRead: 50}, true)},
		{"response tier and long context", Record{Provider: "codex", Model: "gpt-6-astra", ServiceTier: "flex", ResponseServiceTier: "priority", Detail: Detail{InputTokens: 300000, OutputTokens: 100}}, billing.Price("gpt-6-astra", "priority", billing.Tokens{Input: 300000, Output: 100}, true)},
		{"unknown model and total only", Record{Provider: "custom", Model: "new-model", Detail: Detail{TotalTokens: 100}}, billing.Price("new-model", "default", billing.Tokens{Input: 100}, true)},
		{"zero tokens", Record{Provider: "codex", Model: "gpt-6-astra", Failed: true}, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := authRecordCost(context.Background(), tt.record); got != tt.want {
				t.Fatalf("cost = %v, want %v", got, tt.want)
			}
		})
	}
	if got := authRecordCost(WithServiceTier(context.Background(), "priority"), Record{Provider: "codex", Model: "gpt-6-astra", Detail: Detail{InputTokens: 100}}); got != billing.Price("gpt-6-astra", "priority", billing.Tokens{Input: 100}, true) {
		t.Fatalf("context tier lost: %v", got)
	}
}

func TestPublishAccountsSynchronouslyWithoutPlugins(t *testing.T) {
	s, err := authusage.NewStore(filepath.Join(t.TempDir(), "auth.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	restore := authusage.SetDefault(s)
	t.Cleanup(func() { restore(); _ = s.Close() })
	now := time.Now()
	cost := billing.Price("gpt-5.6-sol", "default", billing.Tokens{Input: 100}, true)
	if _, err = s.SetLimit("a", cost, now); err != nil {
		t.Fatal(err)
	}
	m := NewManager(1)
	t.Cleanup(m.Stop)
	m.Publish(context.Background(), Record{AuthIndex: "a", Provider: "codex", Model: "gpt-5.6-sol", Failed: true, RequestedAt: now, Detail: Detail{InputTokens: 100}})
	if state := s.Snapshot("a", now); !state.Exceeded || state.CostUSD != cost {
		t.Fatalf("Publish returned before durable accounting: %+v", state)
	}
	if state := s.Snapshot("b", now); state.CostUSD != 0 || state.Exceeded {
		t.Fatalf("other auth billed: %+v", state)
	}
}

func TestAuthWeeklyResetHeaderScope(t *testing.T) {
	reset := time.Date(2026, 9, 19, 5, 37, 0, 0, time.UTC)
	value := strconv.FormatInt(reset.Unix(), 10)
	for _, tt := range []struct {
		provider string
		headers  http.Header
		known    bool
	}{
		{"claude", http.Header{"anthropic-ratelimit-unified-7d-reset": {value}}, true},
		{"codex", http.Header{"x-codex-secondary-window-minutes": {"10080"}, "x-codex-secondary-reset-at": {value}}, true},
		{"codex", http.Header{"X-Codex-Primary-Window-Minutes": {"10080"}, "X-Codex-Primary-Reset-After-Seconds": {"3600"}, "Date": {reset.Add(-time.Hour).Format(http.TimeFormat)}}, true},
		{"codex", http.Header{"X-Codex-Primary-Window-Minutes": {"10080"}, "X-Codex-Primary-Reset-After-Seconds": {"3600"}}, false},
		{"codex", http.Header{"X-Codex-Primary-Window-Minutes": {"300"}, "X-Codex-Primary-Reset-At": {value}}, false},
		{"custom", http.Header{"anthropic-ratelimit-unified-7d-reset": {value}}, false},
		{"claude", http.Header{"Retry-After": {"100"}}, false},
	} {
		got := authWeeklyReset(tt.provider, tt.headers)
		if tt.known && !got.Equal(reset) || !tt.known && !got.IsZero() {
			t.Fatalf("%s: reset=%v known=%v", tt.provider, got, tt.known)
		}
	}
}

func TestAuthWeeklyQuotaPairsUtilizationWithItsWindow(t *testing.T) {
	reset := time.Date(2026, 9, 19, 5, 37, 0, 0, time.UTC)
	epoch := strconv.FormatInt(reset.Unix(), 10)
	for _, tc := range []struct {
		name, provider string
		headers        http.Header
		percent        float64
		known          bool
	}{
		{"weekly secondary", "codex", http.Header{"X-Codex-Primary-Window-Minutes": {"300"}, "X-Codex-Primary-Used-Percent": {"0"}, "X-Codex-Primary-Reset-At": {epoch}, "X-Codex-Secondary-Window-Minutes": {"10080"}, "X-Codex-Secondary-Used-Percent": {"80"}, "X-Codex-Secondary-Reset-At": {epoch}}, 80, true},
		{"unknown weekly percentage", "codex", http.Header{"X-Codex-Primary-Window-Minutes": {"300"}, "X-Codex-Primary-Used-Percent": {"0"}, "X-Codex-Secondary-Window-Minutes": {"10080"}, "X-Codex-Secondary-Reset-At": {epoch}}, 0, false},
		{"known weekly zero", "codex", http.Header{"X-Codex-Primary-Window-Minutes": {"10080"}, "X-Codex-Primary-Used-Percent": {"0"}, "X-Codex-Primary-Reset-At": {epoch}}, 0, true},
		{"claude fraction", "claude", http.Header{"Anthropic-Ratelimit-Unified-7d-Reset": {epoch}, "Anthropic-Ratelimit-Unified-7d-Utilization": {"0.5"}}, 50, true},
		{"claude missing fraction", "claude", http.Header{"Anthropic-Ratelimit-Unified-7d-Reset": {epoch}, "Anthropic-Ratelimit-Unified-5h-Utilization": {"0"}}, 0, false},
		{"feature window excluded", "codex", http.Header{"X-Codex-Additional-Spark-Primary-Window-Minutes": {"10080"}, "X-Codex-Additional-Spark-Primary-Used-Percent": {"0"}, "X-Codex-Additional-Spark-Primary-Reset-At": {epoch}}, 0, false},
		{"unknown provider", "custom", http.Header{"Anthropic-Ratelimit-Unified-7d-Reset": {epoch}, "Anthropic-Ratelimit-Unified-7d-Utilization": {"0"}}, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, percent, known := authWeeklyQuota(tc.provider, tc.headers)
			if percent != tc.percent || known != tc.known {
				t.Fatalf("percent=%v known=%v, want %v %v", percent, known, tc.percent, tc.known)
			}
		})
	}
	for _, raw := range []string{"", "NaN", "Inf", "-0.1", "1.01"} {
		if _, known := authHeaderUtilization(raw, 1); known {
			t.Errorf("invalid utilization %q considered known", raw)
		}
	}
}

func TestAuthHeaderEarlyResetPreservesNewCycleCosts(t *testing.T) {
	for _, provider := range []string{"codex", "claude"} {
		for _, known := range []bool{true, false} {
			t.Run(provider+"/known="+strconv.FormatBool(known), func(t *testing.T) {
				store, err := authusage.NewStore(filepath.Join(t.TempDir(), "auth.sqlite"))
				if err != nil {
					t.Fatal(err)
				}
				restore := authusage.SetDefault(store)
				t.Cleanup(func() { restore(); _ = store.Close() })
				now := time.Now().UTC().Truncate(time.Second)
				previous := now.Add(-2 * time.Hour)
				start := now.Add(-time.Hour)
				headers := func(reset, observed time.Time, percent string) http.Header {
					h := http.Header{"Date": {observed.Format(http.TimeFormat)}}
					if provider == "codex" {
						h.Set("X-Codex-Secondary-Window-Minutes", "10080")
						h.Set("X-Codex-Secondary-Reset-At", strconv.FormatInt(reset.Unix(), 10))
						if percent != "" {
							h.Set("X-Codex-Secondary-Used-Percent", percent)
						}
					} else {
						h.Set("Anthropic-Ratelimit-Unified-7d-Reset", strconv.FormatInt(reset.Unix(), 10))
						if percent != "" {
							h.Set("Anthropic-Ratelimit-Unified-7d-Utilization", percent)
						}
					}
					return h
				}
				oldPercent := "80"
				if provider == "claude" {
					oldPercent = "0.8"
				}
				oldHeaders := headers(now.Add(48*time.Hour), previous, oldPercent)
				RecordAuthUsage(context.Background(), Record{AuthIndex: "a", Provider: provider, ResponseHeaders: oldHeaders})
				if _, err = store.SetLimit("a", 100, now); err != nil {
					t.Fatal(err)
				}
				if err = store.RecordAt("a", 100, previous.Add(time.Minute), now); err != nil {
					t.Fatal(err)
				}
				if err = store.RecordAt("a", 10, start.Add(time.Minute), now); err != nil {
					t.Fatal(err)
				}
				percent := ""
				if known {
					percent = "0"
				}
				newHeaders := headers(start.Add(7*24*time.Hour), now, percent)
				RecordAuthUsage(context.Background(), Record{AuthIndex: "a", Provider: provider, ResponseHeaders: newHeaders})
				state := store.Snapshot("a", now)
				want := billing.USD(110)
				if known {
					want = 10
				}
				if state.CostUSD != want || state.TotalCostUSD != 110 || state.Exceeded == known {
					t.Fatalf("known=%v state=%+v", known, state)
				}
				if err = store.RecordAt("a", 90, now, now); err != nil {
					t.Fatal(err)
				}
				oldStreamHeaders := oldHeaders.Clone()
				oldStreamHeaders.Del("Date")
				for _, h := range []http.Header{newHeaders, oldHeaders, oldStreamHeaders} {
					RecordAuthUsage(context.Background(), Record{AuthIndex: "a", Provider: provider, RequestedAt: previous, ResponseHeaders: h})
					if state = store.Snapshot("a", now); state.CostUSD != want+90 || !state.Exceeded || !state.Window.End.Equal(start.Add(7*24*time.Hour)) {
						t.Fatalf("stale or duplicate headers cleared costs: %+v", state)
					}
				}
			})
		}
	}
}
