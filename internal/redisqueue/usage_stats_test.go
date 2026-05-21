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
	prevWindow := int(usageStatsWindowSeconds.Load())
	SetUsageStatsWindowSeconds(7 * 24 * 60 * 60)
	t.Cleanup(func() { SetUsageStatsWindowSeconds(prevWindow) })

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
	if got.Tokens.ReadTokens != 100 || got.Tokens.CacheReadTokens != 60 || got.Tokens.WriteTokens != 20 {
		t.Fatalf("tokens = %+v, want read/cache-read/write 100/60/20", got.Tokens)
	}
}
