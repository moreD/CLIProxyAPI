package auth

import (
	"context"
	"math"
	"path/filepath"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/authusage"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/billing"
	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

func setupAuthCostStore(t *testing.T) *authusage.Store {
	t.Helper()
	store, err := authusage.NewStore(filepath.Join(t.TempDir(), "auth-usage.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	t.Cleanup(authusage.SetDefault(store))
	return store
}

type costExhaustingPoolExecutor struct {
	*openAICompatPoolExecutor
	store *authusage.Store
}

func (e *costExhaustingPoolExecutor) Execute(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	if err := e.store.Record(auth.Index, 100, time.Now()); err != nil {
		return cliproxyexecutor.Response{}, err
	}
	return e.openAICompatPoolExecutor.Execute(ctx, auth, req, opts)
}

func (e *costExhaustingPoolExecutor) ExecuteStream(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	if err := e.store.Record(auth.Index, 100, time.Now()); err != nil {
		return nil, err
	}
	return e.openAICompatPoolExecutor.ExecuteStream(ctx, auth, req, opts)
}

func TestAuthCostQuotaStopsBillableModelPoolRetries(t *testing.T) {
	for _, stream := range []bool{false, true} {
		name := "execute"
		if stream {
			name = "stream"
		}
		t.Run(name, func(t *testing.T) {
			store := setupAuthCostStore(t)
			failure := &Error{HTTPStatus: 429, Message: "billed upstream failure"}
			base := &openAICompatPoolExecutor{id: openAICompatPoolProviderKey, executeErrors: map[string]error{"first": failure}, streamFirstErrors: map[string]error{"first": failure}}
			manager := newOpenAICompatPoolTestManager(t, "alias", []internalconfig.OpenAICompatibilityModel{{Name: "first", Alias: "alias"}, {Name: "second", Alias: "alias"}}, base)
			manager.RegisterExecutor(&costExhaustingPoolExecutor{openAICompatPoolExecutor: base, store: store})
			auths := manager.List()
			if len(auths) != 1 {
				t.Fatal("expected one test auth")
			}
			if _, err := store.SetLimit(auths[0].Index, 100, time.Now()); err != nil {
				t.Fatal(err)
			}
			var err error
			var calls []string
			if stream {
				_, err = manager.ExecuteStream(context.Background(), []string{openAICompatPoolProviderKey}, cliproxyexecutor.Request{Model: "alias"}, cliproxyexecutor.Options{})
				calls = base.StreamModels()
			} else {
				_, err = manager.Execute(context.Background(), []string{openAICompatPoolProviderKey}, cliproxyexecutor.Request{Model: "alias"}, cliproxyexecutor.Options{})
				calls = base.ExecuteModels()
			}
			if err == nil || len(calls) != 1 || calls[0] != "first" {
				t.Fatalf("model retry bypassed cap: calls=%v err=%v", calls, err)
			}
		})
	}
}

func TestAuthCostQuotaBlocksBillableProbeButAllowsQuotaRefresh(t *testing.T) {
	store := setupAuthCostStore(t)
	now := time.Now()
	exec := &quotaRefreshTestExecutor{refreshQueue: []quotaRefreshResult{{quota: testQuota(now.Add(48*time.Hour), 10)}}}
	manager := NewManager(nil, nil, nil)
	manager.RegisterExecutor(exec)
	auth, err := manager.Register(context.Background(), &Auth{ID: "probe", Provider: "codex", Metadata: map[string]any{"email": "test@example.com"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetLimit(auth.Index, 100, now); err != nil {
		t.Fatal(err)
	}
	if err := store.Record(auth.Index, 100, now); err != nil {
		t.Fatal(err)
	}
	manager.runQuotaRefreshCycle(context.Background(), 0)
	exec.mu.Lock()
	defer exec.mu.Unlock()
	if len(exec.refreshCalls) != 1 || len(exec.probeCalls) != 0 {
		t.Fatalf("capped auth refresh/probe calls = %v / %v", exec.refreshCalls, exec.probeCalls)
	}
}

func TestAuthCostQuotaSkipsExhaustedAuthInCachedSchedulerAndSelectors(t *testing.T) {
	store := setupAuthCostStore(t)
	now := time.Now()
	limited := &Auth{ID: "limited", Index: "limited-index", Provider: "codex", Attributes: map[string]string{"priority": "10", "disable_cooling": "true"}}
	available := &Auth{ID: "available", Index: "available-index", Provider: "codex"}
	scheduler := newSchedulerForTest(&RoundRobinSelector{}, limited, available)
	if _, err := store.SetLimit(limited.Index, billing.USD(billing.Scale), now); err != nil {
		t.Fatal(err)
	}
	// Warm the ready cache before the usage record arrives.
	if got, err := scheduler.pickSingle(context.Background(), "codex", "", cliproxyexecutor.Options{}, nil); err != nil || got.ID != limited.ID {
		t.Fatalf("initial selection = %v, %v", got, err)
	}
	if err := store.RecordAt(limited.Index, billing.USD(billing.Scale), now, now); err != nil {
		t.Fatal(err)
	}
	// Reconciliation while exhausted must not cache an indefinite local block.
	scheduler.upsertAuth(limited)
	if got, err := scheduler.pickSingle(context.Background(), "codex", "", cliproxyexecutor.Options{}, nil); err != nil || got.ID != available.ID {
		t.Fatalf("cached selection after budget exhausted = %v, %v", got, err)
	}
	pinned := cliproxyexecutor.Options{Metadata: map[string]any{cliproxyexecutor.PinnedAuthMetadataKey: limited.ID}}
	if got, err := scheduler.pickSingle(context.Background(), "codex", "", pinned, nil); err == nil || got != nil {
		t.Fatalf("pinned selection bypassed budget: %v, %v", got, err)
	}
	for _, selector := range []Selector{&RoundRobinSelector{}, &FillFirstSelector{}, &WeightedRoundRobinSelector{}, NewSessionAffinitySelector(&RoundRobinSelector{})} {
		if got, err := selector.Pick(context.Background(), "codex", "", cliproxyexecutor.Options{}, []*Auth{limited, available}); err != nil || got.ID != available.ID {
			t.Fatalf("%T bypassed budget: %v, %v", selector, got, err)
		}
		if sticky, ok := selector.(*SessionAffinitySelector); ok {
			sticky.Stop()
		}
	}
	if _, err := store.SetLimit(limited.Index, billing.USD(2*billing.Scale), now); err != nil {
		t.Fatal(err)
	}
	if got, err := scheduler.pickSingle(context.Background(), "codex", "", pinned, nil); err != nil || got.ID != limited.ID {
		t.Fatalf("increased cap did not restore selection: %v, %v", got, err)
	}
}

func TestAuthCostQuotaFollowsOnlyUpstreamWeeklyReset(t *testing.T) {
	store := setupAuthCostStore(t)
	now := time.Date(2026, 9, 17, 10, 0, 0, 0, time.UTC)
	reset := now.Add(31*time.Hour + 17*time.Minute)
	auth := &Auth{ID: "weekly", Index: "weekly-index", Provider: "codex", RuntimeQuota: &QuotaInfo{
		FiveHour: QuotaWindow{NextFreshAt: now.Add(time.Hour), RefreshedAt: now},
		Weekly:   QuotaWindow{NextFreshAt: reset, RefreshedAt: now, LimitWindowSeconds: 604800},
	}}
	if err := ObserveAuthCostWindow(auth, now); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetLimit(auth.Index, 100, now); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordAt(auth.Index, 100, now, now); err != nil {
		t.Fatal(err)
	}
	if blocked, until := authCostBlocked(auth, now.Add(time.Hour)); !blocked || !until.Equal(reset) {
		t.Fatalf("short window reset dollar cap: blocked=%v reset=%v", blocked, until)
	}
	if blocked, _ := authCostBlocked(auth, reset); blocked {
		t.Fatal("auth still blocked at its upstream weekly reset")
	}
	if err := store.RecordAt(auth.Index, 100, reset.Add(time.Minute), reset.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if blocked, _ := authCostBlocked(auth, reset.Add(2*time.Minute)); !blocked {
		t.Fatal("repeated stale provider reset cleared the next period's usage")
	}
	monthly := auth.Clone()
	monthly.Index = "monthly-index"
	monthly.RuntimeQuota.Weekly.LimitWindowSeconds = 2592000
	if err := ObserveAuthCostWindow(monthly, now); err != nil {
		t.Fatal(err)
	}
	if got := store.Snapshot(monthly.Index, now); !got.Window.End.IsZero() {
		t.Fatal("monthly window treated as weekly")
	}
}

func TestAuthCostQuotaHeaderResetRestoresCachedScheduler(t *testing.T) {
	store := setupAuthCostStore(t)
	now := time.Now().UTC().Truncate(time.Second)
	previous := now.Add(-2 * time.Hour)
	start := now.Add(-time.Hour)
	auth := &Auth{ID: "header-reset", Index: "header-reset-index", Provider: "claude"}
	if err := store.ObserveWeeklyQuota(auth.Index, now.Add(48*time.Hour), previous, previous, 80, true); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetLimit(auth.Index, 100, now); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordAt(auth.Index, 100, previous, now); err != nil {
		t.Fatal(err)
	}
	// Build the scheduler while exhausted. Header observations do not upsert
	// auth metadata, so a dollar cooldown cached here would never recover early.
	scheduler := newSchedulerForTest(&RoundRobinSelector{}, auth)
	if got, err := scheduler.pickSingle(context.Background(), "claude", "", cliproxyexecutor.Options{}, nil); got != nil || err == nil {
		t.Fatalf("exhausted auth was selected: %v %v", got, err)
	}
	if err := store.ObserveWeeklyQuota(auth.Index, start.Add(7*24*time.Hour), now, now, 0, true); err != nil {
		t.Fatal(err)
	}
	if got, err := scheduler.pickSingle(context.Background(), "claude", "", cliproxyexecutor.Options{}, nil); got == nil || err != nil {
		t.Fatalf("header reset did not restore eligibility: %v %v", got, err)
	}
	// A local budget reset cannot bypass an independent provider cooldown.
	auth.Quota = QuotaState{Exceeded: true, Reason: "credential_quota", NextRecoverAt: now.Add(time.Hour)}
	scheduler.upsertAuth(auth)
	if got, err := scheduler.pickSingle(context.Background(), "claude", "", cliproxyexecutor.Options{}, nil); got != nil || err == nil {
		t.Fatalf("local reset bypassed provider cooldown: %v %v", got, err)
	}
}

func TestAuthCostQuotaEarlyUpstreamResetRefreshesScheduler(t *testing.T) {
	store := setupAuthCostStore(t)
	now := time.Now().UTC().Truncate(time.Second)
	previousObservation := now.Add(-2 * time.Hour)
	earlyStart := now.Add(-time.Hour)
	oldQuota := &QuotaInfo{Weekly: QuotaWindow{LimitWindowSeconds: 604800, NextFreshAt: now.Add(48 * time.Hour), RefreshedAt: previousObservation, UsedPercent: 80, UsedPercentKnown: true}}
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	auth, err := manager.Register(context.Background(), &Auth{ID: "early-reset", Provider: "codex", RuntimeQuota: oldQuota})
	if err != nil {
		t.Fatal(err)
	}
	manager.storeQuotaRefreshResult(context.Background(), auth.ID, oldQuota, previousObservation)
	if _, err = store.SetLimit(auth.Index, 100, now); err != nil {
		t.Fatal(err)
	}
	if err = store.RecordAt(auth.Index, 100, previousObservation.Add(time.Minute), now); err != nil {
		t.Fatal(err)
	}
	// A request from the new cycle was already billed before quota polling caught up.
	if err = store.RecordAt(auth.Index, 10, earlyStart.Add(time.Minute), now); err != nil {
		t.Fatal(err)
	}
	pinned := cliproxyexecutor.Options{Metadata: map[string]any{cliproxyexecutor.PinnedAuthMetadataKey: auth.ID}}
	if got, err := manager.scheduler.pickSingle(context.Background(), "codex", "", pinned, nil); got != nil || err == nil {
		t.Fatalf("expected capped auth before observation, got %v %v", got, err)
	}
	// The scheduler rejects metadata older than registration; simulate polling
	// after registration while keeping the upstream cycle timestamps controlled.
	now = time.Now().UTC()
	newQuota := &QuotaInfo{Weekly: QuotaWindow{LimitWindowSeconds: 604800, NextFreshAt: earlyStart.Add(7 * 24 * time.Hour), RefreshedAt: now, UsedPercentKnown: true}}
	manager.storeQuotaRefreshResult(context.Background(), auth.ID, newQuota, now)
	if got := store.Snapshot(auth.Index, now); got.CostUSD != 10 || got.TotalCostUSD != 110 || !got.Window.Start.Equal(earlyStart) {
		t.Fatalf("early reset lost current-period billing: %+v", got)
	}
	if got, err := manager.scheduler.pickSingle(context.Background(), "codex", "", pinned, nil); err != nil || got == nil || got.ID != auth.ID {
		t.Fatalf("quota refresh did not restore scheduler eligibility: %v %v", got, err)
	}
	if err = store.RecordAt(auth.Index, 90, now, now); err != nil {
		t.Fatal(err)
	}
	for _, quota := range []*QuotaInfo{newQuota, oldQuota} {
		manager.storeQuotaRefreshResult(context.Background(), auth.ID, quota, now)
		if got := store.Snapshot(auth.Index, now); got.CostUSD != 100 || !got.Exceeded {
			t.Fatalf("duplicate/stale observation cleared costs: %+v", got)
		}
	}
}

func TestAuthWeeklyUsedPercentRejectsUnknownValues(t *testing.T) {
	for _, tc := range []struct {
		name    string
		window  QuotaWindow
		percent float64
		known   bool
	}{
		{"unknown zero", QuotaWindow{}, 0, false},
		{"known zero", QuotaWindow{UsedPercentKnown: true}, 0, true},
		{"legacy nonzero", QuotaWindow{UsedPercent: 40}, 40, true},
		{"counter ratio", QuotaWindow{Used: 1, Limit: 4}, 25, true},
		{"known zero counters", QuotaWindow{Limit: 4}, 0, true},
		{"nan", QuotaWindow{UsedPercent: math.NaN(), UsedPercentKnown: true}, 0, false},
		{"infinite", QuotaWindow{UsedPercent: math.Inf(1), UsedPercentKnown: true}, 0, false},
		{"negative", QuotaWindow{UsedPercent: -1, UsedPercentKnown: true}, 0, false},
		{"over percent", QuotaWindow{UsedPercent: 101, UsedPercentKnown: true}, 0, false},
		{"invalid counters", QuotaWindow{Used: 5, Limit: 4}, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if percent, known := authWeeklyUsedPercent(tc.window); percent != tc.percent || known != tc.known {
				t.Fatalf("percent=%v known=%v, want %v %v", percent, known, tc.percent, tc.known)
			}
		})
	}
}
