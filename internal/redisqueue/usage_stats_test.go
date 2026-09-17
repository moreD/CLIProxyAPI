package redisqueue

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/billing"
)

func TestUsageStatsPersistAndRecoverEvents(t *testing.T) {
	dir := t.TempDir()
	historyDBPath := filepath.Join(t.TempDir(), usageStatsHistoryDBFileName)
	now := time.Date(2026, 5, 21, 1, 0, 0, 0, time.UTC)

	store := &usageStatsStore{
		events: []usageStatsEvent{
			{
				Timestamp: now.Add(-time.Minute),
				APIKey:    "client-key",
				Provider:  "codex",
				Model:     "gpt-5.5",
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
	prevHistoryDBPath := usageStatsHistoryDBPath
	usageStatsPersistenceDir = dir
	usageStatsHistoryDBPath = historyDBPath
	t.Cleanup(func() {
		usageStatsPersistenceDir = prevDir
		usageStatsHistoryDBPath = prevHistoryDBPath
	})

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
	if got.SevenDay.Tokens.TotalTokens != 120 {
		t.Fatalf("total tokens = %d, want 120", got.SevenDay.Tokens.TotalTokens)
	}
	if got.SevenDay.CostUSD != billing.USD(830_000) {
		t.Fatalf("cost = %s, want 0.000830000", got.SevenDay.CostUSD)
	}
}

func TestFlushUsageStatsPersistsGlobalPerAPIKeyState(t *testing.T) {
	dir := t.TempDir()
	historyDBPath := filepath.Join(dir, usageStatsHistoryDBFileName)
	now := time.Now().UTC()

	prevDir := usageStatsPersistenceDir
	prevHistoryDBPath := usageStatsHistoryDBPath
	usageStatsPersistenceDir = dir
	usageStatsHistoryDBPath = historyDBPath
	globalUsageStats = usageStatsStore{}
	t.Cleanup(func() {
		usageStatsPersistenceDir = prevDir
		usageStatsHistoryDBPath = prevHistoryDBPath
		globalUsageStats = usageStatsStore{}
	})

	globalUsageStats.add(usageStatsEvent{
		Timestamp: now,
		APIKey:    "client-key",
		Provider:  "codex",
		Model:     "gpt-5.6-sol",
		Tokens: usageStatTokens{
			ReadTokens:  100,
			WriteTokens: 20,
			TotalTokens: 120,
		},
	}, now)

	if err := FlushUsageStats(); err != nil {
		t.Fatalf("FlushUsageStats() error = %v", err)
	}

	recovered := &usageStatsStore{}
	if err := recovered.recoverFromDisk(now); err != nil {
		t.Fatalf("recoverFromDisk() error = %v", err)
	}
	snapshot := recovered.snapshot(now)
	if len(snapshot.APIKeys) != 1 {
		t.Fatalf("api keys = %d, want 1", len(snapshot.APIKeys))
	}
	if got := snapshot.APIKeys[0]; got.APIKey != "client-key" || got.SevenDay.RequestCount != 1 {
		t.Fatalf("recovered usage = %+v, want one request for client-key", got)
	}
}

func TestUsageStatsPersistAndRecoverWithHistoryDBInStatsDirectory(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 5, 21, 1, 0, 0, 0, time.UTC)
	historyDBPath := filepath.Join(dir, usageStatsHistoryDBFileName)

	prevDir := usageStatsPersistenceDir
	prevHistoryDBPath := usageStatsHistoryDBPath
	usageStatsPersistenceDir = dir
	usageStatsHistoryDBPath = historyDBPath
	t.Cleanup(func() {
		usageStatsPersistenceDir = prevDir
		usageStatsHistoryDBPath = prevHistoryDBPath
	})

	store := &usageStatsStore{
		events: []usageStatsEvent{{
			Timestamp: now,
			APIKey:    "client-key",
			Provider:  "codex",
			Model:     "gpt-5.6-sol",
		}},
	}
	if err := store.persistToDisk(now); err != nil {
		t.Fatalf("persistToDisk() error = %v", err)
	}
	if err := writeUsageStatsHistory(historyDBPath, []ClientUsageHistoricalStat{{
		APIKey: "client-key",
		Window: UsageWindowInfo{
			Start: now.Add(-7 * 24 * time.Hour),
			End:   now,
		},
		SevenDay: ClientUsageWindowStat{
			RequestCount: 1,
			FirstRequest: now.Add(-time.Hour),
			LastRequest:  now,
		},
	}}); err != nil {
		t.Fatalf("writeUsageStatsHistory() error = %v", err)
	}

	recovered := &usageStatsStore{}
	if err := recovered.recoverFromDisk(now); err != nil {
		t.Fatalf("recoverFromDisk() error = %v", err)
	}
	if _, err := os.Stat(historyDBPath); err != nil {
		t.Fatalf("history database was not preserved: %v", err)
	}
	if len(recovered.events) != 1 {
		t.Fatalf("recovered events = %d, want 1", len(recovered.events))
	}

	for _, suffix := range []string{"-journal", "-wal", "-shm"} {
		sidecarPath := historyDBPath + suffix
		if err := os.WriteFile(sidecarPath, []byte{0, 1, 2}, 0o600); err != nil {
			t.Fatalf("write history sidecar %q: %v", suffix, err)
		}
	}
	if _, err := readUsageStatsEvents(dir); err != nil {
		t.Fatalf("readUsageStatsEvents() with history sidecars error = %v", err)
	}
	if err := writeUsageStatsEvents(dir, recovered.events); err != nil {
		t.Fatalf("writeUsageStatsEvents() with history sidecars error = %v", err)
	}
	for _, suffix := range []string{"-journal", "-wal", "-shm"} {
		if _, err := os.Stat(historyDBPath + suffix); err != nil {
			t.Fatalf("history sidecar %q was not preserved: %v", suffix, err)
		}
	}
}

func TestUsageStatsPersistsClosedSevenDayWindowStatsOnly(t *testing.T) {
	dir := t.TempDir()
	historyDBPath := filepath.Join(t.TempDir(), usageStatsHistoryDBFileName)
	windowNow := time.Date(2026, 5, 21, 1, 0, 0, 0, time.UTC)
	nextWindowNow := time.Date(2026, 5, 24, 1, 0, 0, 0, time.UTC)

	prevDir := usageStatsPersistenceDir
	prevHistoryDBPath := usageStatsHistoryDBPath
	usageStatsPersistenceDir = dir
	usageStatsHistoryDBPath = historyDBPath
	t.Cleanup(func() {
		usageStatsPersistenceDir = prevDir
		usageStatsHistoryDBPath = prevHistoryDBPath
	})

	store := &usageStatsStore{}
	store.add(usageStatsEvent{
		Timestamp:         windowNow,
		APIKey:            "client-key",
		SessionAffinityID: "codex:session-1",
		Provider:          "codex",
		Model:             "gpt-5.5",
		LatencyMs:         120,
		Tokens: usageStatTokens{
			ReadTokens:  100,
			WriteTokens: 10,
		},
	}, windowNow)

	if err := store.persistToDisk(nextWindowNow); err != nil {
		t.Fatalf("persistToDisk() error = %v", err)
	}

	if _, err := os.Stat(filepath.Join(dir, "client-key")); !os.IsNotExist(err) {
		t.Fatalf("closed-window detail file still exists, stat error = %v", err)
	}

	history, err := readUsageStatsHistory(historyDBPath)
	if err != nil {
		t.Fatalf("readUsageStatsHistory() error = %v", err)
	}
	if len(history) != 1 {
		t.Fatalf("history rows = %d, want 1", len(history))
	}
	got := history[0]
	if got.APIKey != "client-key" {
		t.Fatalf("api key = %q, want client-key", got.APIKey)
	}
	if !got.Window.Start.Equal(time.Date(2026, 5, 17, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("window start = %s, want 2026-05-17T00:00:00Z", got.Window.Start)
	}
	if got.SevenDay.RequestCount != 1 || got.SevenDay.Tokens.TotalTokens != 110 {
		t.Fatalf("history stat = %+v, want one request and 110 total tokens", got.SevenDay)
	}
	if len(got.SevenDay.ProviderStats) != 1 {
		t.Fatalf("provider stats = %d, want 1", len(got.SevenDay.ProviderStats))
	}
}

func TestUsageStatsRecoverPersistsStaleClosedSevenDayWindowStatsOnly(t *testing.T) {
	dir := t.TempDir()
	historyDBPath := filepath.Join(t.TempDir(), usageStatsHistoryDBFileName)
	staleWindowNow := time.Date(2026, 5, 21, 1, 0, 0, 0, time.UTC)
	recoverNow := time.Date(2026, 5, 24, 1, 0, 0, 0, time.UTC)
	event := usageStatsEvent{
		Timestamp: staleWindowNow,
		APIKey:    "client-key",
		Provider:  "codex",
		Model:     "gpt-5.5",
		Tokens:    usageStatTokens{ReadTokens: 50},
	}
	if err := writeUsageStatsEvents(dir, []usageStatsEvent{event}); err != nil {
		t.Fatalf("writeUsageStatsEvents() error = %v", err)
	}

	prevDir := usageStatsPersistenceDir
	prevHistoryDBPath := usageStatsHistoryDBPath
	usageStatsPersistenceDir = dir
	usageStatsHistoryDBPath = historyDBPath
	t.Cleanup(func() {
		usageStatsPersistenceDir = prevDir
		usageStatsHistoryDBPath = prevHistoryDBPath
	})

	recovered := &usageStatsStore{}
	if err := recovered.recoverFromDisk(recoverNow); err != nil {
		t.Fatalf("recoverFromDisk() error = %v", err)
	}
	if len(recovered.events) != 0 {
		t.Fatalf("recovered events = %d, want 0", len(recovered.events))
	}
	if _, err := os.Stat(filepath.Join(dir, "client-key")); !os.IsNotExist(err) {
		t.Fatalf("stale detail file still exists, stat error = %v", err)
	}

	history, err := readUsageStatsHistory(historyDBPath)
	if err != nil {
		t.Fatalf("readUsageStatsHistory() error = %v", err)
	}
	if len(history) != 1 {
		t.Fatalf("history rows = %d, want 1", len(history))
	}
	if got := history[0].SevenDay.Tokens.TotalTokens; got != 50 {
		t.Fatalf("total tokens = %d, want 50", got)
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
		Model:             "gpt-5.5",
		Tokens:            usageStatTokens{TotalTokens: 80, ReadTokens: 80},
	}, now)
	store.add(usageStatsEvent{
		Timestamp:         now.Add(-3 * time.Hour),
		APIKey:            "client-key",
		SessionAffinityID: "session-b",
		Provider:          "codex",
		Model:             "gpt-5.5",
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
	if got := snapshot.APIKeys[0].TwelveHour.Tokens.TotalTokens; got != 80 {
		t.Fatalf("12h total tokens = %d, want 80", got)
	}
	if got := snapshot.APIKeys[0].SevenDay.Tokens.TotalTokens; got != 110 {
		t.Fatalf("7d total tokens = %d, want 110", got)
	}
	if got := len(snapshot.APIKeys[0].SevenDay.ProviderStats); got != 2 {
		t.Fatalf("7d provider stats = %d, want one row per session affinity id", got)
	}

	decision := store.checkLimit("client-key", ClientCostLimits{TwelveHour: billing.USD(400_001), SevenDay: billing.USD(400_000)}, now)
	if !decision.Exceeded || decision.Window != "7d" || decision.Used != billing.USD(550_000) {
		t.Fatalf("decision = %+v, want exceeded 7d at $0.000550000", decision)
	}
}

func TestNormalizeUsageStatTokensUsesCanonicalRawTotal(t *testing.T) {
	tokens := normalizeUsageStatTokens(usageStatTokens{
		ReadTokens:      100,
		CacheReadTokens: 40,
		WriteTokens:     3,
		ReasoningTokens: 2,
	})
	if tokens.TotalTokens != 103 {
		t.Fatalf("total tokens = %d, want 103", tokens.TotalTokens)
	}
}

func TestUsageCostLimitBoundaryIsInclusive(t *testing.T) {
	now := time.Date(2026, 5, 21, 13, 0, 0, 0, time.UTC)
	store := &usageStatsStore{}
	store.add(usageStatsEvent{Timestamp: now, APIKey: "key", Model: "gpt-5.6-sol", Tokens: usageStatTokens{ReadTokens: 100}}, now)

	const cost billing.USD = 400_000
	if got := store.checkLimit("key", ClientCostLimits{TwelveHour: cost + 1}, now); got.Exceeded {
		t.Fatalf("limit above cost unexpectedly exceeded: %+v", got)
	}
	if got := store.checkLimit("key", ClientCostLimits{TwelveHour: cost}, now); !got.Exceeded || got.Used != cost || got.Limit != cost {
		t.Fatalf("exact limit decision = %+v, want inclusive boundary at %s", got, cost)
	}
}

func TestRecoveryFailureFailsClosedAndPreventsFlushOverwrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "preserved")
	want := []byte(`{"api_key":"preserved","events":[]}`)
	if err := os.WriteFile(path, want, 0o600); err != nil {
		t.Fatal(err)
	}

	previousDir := usageStatsPersistenceDir
	previousHistory := usageStatsHistoryDBPath
	clientCostLimitsMu.Lock()
	previousLimits := clientCostLimits
	clientCostLimits = map[string]ClientCostLimits{"limited-key": {TwelveHour: billing.USD(1)}}
	clientCostLimitsMu.Unlock()
	usageStatsPersistenceDir = dir
	usageStatsHistoryDBPath = filepath.Join(dir, usageStatsHistoryDBFileName)
	usageStatsRecoveryFailed.Store(true)
	t.Cleanup(func() {
		usageStatsRecoveryFailed.Store(false)
		usageStatsPersistenceDir = previousDir
		usageStatsHistoryDBPath = previousHistory
		clientCostLimitsMu.Lock()
		clientCostLimits = previousLimits
		clientCostLimitsMu.Unlock()
		globalUsageStats = usageStatsStore{}
	})
	globalUsageStats = usageStatsStore{events: []usageStatsEvent{{Timestamp: time.Now(), APIKey: "replacement", Model: "gpt-5.6-sol"}}}

	if decision := CheckClientCostLimit("unlimited-key", time.Now()); decision.Exceeded || decision.Unavailable {
		t.Fatalf("unlimited decision = %+v, want unaffected by recovery failure", decision)
	}
	if decision := CheckClientCostLimit("limited-key", time.Now()); !decision.Exceeded || !decision.Unavailable {
		t.Fatalf("decision = %+v, want fail-closed unavailable", decision)
	}
	if err := FlushUsageStats(); err == nil {
		t.Fatal("FlushUsageStats() succeeded after recovery failure")
	}
	if _, available := ClientUsageSnapshotNow("limited-key"); available {
		t.Fatal("client usage lookup succeeded after recovery failure")
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Fatalf("persisted data changed: got %q want %q", got, want)
	}
	if _, err = os.Stat(filepath.Join(dir, "replacement")); !os.IsNotExist(err) {
		t.Fatalf("replacement file created, stat error = %v", err)
	}
}

func TestNormalizeLegacyTokensSaturatesOverflow(t *testing.T) {
	const maxInt64 = int64(^uint64(0) >> 1)
	got := normalizeLegacyTokens(usageStatTokens{
		ReadTokens: maxInt64, WriteTokens: maxInt64, ReasoningTokens: 1,
		CacheReadTokens: 1, CacheWriteTokens: 1,
	}, "claude")
	if got.ReadTokens != maxInt64 || got.WriteTokens != maxInt64 || got.TotalTokens != maxInt64 {
		t.Fatalf("normalized overflow = %+v, want saturated counters", got)
	}
}

func TestDefaultUsageStatsPathsStayUnderWorkingDirectory(t *testing.T) {
	if filepath.IsAbs(defaultUsageStatsPersistenceDir) {
		t.Fatalf("default stats dir = %q, want relative path under working directory", defaultUsageStatsPersistenceDir)
	}

	wantHistoryPath := filepath.Join(defaultUsageStatsPersistenceDir, usageStatsHistoryDBFileName)
	if got := defaultUsageStatsHistoryDBPath(); got != wantHistoryPath {
		t.Fatalf("default history DB path = %q, want %q", got, wantHistoryPath)
	}
}
