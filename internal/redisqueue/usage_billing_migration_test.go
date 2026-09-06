package redisqueue

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/billing"
)

func TestMigrateUsageBillingIsIdempotentAndPreservesHistory(t *testing.T) {
	dir := t.TempDir()
	useMigrationHistoryPath(t, dir)
	now := time.Date(2026, 5, 21, 1, 2, 3, 4, time.UTC)
	events := []usageStatsEvent{{
		Timestamp: now, APIKey: "client", Provider: "codex", Model: "gpt-5.6-sol",
		Tokens: usageStatTokens{ReadTokens: 100, WriteTokens: 20, ReasoningTokens: 5, TotalTokens: 120},
	}}
	if err := writeUsageStatsEvents(dir, events); err != nil {
		t.Fatal(err)
	}
	artifactPath := filepath.Join(dir, "unrelated.json")
	if err := os.WriteFile(artifactPath, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	window := UsageWindowInfo{Start: now.Add(-7 * 24 * time.Hour), End: now}
	firstRequest, lastRequest := now.Add(-time.Hour), now.Add(-time.Minute)
	history := []ClientUsageHistoricalStat{{APIKey: "client", Window: window, SevenDay: ClientUsageWindowStat{
		RequestCount: 2, FirstRequest: firstRequest, LastRequest: lastRequest,
		ProviderStats: []ProviderUsageStat{{Provider: "codex", Model: "gpt-5.6-sol", RequestCount: 2, FirstRequest: firstRequest, LastRequest: lastRequest, Tokens: usageStatTokens{ReadTokens: 200, WriteTokens: 40}}},
	}}}
	if err := writeUsageStatsHistory(usageStatsHistoryDBPath, history); err != nil {
		t.Fatal(err)
	}

	first, err := MigrateUsageBilling(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	if first.MigratedEvents != 1 || first.Events != 1 || first.HistoricalWindows != 1 {
		t.Fatalf("first report = %+v", first)
	}
	if artifact, err := os.ReadFile(artifactPath); err != nil || string(artifact) != "{}" {
		t.Fatalf("unrelated artifact = %q, %v; want preserved", artifact, err)
	}
	migratedEvents, err := readUsageStatsEvents(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := migratedEvents[0]; got.Timestamp != now || got.BillingVersion != 1 || got.CostUSD != billing.USD(800_000) {
		t.Fatalf("migrated event = %+v", got)
	}
	migratedHistory, err := readUsageStatsHistory(usageStatsHistoryDBPath)
	if err != nil {
		t.Fatal(err)
	}
	gotHistory := migratedHistory[0]
	if gotHistory.Window != window || gotHistory.SevenDay.FirstRequest != firstRequest || gotHistory.SevenDay.LastRequest != lastRequest {
		t.Fatalf("historical times changed: %+v", gotHistory)
	}
	if got := gotHistory.SevenDay.ProviderStats[0]; got.BillingVersion != 1 || got.CostUSD != billing.USD(1_600_000) {
		t.Fatalf("historical billing = %+v", got)
	}

	second, err := MigrateUsageBilling(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	if second.MigratedEvents != 0 || second.EventCostUSD != first.EventCostUSD || second.HistoricalCostUSD != first.HistoricalCostUSD {
		t.Fatalf("second report = %+v, first = %+v", second, first)
	}
}

func TestMigrateUsageBillingRestartsAfterPartialRunAcross100KEvents(t *testing.T) {
	dir := t.TempDir()
	useMigrationHistoryPath(t, dir)
	now := time.Date(2026, 5, 21, 1, 0, 0, 0, time.UTC)
	const count = 100_000
	events := make([]usageStatsEvent, count)
	for i := range events {
		events[i] = usageStatsEvent{
			Timestamp: now.Add(time.Duration(i) * time.Nanosecond), APIKey: "stress", Provider: "codex", Model: "gpt-5.6-sol",
			Tokens: usageStatTokens{ReadTokens: 1, TotalTokens: 1},
		}
		// Model an interrupted prior run: its first atomic event-file replacement
		// was already fully versioned while the remaining data stayed legacy.
		if i < count/2 {
			events[i].BillingVersion = 1
			events[i].CostUSD = billing.USD(4_000)
		}
	}
	if err := writeUsageStatsEvents(dir, events); err != nil {
		t.Fatal(err)
	}

	report, err := MigrateUsageBilling(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	if report.Events != count || report.MigratedEvents != count/2 || report.EventCostUSD != billing.USD(400_000_000) {
		t.Fatalf("report = %+v", report)
	}
	got, err := readUsageStatsEvents(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != count {
		t.Fatalf("events = %d, want %d", len(got), count)
	}
	for i := range got {
		if got[i].BillingVersion != 1 || got[i].CostUSD != billing.USD(4_000) {
			t.Fatalf("event %d = %+v", i, got[i])
		}
	}
}

func TestMigrateUsageBillingDryRunDoesNotInitializeEmptySQLite(t *testing.T) {
	dir := t.TempDir()
	useMigrationHistoryPath(t, dir)
	if err := os.WriteFile(usageStatsHistoryDBPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(usageStatsHistoryDBPath)
	if err != nil {
		t.Fatal(err)
	}

	report, err := MigrateUsageBilling(dir, true)
	if err != nil {
		t.Fatalf("dry-run empty SQLite: %v", err)
	}
	if !report.DryRun || report.HistoricalWindows != 0 {
		t.Fatalf("report = %+v, want empty dry-run", report)
	}
	after, err := os.Stat(usageStatsHistoryDBPath)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(usageStatsHistoryDBPath)
	if err != nil {
		t.Fatal(err)
	}
	if before.Size() != 0 || after.Size() != 0 || len(data) != 0 || !after.ModTime().Equal(before.ModTime()) {
		t.Fatalf("empty SQLite mutated: before=%+v after=%+v bytes=%d", before, after, len(data))
	}
}

func useMigrationHistoryPath(t *testing.T, dir string) {
	t.Helper()
	previous := usageStatsHistoryDBPath
	usageStatsHistoryDBPath = dir + "/" + usageStatsHistoryDBFileName
	t.Cleanup(func() { usageStatsHistoryDBPath = previous })
}
