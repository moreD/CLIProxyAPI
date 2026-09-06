package redisqueue

import (
	"fmt"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/billing"
	"path/filepath"
)

type BillingMigrationReport struct {
	Events            int         `json:"events"`
	HistoricalWindows int         `json:"historical_windows"`
	EventCostUSD      billing.USD `json:"event_cost_usd"`
	HistoricalCostUSD billing.USD `json:"historical_cost_usd"`
	MigratedEvents    int         `json:"migrated_events"`
	DryRun            bool        `json:"dry_run"`
}

// MigrateUsageBilling is an explicit offline maintenance operation. Stop the
// writer first and back up the directory. Each event file is replaced atomically;
// history updates use one SQLite transaction. Versioned costs make retries safe.
// It preserves all windows and request timestamps, unlike normal startup pruning.
func MigrateUsageBilling(dir string, dryRun bool) (BillingMigrationReport, error) {
	r := BillingMigrationReport{DryRun: dryRun}
	events, err := readUsageStatsEvents(dir)
	if err != nil {
		return r, err
	}
	path := filepath.Join(dir, usageStatsHistoryDBFileName)
	history, err := readUsageStatsHistory(path)
	if err != nil {
		return r, err
	}
	for i, event := range events {
		if event.BillingVersion < 1 {
			r.MigratedEvents++
		}
		events[i] = normalizeBilledEvent(event)
		r.EventCostUSD = billing.Add(r.EventCostUSD, events[i].CostUSD)
	}
	for _, stat := range history {
		if len(stat.SevenDay.ProviderStats) == 0 && stat.SevenDay.Tokens.TotalTokens > 0 {
			return r, fmt.Errorf("historical window has no per-model token breakdown; manual recovery required")
		}
		r.HistoricalCostUSD = billing.Add(r.HistoricalCostUSD, stat.SevenDay.CostUSD)
	}
	r.Events = len(events)
	r.HistoricalWindows = len(history)
	if dryRun {
		return r, nil
	}
	if err = writeUsageStatsEventsWithCleanup(dir, events, false); err != nil {
		return r, err
	}
	if err = writeUsageStatsHistory(path, history); err != nil {
		return r, err
	}
	return r, nil
}
