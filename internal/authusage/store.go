// Package authusage persists per-credential dollar accounting independently of client statistics.
package authusage

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/billing"
	_ "modernc.org/sqlite"
)

type Window struct {
	Start time.Time `json:"start"`
	End   time.Time `json:"end"`
}

func (w Window) MarshalJSON() ([]byte, error) {
	var start, end *time.Time
	if !w.Start.IsZero() {
		start = &w.Start
	}
	if !w.End.IsZero() {
		end = &w.End
	}
	return json.Marshal(struct {
		Start *time.Time `json:"start"`
		End   *time.Time `json:"end"`
	}{start, end})
}

type State struct {
	AuthIndex    string      `json:"auth_index"`
	Currency     string      `json:"currency"`
	CostUSD      billing.USD `json:"cost_usd"`
	TotalCostUSD billing.USD `json:"total_cost_usd"`
	LimitUSD     billing.USD `json:"limit_usd"`
	RemainingUSD billing.USD `json:"remaining_usd"`
	Window       Window      `json:"window"`
	Exceeded     bool        `json:"exceeded"`
	Unavailable  bool        `json:"unavailable"`
	observedAt   time.Time
	quotaResetAt time.Time
	quotaSeenAt  time.Time
	quotaUsed    float64
}

// Store serializes accounting transactions and latches storage errors. An error
// cannot silently turn an exhausted or unrecorded budget into an available one.
type Store struct {
	mu      sync.Mutex
	db      *sql.DB
	failure error
}

func NewStore(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, fmt.Errorf("create auth usage directory: %w", err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err = db.Exec(`PRAGMA busy_timeout=5000;
PRAGMA journal_mode=WAL;
PRAGMA synchronous=FULL;
CREATE TABLE IF NOT EXISTS auth_usage (
 auth_index TEXT PRIMARY KEY,
 limit_usd INTEGER NOT NULL DEFAULT 0 CHECK(limit_usd >= 0),
 cost_usd INTEGER NOT NULL DEFAULT 0 CHECK(cost_usd >= 0),
 total_cost_usd INTEGER NOT NULL DEFAULT 0 CHECK(total_cost_usd >= 0),
 window_start INTEGER NOT NULL DEFAULT 0,
 window_end INTEGER NOT NULL DEFAULT 0,
 observed_at INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS auth_usage_quota (
 auth_index TEXT PRIMARY KEY,
 reset_at INTEGER NOT NULL,
 observed_at INTEGER NOT NULL,
 used_percent REAL NOT NULL CHECK(used_percent >= 0 AND used_percent <= 100)
);
CREATE TABLE IF NOT EXISTS auth_usage_events (
 id INTEGER PRIMARY KEY,
 auth_index TEXT NOT NULL,
 requested_at INTEGER NOT NULL,
 cost_usd INTEGER NOT NULL CHECK(cost_usd > 0)
);
CREATE INDEX IF NOT EXISTS auth_usage_events_window ON auth_usage_events(auth_index,requested_at);`); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("initialize auth usage: %w", err)
	}
	// Check the schema and contents before serving any requests.
	rows, err := db.Query(`SELECT auth_index, limit_usd, cost_usd, total_cost_usd, window_start, window_end, observed_at FROM auth_usage`)
	if err == nil {
		for rows.Next() {
			var index string
			var limit, cost, total, start, end, observed int64
			if err = rows.Scan(&index, &limit, &cost, &total, &start, &end, &observed); err != nil {
				break
			}
			if index == "" || limit < 0 || cost < 0 || total < cost || (end != 0 && start >= end) {
				err = errors.New("invalid auth usage row")
				break
			}
		}
		if err == nil {
			err = rows.Err()
		}
		_ = rows.Close()
	}
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("validate auth usage: %w", err)
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failure = errors.New("auth usage store closed")
	return s.db.Close()
}

func newState(index string) State { return State{AuthIndex: strings.TrimSpace(index), Currency: "USD"} }

func (s *Store) read(index string) (State, error) {
	state := newState(index)
	var start, end, observed, quotaReset, quotaSeen int64
	err := s.db.QueryRow(`SELECT u.limit_usd,u.cost_usd,u.total_cost_usd,u.window_start,u.window_end,u.observed_at,
COALESCE(q.reset_at,0),COALESCE(q.observed_at,0),COALESCE(q.used_percent,0)
FROM auth_usage u LEFT JOIN auth_usage_quota q ON q.auth_index=u.auth_index WHERE u.auth_index=?`, state.AuthIndex).
		Scan(&state.LimitUSD, &state.CostUSD, &state.TotalCostUSD, &start, &end, &observed, &quotaReset, &quotaSeen, &state.quotaUsed)
	if errors.Is(err, sql.ErrNoRows) {
		return state, nil
	}
	if start != 0 {
		state.Window.Start = time.Unix(start, 0).UTC()
	}
	if end != 0 {
		state.Window.End = time.Unix(end, 0).UTC()
	}
	if observed != 0 {
		state.observedAt = time.Unix(0, observed).UTC()
	}
	if quotaSeen != 0 {
		state.quotaResetAt = time.Unix(quotaReset, 0).UTC()
		state.quotaSeenAt = time.Unix(0, quotaSeen).UTC()
	}
	return state, err
}

type costEvent struct {
	requestedAt time.Time
	cost        billing.USD
}

func (s *Store) write(state State, event *costEvent, now time.Time) error {
	var start, end, observed int64
	if !state.Window.Start.IsZero() {
		start = state.Window.Start.Unix()
	}
	if !state.Window.End.IsZero() {
		end = state.Window.End.Unix()
	}
	if !state.observedAt.IsZero() {
		observed = state.observedAt.UnixNano()
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	_, err = tx.Exec(`INSERT INTO auth_usage(auth_index,limit_usd,cost_usd,total_cost_usd,window_start,window_end,observed_at) VALUES(?,?,?,?,?,?,?)
ON CONFLICT(auth_index) DO UPDATE SET limit_usd=excluded.limit_usd,cost_usd=excluded.cost_usd,total_cost_usd=excluded.total_cost_usd,window_start=excluded.window_start,window_end=excluded.window_end,observed_at=excluded.observed_at`,
		state.AuthIndex, state.LimitUSD, state.CostUSD, state.TotalCostUSD, start, end, observed)
	if err != nil {
		return err
	}
	if !state.quotaSeenAt.IsZero() {
		if _, err = tx.Exec(`INSERT INTO auth_usage_quota(auth_index,reset_at,observed_at,used_percent) VALUES(?,?,?,?)
ON CONFLICT(auth_index) DO UPDATE SET reset_at=excluded.reset_at,observed_at=excluded.observed_at,used_percent=excluded.used_percent`,
			state.AuthIndex, state.quotaResetAt.Unix(), state.quotaSeenAt.UnixNano(), state.quotaUsed); err != nil {
			return err
		}
	}
	if event != nil {
		if _, err = tx.Exec(`INSERT INTO auth_usage_events(auth_index,requested_at,cost_usd) VALUES(?,?,?)`, state.AuthIndex, event.requestedAt.UnixNano(), event.cost); err != nil {
			return err
		}
	}
	// Only the last week can belong to an early-reset window observed now.
	// Bound cleanup work so a long-idle credential cannot stall a request.
	if _, err = tx.Exec(`DELETE FROM auth_usage_events WHERE id IN
(SELECT id FROM auth_usage_events WHERE auth_index=? AND requested_at<? LIMIT 1000)`, state.AuthIndex, now.Add(-7*24*time.Hour).UnixNano()); err != nil {
		return err
	}
	return tx.Commit()
}

func finishState(state State) State {
	state.Exceeded = state.LimitUSD > 0 && state.CostUSD >= state.LimitUSD
	if state.LimitUSD > state.CostUSD {
		state.RemainingUSD = state.LimitUSD - state.CostUSD
	} else {
		state.RemainingUSD = 0
	}
	return state
}

func (s *Store) Snapshot(index string, now time.Time) State {
	if s == nil {
		state := newState(index)
		state.Unavailable = true
		return state
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	state, err := s.read(index)
	if err != nil {
		s.failure = err
	}
	state.Unavailable = s.failure != nil
	return finishState(advanceWindow(state, now))
}

func (s *Store) SetLimit(index string, limit billing.USD, now time.Time) (State, error) {
	return s.update(index, now, func(state *State) error {
		if limit < 0 {
			return errors.New("auth USD limit must be non-negative")
		}
		state.LimitUSD = limit
		return nil
	})
}

func (s *Store) Record(index string, cost billing.USD, requestedAt time.Time) error {
	return s.RecordAt(index, cost, requestedAt, time.Now())
}

// RecordAt attributes completed requests to their starting window, even if an
// upstream stream finishes after a reset. Lifetime usage always includes them.
func (s *Store) RecordAt(index string, cost billing.USD, requestedAt, now time.Time) error {
	if cost <= 0 || strings.TrimSpace(index) == "" {
		return nil
	}
	if requestedAt.IsZero() {
		requestedAt = now
	}
	_, err := s.updateWithEvent(index, now, &costEvent{requestedAt: requestedAt, cost: cost}, func(state *State) error {
		state.TotalCostUSD = billing.Add(state.TotalCostUSD, cost)
		if state.Window.Start.IsZero() || !requestedAt.Before(state.Window.Start) {
			state.CostUSD = billing.Add(state.CostUSD, cost)
		}
		return nil
	})
	return err
}

func (s *Store) update(index string, now time.Time, mutate func(*State) error) (State, error) {
	return s.updateWithEvent(index, now, nil, mutate)
}

func (s *Store) updateWithEvent(index string, now time.Time, event *costEvent, mutate func(*State) error) (State, error) {
	if s == nil {
		return newState(index), errors.New("auth usage store unavailable")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failure != nil {
		return State{Unavailable: true}, s.failure
	}
	if strings.TrimSpace(index) == "" {
		return State{}, errors.New("auth index is required")
	}
	state, err := s.read(index)
	if err != nil {
		s.failure = err
		return State{Unavailable: true}, err
	}
	previous := state
	state = advanceWindow(state, now)
	if err = mutate(&state); err != nil {
		return finishState(state), err
	}
	if state == previous && event == nil {
		return finishState(state), nil
	}
	if err = s.write(state, event, now); err != nil {
		s.failure = err
		state.Unavailable = true
	}
	return finishState(state), err
}

func (s *Store) Check(index string, now time.Time) (bool, time.Time) {
	if s == nil {
		return false, time.Time{}
	}
	state := s.Snapshot(index, now)
	return state.Unavailable || state.Exceeded, state.Window.End
}

// advanceWindow consumes an observed upstream reset once. The next reset stays
// unknown until upstream reports it; no calendar or interval is invented.
func advanceWindow(state State, now time.Time) State {
	if !state.Window.End.IsZero() && !now.Before(state.Window.End) {
		state.Window.Start = state.Window.End
		state.Window.End = time.Time{}
		state.CostUSD = 0
	}
	return state
}

// ObserveWindow records upstream's reset without clearing already billed usage.
// Older observations and resets already consumed can never reopen a budget.
func (s *Store) ObserveWindow(index string, resetAt, observedAt, now time.Time) error {
	return s.ObserveWeeklyQuota(index, resetAt, observedAt, now, 0, false)
}

// ObserveWeeklyQuota confirms an early new cycle only when upstream usage falls
// and the new seven-day window began after the previous usage observation.
// A percentage correction or a changed deadline alone cannot erase spending.
func (s *Store) ObserveWeeklyQuota(index string, resetAt, observedAt, now time.Time, usedPercent float64, usedKnown bool) error {
	if resetAt.IsZero() || observedAt.IsZero() {
		return nil
	}
	resetAt = resetAt.UTC().Truncate(time.Second)
	usedKnown = usedKnown && !math.IsNaN(usedPercent) && !math.IsInf(usedPercent, 0) && usedPercent >= 0 && usedPercent <= 100
	_, err := s.update(index, now, func(state *State) error {
		// A reset-only header may arrive before a detailed observation of the
		// same window. Permit the latter to supply missing usage evidence without
		// accepting an older deadline or replaying a detailed observation.
		newDetail := usedKnown && observedAt.After(state.quotaSeenAt)
		if !resetAt.After(state.Window.Start) || (!observedAt.After(state.observedAt) &&
			!(newDetail && resetAt.Equal(state.Window.End))) {
			return nil
		}
		// A stale reset must not clear a budget accrued while no reset was known.
		if !resetAt.After(now) {
			return nil
		}
		if usedKnown && observedAt.After(state.quotaSeenAt) {
			cycleStart := resetAt.Add(-7 * 24 * time.Hour)
			if !state.quotaSeenAt.IsZero() && usedPercent < state.quotaUsed &&
				resetAt.After(state.quotaResetAt) && cycleStart.After(state.quotaSeenAt) &&
				cycleStart.After(state.Window.Start) && !cycleStart.After(observedAt) && !cycleStart.After(now) {
				cost, errSum := s.costSince(state.AuthIndex, cycleStart)
				if errSum != nil {
					s.failure = errSum
					return errSum
				}
				state.Window.Start = cycleStart
				state.CostUSD = cost
			}
			state.quotaResetAt = resetAt
			state.quotaSeenAt = observedAt.UTC()
			state.quotaUsed = usedPercent
		}
		state.Window.End = resetAt
		if observedAt.After(state.observedAt) {
			state.observedAt = observedAt.UTC()
		}
		return nil
	})
	return err
}

func (s *Store) costSince(index string, start time.Time) (billing.USD, error) {
	rows, err := s.db.Query(`SELECT cost_usd FROM auth_usage_events WHERE auth_index=? AND requested_at>=?`, index, start.UnixNano())
	if err != nil {
		return 0, err
	}
	defer func() { _ = rows.Close() }()
	var total billing.USD
	for rows.Next() {
		var cost billing.USD
		if err = rows.Scan(&cost); err != nil {
			return 0, err
		}
		total = billing.Add(total, cost)
	}
	return total, rows.Err()
}

var defaultMu sync.RWMutex
var defaultStore *Store

// SetDefault installs a store and returns a restoration closure for SDK hosts and tests.
func SetDefault(store *Store) func() {
	defaultMu.Lock()
	previous := defaultStore
	defaultStore = store
	defaultMu.Unlock()
	return func() { defaultMu.Lock(); defaultStore = previous; defaultMu.Unlock() }
}

func current() *Store                            { defaultMu.RLock(); defer defaultMu.RUnlock(); return defaultStore }
func Snapshot(index string, now time.Time) State { return current().Snapshot(index, now) }
func SetLimit(index string, limit billing.USD, now time.Time) (State, error) {
	return current().SetLimit(index, limit, now)
}
func Check(index string, now time.Time) (bool, time.Time) { return current().Check(index, now) }
func Record(index string, cost billing.USD, requestedAt time.Time) error {
	store := current()
	if store == nil {
		return nil
	}
	return store.Record(index, cost, requestedAt)
}
func ObserveWindow(index string, resetAt, observedAt, now time.Time) error {
	store := current()
	if store == nil {
		return nil
	}
	return store.ObserveWindow(index, resetAt, observedAt, now)
}

func ObserveWeeklyQuota(index string, resetAt, observedAt, now time.Time, usedPercent float64, usedKnown bool) error {
	store := current()
	if store == nil {
		return nil
	}
	return store.ObserveWeeklyQuota(index, resetAt, observedAt, now, usedPercent, usedKnown)
}
