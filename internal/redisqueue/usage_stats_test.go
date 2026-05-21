package redisqueue

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestUsageStatsPersistAndRecoverEvents(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 5, 21, 1, 0, 0, 0, time.UTC)

	store := &usageStatsStore{
		events: []usageStatsEvent{
			{
				Timestamp: now.Add(-time.Minute),
				APIKey:    "client-key",
				Provider:  "codex",
				Model:     "gpt-5.3-codex",
				Alias:     "codex",
				Endpoint:  "POST /v1/responses",
				LatencyMs: 120,
				Tokens: usageStatTokens{
					ReadTokens:      100,
					WriteTokens:     20,
					CacheReadTokens: 60,
					TotalTokens:     120,
				},
			},
		},
	}

	prevDir := usageStatsPersistenceDir
	usageStatsPersistenceDir = dir
	t.Cleanup(func() { usageStatsPersistenceDir = prevDir })

	if err := store.persistToDisk(now); err != nil {
		t.Fatalf("persistToDisk() error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "client-key")); err != nil {
		t.Fatalf("expected stats file for api key: %v", err)
	}

	recovered := &usageStatsStore{}
	if err := recovered.recoverFromDisk(now); err != nil {
		t.Fatalf("recoverFromDisk() error = %v", err)
	}
	snapshot := recovered.snapshot(now)
	if len(snapshot.APIKeys) != 1 {
		t.Fatalf("api keys = %d, want 1", len(snapshot.APIKeys))
	}
	got := snapshot.APIKeys[0]
	if got.APIKey != "client-key" {
		t.Fatalf("api key = %q, want client-key", got.APIKey)
	}
	if got.SevenDay.Tokens.ReadTokens != 100 || got.SevenDay.Tokens.CacheReadTokens != 60 || got.SevenDay.Tokens.WriteTokens != 20 {
		t.Fatalf("tokens = %+v, want read/cache-read/write 100/60/20", got.SevenDay.Tokens)
	}
	if got.SevenDay.Tokens.TotalTokens != 1660 {
		t.Fatalf("weighted total = %d, want 1660", got.SevenDay.Tokens.TotalTokens)
	}
}

func TestUsageStatsFixedWindowsAndLimitDecision(t *testing.T) {
	now := time.Date(2026, 5, 21, 13, 0, 0, 0, time.UTC)
	store := &usageStatsStore{}
	store.add(usageStatsEvent{
		Timestamp:         now.Add(-30 * time.Minute),
		APIKey:            "client-key",
		SessionAffinityID: "session-a",
		Provider:          "codex",
		Model:             "gpt-5.3-codex",
		Tokens:            usageStatTokens{TotalTokens: 80, ReadTokens: 80},
	}, now)
	store.add(usageStatsEvent{
		Timestamp:         now.Add(-3 * time.Hour),
		APIKey:            "client-key",
		SessionAffinityID: "session-b",
		Provider:          "codex",
		Model:             "gpt-5.3-codex",
		Tokens:            usageStatTokens{TotalTokens: 30, ReadTokens: 30},
	}, now)

	snapshot := store.snapshot(now)
	if got := snapshot.Windows.TwelveHour.Start; !got.Equal(time.Date(2026, 5, 21, 12, 0, 0, 0, time.UTC)) {
		t.Fatalf("12h start = %s, want 2026-05-21T12:00:00Z", got)
	}
	if got := snapshot.Windows.SevenDay.Start; !got.Equal(time.Date(2026, 5, 17, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("7d start = %s, want 2026-05-17T00:00:00Z", got)
	}
	if len(snapshot.APIKeys) != 1 {
		t.Fatalf("api keys = %d, want 1", len(snapshot.APIKeys))
	}
	if got := snapshot.APIKeys[0].TwelveHour.Tokens.TotalTokens; got != 800 {
		t.Fatalf("12h weighted total = %d, want 800", got)
	}
	if got := snapshot.APIKeys[0].SevenDay.Tokens.TotalTokens; got != 1100 {
		t.Fatalf("7d weighted total = %d, want 1100", got)
	}
	if got := len(snapshot.APIKeys[0].SevenDay.ProviderStats); got != 2 {
		t.Fatalf("7d provider stats = %d, want one row per session affinity id", got)
	}

	decision := store.checkLimit("client-key", ClientTokenLimits{TwelveHour: 1000, SevenDay: 1000}, now)
	if !decision.Exceeded || decision.Window != "7d" || decision.Used != 1100 {
		t.Fatalf("decision = %+v, want exceeded 7d at weighted 1100", decision)
	}
}

func TestNormalizeUsageStatTokensAppliesWeights(t *testing.T) {
	tokens := normalizeUsageStatTokens(usageStatTokens{
		ReadTokens:      100,
		CacheReadTokens: 40,
		WriteTokens:     3,
		ReasoningTokens: 2,
	})
	if tokens.TotalTokens != 940 {
		t.Fatalf("weighted total = %d, want 940", tokens.TotalTokens)
	}
}
