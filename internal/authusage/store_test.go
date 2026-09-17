package authusage

import (
	"database/sql"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/billing"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	s, err := NewStore(filepath.Join(t.TempDir(), "auth.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestEarlyUpstreamResetPreservesNewCycleCostsAcrossRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.sqlite")
	s, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 17, 10, 0, 0, 0, time.UTC)
	oldEnd := now.Add(3 * 24 * time.Hour)
	start := now.Add(time.Hour)
	newEnd := start.Add(7 * 24 * time.Hour)
	seen := start.Add(10 * time.Minute)
	if _, err = s.SetLimit("a", 100, now); err != nil {
		t.Fatal(err)
	}
	if err = s.ObserveWeeklyQuota("a", oldEnd, now, now, 85, true); err != nil {
		t.Fatal(err)
	}
	for _, event := range []struct {
		cost billing.USD
		at   time.Time
	}{{100, now}, {7, start}, {8, seen}} {
		if err = s.RecordAt("a", event.cost, event.at, event.at); err != nil {
			t.Fatal(err)
		}
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err = s.ObserveWeeklyQuota("a", newEnd, seen, seen, 2, true); err != nil {
		t.Fatal(err)
	}
	state := s.Snapshot("a", seen)
	if state.Exceeded || state.CostUSD != 15 || state.TotalCostUSD != 115 || !state.Window.Start.Equal(start) || !state.Window.End.Equal(newEnd) {
		t.Fatalf("early reset lost new spending or did not reopen auth: %+v", state)
	}
	// A late old-cycle request adds lifetime usage only. A new-cycle request
	// can exhaust the budget again; repeated/stale observations must not reopen it.
	if err = s.RecordAt("a", 9, now, seen); err != nil {
		t.Fatal(err)
	}
	if err = s.RecordAt("a", 85, seen, seen); err != nil {
		t.Fatal(err)
	}
	for _, observation := range []struct {
		end, at time.Time
		used    float64
	}{
		{newEnd, seen, 2}, {newEnd, seen.Add(time.Minute), 1}, {oldEnd, now, 85},
	} {
		if err = s.ObserveWeeklyQuota("a", observation.end, observation.at, seen.Add(time.Minute), observation.used, true); err != nil {
			t.Fatal(err)
		}
	}
	state = s.Snapshot("a", seen.Add(time.Minute))
	if !state.Exceeded || state.CostUSD != 100 || state.TotalCostUSD != 209 || !state.Window.Start.Equal(start) {
		t.Fatalf("replayed observation or delayed response changed budget: %+v", state)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.ObserveWeeklyQuota("a", newEnd, seen, seen.Add(time.Minute), 2, true); err != nil {
		t.Fatal(err)
	}
	if state = s.Snapshot("a", seen.Add(time.Minute)); !state.Exceeded || state.CostUSD != 100 {
		t.Fatalf("restart forgot consumed early reset: %+v", state)
	}
}

func TestEarlyResetRequiresNewCycleAndFallingKnownUsage(t *testing.T) {
	now := time.Date(2026, 9, 17, 10, 0, 0, 0, time.UTC)
	oldEnd := now.Add(3 * 24 * time.Hour)
	seen := now.Add(2 * time.Hour)
	newEnd := now.Add(time.Hour + 7*24*time.Hour)
	for _, tc := range []struct {
		name    string
		end, at time.Time
		used    float64
		known   bool
	}{
		{"same deadline", oldEnd, seen, 0, true},
		{"deadline jitter", oldEnd.Add(time.Second), seen, 84, true},
		{"deadline only", newEnd, seen, 0, false},
		{"usage increased", newEnd, seen, 90, true},
		{"stale observation", newEnd, now.Add(-time.Second), 0, true},
		{"cycle has not started", seen.Add(8 * 24 * time.Hour), seen, 0, true},
		{"invalid percent", newEnd, seen, math.NaN(), true},
		{"negative percent", newEnd, seen, -1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := testStore(t)
			if _, err := s.SetLimit("a", 100, now); err != nil {
				t.Fatal(err)
			}
			if err := s.ObserveWeeklyQuota("a", oldEnd, now, now, 85, true); err != nil {
				t.Fatal(err)
			}
			if err := s.RecordAt("a", 100, now, now); err != nil {
				t.Fatal(err)
			}
			if err := s.ObserveWeeklyQuota("a", tc.end, tc.at, seen, tc.used, tc.known); err != nil {
				t.Fatal(err)
			}
			if got := s.Snapshot("a", seen); !got.Exceeded || got.CostUSD != 100 {
				t.Fatalf("unconfirmed reset reopened budget: %+v", got)
			}
		})
	}
	// Reset-only headers must not erase the older percentage baseline before
	// the detailed observation for that same new window arrives.
	s := testStore(t)
	if err := s.ObserveWeeklyQuota("a", oldEnd, now, now, 85, true); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordAt("a", 100, now, now); err != nil {
		t.Fatal(err)
	}
	if err := s.ObserveWindow("a", newEnd, seen, seen); err != nil {
		t.Fatal(err)
	}
	if err := s.ObserveWeeklyQuota("a", newEnd, seen, seen, 0, true); err != nil {
		t.Fatal(err)
	}
	if got := s.Snapshot("a", seen); got.CostUSD != 0 {
		t.Fatalf("detailed reset evidence lost after deadline-only header: %+v", got)
	}
}

func TestUsageLedgerMigrationAndAtomicWrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.sqlite")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`CREATE TABLE auth_usage(auth_index TEXT PRIMARY KEY,limit_usd INTEGER,cost_usd INTEGER,total_cost_usd INTEGER,window_start INTEGER,window_end INTEGER,observed_at INTEGER);
INSERT INTO auth_usage VALUES('a',100,100,100,0,0,0);`)
	if err != nil {
		t.Fatal(err)
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	now := time.Now()
	if got := s.Snapshot("a", now); !got.Exceeded || got.CostUSD != 100 {
		t.Fatalf("upgrade erased existing costs: %+v", got)
	}
	if _, err = s.db.Exec(`CREATE TRIGGER fail_event BEFORE INSERT ON auth_usage_events BEGIN SELECT RAISE(ABORT,'test event failure'); END;`); err != nil {
		t.Fatal(err)
	}
	if err = s.RecordAt("a", 10, now, now); err == nil {
		t.Fatal("event failure ignored")
	}
	state, err := s.read("a")
	if err != nil || state.TotalCostUSD != 100 || state.CostUSD != 100 {
		t.Fatalf("partial billing transaction: %+v, %v", state, err)
	}
	if !s.Snapshot("a", now).Unavailable {
		t.Fatal("event failure did not fail closed")
	}
}

func TestUpstreamWindowAndDelayedRequests(t *testing.T) {
	s := testStore(t)
	now := time.Date(2026, 9, 17, 11, 37, 0, 0, time.UTC)
	reset := now.Add(43 * time.Hour)
	if _, err := s.SetLimit("a", 10, now); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordAt("a", 10, now, now); err != nil {
		t.Fatal(err)
	}
	if state := s.Snapshot("a", now.Add(8*24*time.Hour)); !state.Exceeded || !state.Window.End.IsZero() {
		t.Fatalf("unknown upstream reset invented: %+v", state)
	}
	if err := s.ObserveWindow("a", reset, now, now); err != nil {
		t.Fatal(err)
	}
	if blocked, end := s.Check("a", reset.Add(-time.Nanosecond)); !blocked || !end.Equal(reset) {
		t.Fatalf("inclusive limit not enforced: %v, %v", blocked, end)
	}
	state := s.Snapshot("a", reset)
	if state.Exceeded || state.CostUSD != 0 || state.TotalCostUSD != 10 || !state.Window.End.IsZero() || !state.Window.Start.Equal(reset) {
		t.Fatalf("incorrect rollover: %+v", state)
	}
	// A read must not mutate stored totals or window state.
	stored, err := s.read("a")
	if err != nil || stored.CostUSD != 10 || !stored.Window.End.Equal(reset) {
		t.Fatalf("read mutated storage: %+v %v", stored, err)
	}
	after := reset.Add(time.Minute)
	if err := s.RecordAt("a", 7, now, after); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordAt("a", 3, after, after); err != nil {
		t.Fatal(err)
	}
	if err := s.ObserveWindow("a", reset, after, after); err != nil {
		t.Fatal(err)
	}
	state = s.Snapshot("a", after)
	if state.CostUSD != 3 || state.TotalCostUSD != 20 || !state.Window.End.IsZero() {
		t.Fatalf("delayed request or repeated reset corrupted budget: %+v", state)
	}
	nextReset := reset.Add(7 * 24 * time.Hour)
	if err := s.ObserveWindow("a", nextReset, after, after); err != nil {
		t.Fatal(err)
	}
	if err := s.ObserveWindow("a", nextReset.Add(-time.Hour), now, after); err != nil {
		t.Fatal(err)
	}
	state = s.Snapshot("a", after)
	if state.CostUSD != 3 || !state.Window.End.Equal(nextReset) {
		t.Fatalf("older observation changed reset or costs: %+v", state)
	}
	// A newer upstream correction changes the deadline without giving free usage.
	if err := s.ObserveWindow("a", nextReset.Add(time.Hour), after.Add(time.Second), after); err != nil {
		t.Fatal(err)
	}
	state = s.Snapshot("a", after)
	if state.CostUSD != 3 || !state.Window.End.Equal(nextReset.Add(time.Hour)) {
		t.Fatalf("new deadline cleared costs: %+v", state)
	}
}

func TestPersistenceConcurrencyAndUnknownJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.sqlite")
	s, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)
	if _, err = s.SetLimit("a", 100, now); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 50 {
		wg.Go(func() {
			if errRecord := s.RecordAt("a", 2, now, now); errRecord != nil {
				t.Errorf("record: %v", errRecord)
			}
		})
	}
	wg.Wait()
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	state := s.Snapshot("a", now)
	if state.CostUSD != 100 || state.TotalCostUSD != 100 || state.LimitUSD != 100 || !state.Exceeded {
		t.Fatalf("lost concurrent or persisted costs: %+v", state)
	}
	data, err := json.Marshal(state)
	if err != nil || !strings.Contains(string(data), `"window":{"start":null,"end":null}`) {
		t.Fatalf("unknown window JSON: %s %v", data, err)
	}
	if _, err = s.SetLimit("a", 0, now); err != nil {
		t.Fatal(err)
	}
	if blocked, _ := s.Check("a", now); blocked {
		t.Fatal("zero limit must remove cap without deleting usage")
	}
	if err = s.RecordAt("a", billing.USD(1<<63-1), now, now); err != nil {
		t.Fatal(err)
	}
	if got := s.Snapshot("a", now).CostUSD; got != billing.USD(1<<63-1) {
		t.Fatalf("overflow wrapped: %d", got)
	}
}

func TestStorageFailuresFailClosed(t *testing.T) {
	s := testStore(t)
	now := time.Now()
	if _, err := s.SetLimit("a", 100, now); err != nil {
		t.Fatal(err)
	}
	// Simulate a broken DB underneath a live store rather than intentional Close.
	if err := s.db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordAt("a", 1, now, now); err == nil {
		t.Fatal("write failure hidden")
	}
	if blocked, _ := s.Check("a", now); !blocked {
		t.Fatal("failed storage reopened credential")
	}
	if !s.Snapshot("a", now).Unavailable {
		t.Fatal("storage failure not exposed")
	}
	path := filepath.Join(t.TempDir(), "broken.sqlite")
	if err := os.WriteFile(path, []byte("not sqlite"), 0600); err != nil {
		t.Fatal(err)
	}
	if broken, err := NewStore(path); err == nil {
		_ = broken.Close()
		t.Fatal("corrupt storage accepted")
	}
}
