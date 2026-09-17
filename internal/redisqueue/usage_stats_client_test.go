package redisqueue

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/billing"
)

func TestClientUsageSnapshotIsolatedAndNonDestructive(t *testing.T) {
	now := time.Date(2026, 9, 9, 10, 0, 0, 0, time.UTC)
	store := &usageStatsStore{}
	store.add(usageStatsEvent{APIKey: "client-a", Timestamp: now, CostUSD: billing.USD(2 * billing.Scale), BillingVersion: 1, Provider: "private-provider", SessionAffinityID: "private-session", Tokens: usageStatTokens{ReadTokens: 100, WriteTokens: 20, CacheReadTokens: 80, TotalTokens: 120}}, now)
	store.add(usageStatsEvent{APIKey: "client-b", Timestamp: now, CostUSD: billing.USD(9 * billing.Scale), BillingVersion: 1, Tokens: usageStatTokens{TotalTokens: 999}}, now)
	clientCostLimitsMu.Lock()
	previousLimits := clientCostLimits
	clientCostLimits = map[string]ClientCostLimits{"client-a": {TwelveHour: billing.USD(billing.Scale)}, "unused": {SevenDay: billing.USD(5 * billing.Scale)}}
	clientCostLimitsMu.Unlock()
	t.Cleanup(func() {
		clientCostLimitsMu.Lock()
		clientCostLimits = previousLimits
		clientCostLimitsMu.Unlock()
	})

	if decision := store.checkLimit("client-a", ClientCostLimits{TwelveHour: billing.USD(billing.Scale)}, now); !decision.Exceeded {
		t.Fatal("fixture must have an exhausted inference budget")
	}
	for range 2 {
		snapshot := store.clientSnapshot("client-a", now)
		if snapshot.Usage.TwelveHour.CostUSD != billing.USD(2*billing.Scale) || snapshot.Usage.SevenDay.Tokens.TotalTokens != 120 || snapshot.Usage.Limits.TwelveHour != billing.USD(billing.Scale) {
			t.Fatalf("unexpected client totals: %+v", snapshot.Usage)
		}
		if snapshot.Windows != currentUsageStatsWindows(now) {
			t.Fatal("window boundaries differ from billing windows")
		}
		data, err := json.Marshal(snapshot)
		if err != nil {
			t.Fatal(err)
		}
		for _, private := range []string{"client-a", "client-b", "private-provider", "private-session", "api_keys", "historical_7d"} {
			if strings.Contains(string(data), private) {
				t.Fatalf("public snapshot contains %q", private)
			}
		}
	}
	if snapshot := store.snapshot(now); len(snapshot.APIKeys) != 2 || len(snapshot.APIKeys[1].TwelveHour.ProviderStats) != 1 {
		t.Fatal("public lookup modified the management snapshot")
	}
	unused := store.clientSnapshot("unused", now)
	if unused.Usage.SevenDay.RequestCount != 0 || unused.Usage.Limits.SevenDay != billing.USD(5*billing.Scale) {
		t.Fatalf("unused key must retain its limit and zero usage: %+v", unused.Usage)
	}
	rolled := store.clientSnapshot("client-a", now.Add(3*time.Hour))
	if rolled.Usage.TwelveHour.RequestCount != 0 || rolled.Usage.SevenDay.RequestCount != 1 {
		t.Fatal("public lookup did not roll the 12h window independently")
	}
}
