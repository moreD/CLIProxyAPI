package redisqueue

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/billing"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	log "github.com/sirupsen/logrus"
	_ "modernc.org/sqlite"
)

const (
	defaultUsageStatsPersistenceDir = "usage-stats"
	usageStatsPersistenceInterval   = 5 * time.Minute
	usageStatsHistoryDBFileName     = "usage-stats.sqlite"
)

type UsageStatsSnapshot struct {
	Currency           string                      `json:"currency"`
	GeneratedAt        time.Time                   `json:"generated_at"`
	Windows            ClientUsageWindows          `json:"windows"`
	APIKeys            []ClientUsageStat           `json:"api_keys"`
	HistoricalSevenDay []ClientUsageHistoricalStat `json:"historical_7d,omitempty"`
}

type ClientUsageStat struct {
	APIKey     string                `json:"api_key"`
	TwelveHour ClientUsageWindowStat `json:"12h"`
	SevenDay   ClientUsageWindowStat `json:"7d"`
	Limits     ClientCostLimits      `json:"limits,omitempty"`
}

type ClientUsageHistoricalStat struct {
	APIKey   string                `json:"api_key"`
	Window   UsageWindowInfo       `json:"window"`
	SevenDay ClientUsageWindowStat `json:"7d"`
}

type ClientUsageWindows struct {
	TwelveHour UsageWindowInfo `json:"12h"`
	SevenDay   UsageWindowInfo `json:"7d"`
}

type UsageWindowInfo struct {
	Start time.Time `json:"start"`
	End   time.Time `json:"end"`
}

type ClientUsageWindowStat struct {
	CostUSD       billing.USD         `json:"cost_usd"`
	RequestCount  int64               `json:"request_count"`
	SuccessCount  int64               `json:"success_count"`
	FailureCount  int64               `json:"failure_count"`
	FirstRequest  time.Time           `json:"first_request_at"`
	LastRequest   time.Time           `json:"last_request_at"`
	LatencyMs     int64               `json:"latency_ms"`
	Tokens        usageStatTokens     `json:"tokens"`
	ProviderStats []ProviderUsageStat `json:"provider_stats"`
}

type ProviderUsageStat struct {
	CostUSD           billing.USD     `json:"cost_usd"`
	BillingVersion    int             `json:"billing_version"`
	SessionAffinityID string          `json:"session_affinity_id,omitempty"`
	Provider          string          `json:"provider"`
	Model             string          `json:"model"`
	Alias             string          `json:"alias"`
	Endpoint          string          `json:"endpoint"`
	RequestCount      int64           `json:"request_count"`
	SuccessCount      int64           `json:"success_count"`
	FailureCount      int64           `json:"failure_count"`
	FirstRequest      time.Time       `json:"first_request_at"`
	LastRequest       time.Time       `json:"last_request_at"`
	LatencyMs         int64           `json:"latency_ms"`
	Tokens            usageStatTokens `json:"tokens"`
}

type usageStatsEvent struct {
	CostUSD           billing.USD     `json:"cost_usd"`
	BillingVersion    int             `json:"billing_version"`
	ServiceTier       string          `json:"service_tier,omitempty"`
	Timestamp         time.Time       `json:"timestamp"`
	APIKey            string          `json:"api_key"`
	SessionAffinityID string          `json:"session_affinity_id,omitempty"`
	Provider          string          `json:"provider"`
	Model             string          `json:"model"`
	Alias             string          `json:"alias"`
	Endpoint          string          `json:"endpoint"`
	LatencyMs         int64           `json:"latency_ms"`
	Tokens            usageStatTokens `json:"tokens"`
	Failed            bool            `json:"failed"`
}

type usageStatTokens struct {
	CacheWriteTokens int64 `json:"cache_write_tokens,omitempty"`
	ReadTokens       int64 `json:"read_tokens"`
	WriteTokens      int64 `json:"write_tokens"`
	ReasoningTokens  int64 `json:"reasoning_tokens"`
	CacheReadTokens  int64 `json:"cache_read_tokens"`
	TotalTokens      int64 `json:"total_tokens"`
}

type usageStatsStore struct {
	mu         sync.Mutex
	persistMu  sync.Mutex
	events     []usageStatsEvent
	windows    ClientUsageWindows
	aggregates map[string]*clientUsageAggregate
	history    map[string][]ClientUsageHistoricalStat
}

type clientUsageAggregate struct {
	TwelveHour ClientUsageWindowStat
	SevenDay   ClientUsageWindowStat
}

type ClientCostLimits struct {
	TwelveHour billing.USD `json:"12h,omitempty"`
	SevenDay   billing.USD `json:"7d,omitempty"`
}

type ClientUsageLimitDecision struct {
	Unavailable bool        `json:"unavailable,omitempty"`
	Exceeded    bool        `json:"exceeded"`
	Window      string      `json:"window,omitempty"`
	Used        billing.USD `json:"used,omitempty"`
	Limit       billing.USD `json:"limit,omitempty"`
	ResetsAt    time.Time   `json:"resets_at,omitempty"`
}

type usageStatsPersistedFile struct {
	APIKey string            `json:"api_key"`
	Events []usageStatsEvent `json:"events,omitempty"`
}

var (
	usageStatsRecoveryFailed  atomic.Bool
	globalUsageStats          usageStatsStore
	usageStatsPersistenceDir  = defaultUsageStatsPersistenceDir
	usageStatsHistoryDBPath   = defaultUsageStatsHistoryDBPath()
	usageStatsPersistenceOnce sync.Once
	clientCostLimitsMu        sync.RWMutex
	clientCostLimits          = make(map[string]ClientCostLimits)
)

func RecordUsageStat(detail queuedUsageDetail) {
	if !usageStatsTrackingEnabled() {
		return
	}

	timestamp := detail.Timestamp
	if timestamp.IsZero() {
		timestamp = time.Now()
	}

	event := usageStatsEvent{
		ServiceTier:       detail.ServiceTier,
		Timestamp:         timestamp,
		APIKey:            strings.TrimSpace(detail.APIKey),
		SessionAffinityID: strings.TrimSpace(detail.SessionAffinityID),
		Provider:          strings.TrimSpace(detail.Provider),
		Model:             strings.TrimSpace(detail.Model),
		Alias:             strings.TrimSpace(detail.Alias),
		Endpoint:          strings.TrimSpace(detail.Endpoint),
		LatencyMs:         detail.LatencyMs,
		Tokens:            usageStatTokensFromQueueTokens(detail.Tokens),
		Failed:            detail.Failed,
	}
	if tier := strings.TrimSpace(detail.ResponseServiceTier); tier != "" && tier != "auto" {
		event.ServiceTier = tier
	}
	// Upstream v2 provides disjoint input/output buckets, including reasoning once.
	if b := detail.TokenBreakdown; b.Valid() && b.UnclassifiedTokens == 0 {
		event.Tokens = usageStatTokens{ReadTokens: b.Input.TotalTokens, WriteTokens: b.Output.TotalTokens, ReasoningTokens: b.Output.ReasoningTokens, CacheReadTokens: b.Input.CacheReadTokens, CacheWriteTokens: b.Input.CacheWriteTokens, TotalTokens: b.TotalTokens}
		event.BillingVersion = 1
		event.CostUSD = eventCost(event, true)
	}

	globalUsageStats.add(event, time.Now())
}

func StartUsageStatsPersistence(ctx context.Context) {
	if ctx == nil {
		ctx = context.Background()
	}
	usageStatsPersistenceOnce.Do(func() {
		if usageStatsTrackingEnabled() {
			if err := globalUsageStats.recoverFromDisk(time.Now()); err != nil {
				usageStatsRecoveryFailed.Store(true)
				log.Errorf("client usage recovery failed; quota traffic and persistence are disabled until data is repaired and CPA restarted: %v", err)
				return
			} else {
				usageStatsRecoveryFailed.Store(false)
				log.Infof("client usage stats recovered from %s", usageStatsPersistenceDir)
			}
		}
		go runUsageStatsPersistenceLoop(ctx)
		log.Infof("client usage stats persistence started (interval=%s)", usageStatsPersistenceInterval)
	})
}

// FlushUsageStats persists the current per-API-key usage state immediately.
func FlushUsageStats() error {
	return globalUsageStats.persistToDisk(time.Now())
}

func UsageStatsSnapshotNow() UsageStatsSnapshot {
	return globalUsageStats.snapshot(time.Now())
}

func SetClientCostLimits(entries []config.APIKeyEntry) {
	next := make(map[string]ClientCostLimits)
	for _, entry := range config.NormalizeAPIKeyEntries(entries) {
		limits := ClientCostLimits{
			TwelveHour: entry.CostLimits.TwelveHour,
			SevenDay:   entry.CostLimits.SevenDay,
		}
		if limits.TwelveHour <= 0 && limits.SevenDay <= 0 {
			continue
		}
		next[entry.APIKey] = limits
	}

	clientCostLimitsMu.Lock()
	clientCostLimits = next
	clientCostLimitsMu.Unlock()
	if len(next) == 0 && !UsageStatisticsEnabled() {
		ClearUsageStats()
	}
}

func CheckClientCostLimit(apiKey string, now time.Time) ClientUsageLimitDecision {
	apiKey = normalizedUsageStatsAPIKey(apiKey)

	clientCostLimitsMu.RLock()
	limits := clientCostLimits[apiKey]
	clientCostLimitsMu.RUnlock()
	if limits.TwelveHour <= 0 && limits.SevenDay <= 0 {
		return ClientUsageLimitDecision{}
	}
	if usageStatsRecoveryFailed.Load() {
		return ClientUsageLimitDecision{Exceeded: true, Unavailable: true}
	}

	return globalUsageStats.checkLimit(apiKey, limits, now)
}

func ClearUsageStats() {
	globalUsageStats.clear()
}

func (s *usageStatsStore) add(event usageStatsEvent, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.ensureWindowsLocked(now)
	event.APIKey = normalizedUsageStatsAPIKey(event.APIKey)
	event = normalizeBilledEvent(event)
	s.events = append(s.events, event)
	if isWithinWindow(event.Timestamp, s.windows.TwelveHour) {
		s.addToAggregateWindowLocked(event.APIKey, event, "12h")
	}
	if isWithinWindow(event.Timestamp, s.windows.SevenDay) {
		s.addToAggregateWindowLocked(event.APIKey, event, "7d")
	}
}

func (s *usageStatsStore) snapshot(now time.Time) UsageStatsSnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.ensureWindowsLocked(now)

	out := UsageStatsSnapshot{
		Currency:           "USD",
		GeneratedAt:        now,
		Windows:            s.windows,
		APIKeys:            make([]ClientUsageStat, 0, len(s.aggregates)),
		HistoricalSevenDay: s.historicalSevenDayStatsLocked(),
	}
	limits := clientCostLimitsSnapshot()
	for apiKey, aggregate := range s.aggregates {
		stat := ClientUsageStat{
			APIKey:     apiKey,
			TwelveHour: cloneClientUsageWindowStat(aggregate.TwelveHour),
			SevenDay:   cloneClientUsageWindowStat(aggregate.SevenDay),
			Limits:     limits[apiKey],
		}
		sortProviderUsageStats(stat.TwelveHour.ProviderStats)
		sortProviderUsageStats(stat.SevenDay.ProviderStats)
		out.APIKeys = append(out.APIKeys, stat)
	}
	sort.Slice(out.APIKeys, func(i, j int) bool {
		left := out.APIKeys[i]
		right := out.APIKeys[j]
		if left.SevenDay.CostUSD != right.SevenDay.CostUSD {
			return left.SevenDay.CostUSD > right.SevenDay.CostUSD
		}
		if !left.SevenDay.LastRequest.Equal(right.SevenDay.LastRequest) {
			return left.SevenDay.LastRequest.After(right.SevenDay.LastRequest)
		}
		return left.APIKey < right.APIKey
	})

	return out
}

func (s *usageStatsStore) clear() {
	s.mu.Lock()
	s.events = nil
	s.windows = ClientUsageWindows{}
	s.aggregates = nil
	s.history = nil
	s.mu.Unlock()
}

func (s *usageStatsStore) checkLimit(apiKey string, limits ClientCostLimits, now time.Time) ClientUsageLimitDecision {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.ensureWindowsLocked(now)
	aggregate := s.aggregates[apiKey]
	if aggregate == nil {
		return ClientUsageLimitDecision{}
	}
	if limits.TwelveHour > 0 && aggregate.TwelveHour.CostUSD >= limits.TwelveHour {
		return ClientUsageLimitDecision{
			Exceeded: true,
			Window:   "12h",
			Used:     aggregate.TwelveHour.CostUSD,
			Limit:    limits.TwelveHour,
			ResetsAt: s.windows.TwelveHour.End,
		}
	}
	if limits.SevenDay > 0 && aggregate.SevenDay.CostUSD >= limits.SevenDay {
		return ClientUsageLimitDecision{
			Exceeded: true,
			Window:   "7d",
			Used:     aggregate.SevenDay.CostUSD,
			Limit:    limits.SevenDay,
			ResetsAt: s.windows.SevenDay.End,
		}
	}
	return ClientUsageLimitDecision{}
}

func runUsageStatsPersistenceLoop(ctx context.Context) {
	ticker := time.NewTicker(usageStatsPersistenceInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			if err := globalUsageStats.persistToDisk(time.Now()); err != nil {
				log.Warnf("failed to persist client usage stats during shutdown: %v", err)
			}
			return
		case <-ticker.C:
			if !usageStatsTrackingEnabled() {
				continue
			}
			if err := globalUsageStats.persistToDisk(time.Now()); err != nil {
				log.Warnf("failed to persist client usage stats: %v", err)
			}
		}
	}
}

func (s *usageStatsStore) persistToDisk(now time.Time) error {
	if s == &globalUsageStats && usageStatsRecoveryFailed.Load() {
		return fmt.Errorf("usage persistence disabled after recovery failure")
	}
	s.persistMu.Lock()
	defer s.persistMu.Unlock()

	s.mu.Lock()
	s.ensureWindowsLocked(now)
	events := append([]usageStatsEvent(nil), s.events...)
	history := s.historicalSevenDayStatsLocked()
	s.mu.Unlock()

	if err := writeUsageStatsEvents(usageStatsPersistenceDir, events); err != nil {
		return err
	}
	if err := writeUsageStatsHistory(usageStatsHistoryDBPath, history); err != nil {
		return err
	}
	return nil
}

func (s *usageStatsStore) recoverFromDisk(now time.Time) error {
	s.persistMu.Lock()
	defer s.persistMu.Unlock()

	events, err := readUsageStatsEvents(usageStatsPersistenceDir)
	if err != nil {
		return err
	}
	history, err := readUsageStatsHistory(usageStatsHistoryDBPath)
	if err != nil {
		return err
	}

	sort.SliceStable(events, func(i, j int) bool {
		return events[i].Timestamp.Before(events[j].Timestamp)
	})

	s.mu.Lock()
	s.events = events
	s.history = makeUsageStatsHistoryMap(history)
	s.windows = currentUsageStatsWindows(now)
	s.captureClosedSevenDayStatsFromEventsLocked()
	s.pruneLocked()
	s.rebuildAggregatesLocked()
	events = append([]usageStatsEvent(nil), s.events...)
	history = s.historicalSevenDayStatsLocked()
	s.mu.Unlock()
	if err = writeUsageStatsEvents(usageStatsPersistenceDir, events); err != nil {
		return err
	}
	if err = writeUsageStatsHistory(usageStatsHistoryDBPath, history); err != nil {
		return err
	}
	return nil
}

func writeUsageStatsEvents(dir string, events []usageStatsEvent) error {
	return writeUsageStatsEventsWithCleanup(dir, events, true)
}

func writeUsageStatsEventsWithCleanup(dir string, events []usageStatsEvent, cleanup bool) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create stats directory: %w", err)
	}

	byAPIKey := make(map[string][]usageStatsEvent)
	for _, event := range events {
		apiKey := normalizedUsageStatsAPIKey(event.APIKey)
		event.APIKey = apiKey
		byAPIKey[apiKey] = append(byAPIKey[apiKey], event)
	}

	written := make(map[string]struct{}, len(byAPIKey))
	for apiKey, apiKeyEvents := range byAPIKey {
		name := usageStatsFileName(apiKey)
		path := filepath.Join(dir, name)
		payload := usageStatsPersistedFile{
			APIKey: apiKey,
			Events: apiKeyEvents,
		}
		if err := writeUsageStatsFile(path, payload); err != nil {
			return err
		}
		written[name] = struct{}{}
	}
	if !cleanup {
		return nil
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("read stats directory: %w", err)
	}
	for _, entry := range entries {
		if entry.IsDir() || isUsageStatsTempFile(entry.Name()) || isUsageStatsHistoryFile(dir, entry.Name()) {
			continue
		}
		if _, ok := written[entry.Name()]; ok {
			continue
		}
		// Only prune a verified per-key snapshot, never an unrelated artifact.
		data, readErr := os.ReadFile(filepath.Join(dir, entry.Name()))
		var previous usageStatsPersistedFile
		if readErr != nil || json.Unmarshal(data, &previous) != nil || previous.APIKey == "" || usageStatsFileName(previous.APIKey) != entry.Name() {
			continue
		}
		if err := os.Remove(filepath.Join(dir, entry.Name())); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove stale stats file %q: %w", entry.Name(), err)
		}
	}

	return nil
}

func writeUsageStatsFile(path string, payload usageStatsPersistedFile) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal stats file %q: %w", path, err)
	}

	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return fmt.Errorf("create temp stats file %q: %w", path, err)
	}
	tmpName := tmp.Name()
	defer func() {
		if errRemove := os.Remove(tmpName); errRemove != nil && !os.IsNotExist(errRemove) {
			log.Debugf("failed to remove temporary stats file %q: %v", tmpName, errRemove)
		}
	}()

	if _, err = tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temp stats file %q: %w", tmpName, err)
	}
	if err = tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("chmod temp stats file %q: %w", tmpName, err)
	}
	if err = tmp.Close(); err != nil {
		return fmt.Errorf("close temp stats file %q: %w", tmpName, err)
	}
	if err = os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("replace stats file %q: %w", path, err)
	}
	return nil
}

func readUsageStatsEvents(dir string) ([]usageStatsEvent, error) {
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read stats directory: %w", err)
	}

	var events []usageStatsEvent
	for _, entry := range entries {
		if entry.IsDir() || isUsageStatsTempFile(entry.Name()) || isUsageStatsHistoryFile(dir, entry.Name()) {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read stats file %q: %w", entry.Name(), err)
		}
		var payload usageStatsPersistedFile
		if err = json.Unmarshal(data, &payload); err != nil {
			return nil, fmt.Errorf("parse stats file %q: %w", entry.Name(), err)
		}
		apiKey := normalizedUsageStatsAPIKey(payload.APIKey)
		for _, event := range payload.Events {
			event.APIKey = normalizedUsageStatsAPIKey(event.APIKey)
			if event.APIKey == "unknown" {
				event.APIKey = apiKey
			}
			if event.Timestamp.IsZero() {
				continue
			}
			events = append(events, event)
		}
	}
	return events, nil
}

func writeUsageStatsHistory(dbPath string, stats []ClientUsageHistoricalStat) error {
	if len(stats) == 0 {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o700); err != nil {
		return fmt.Errorf("create stats history directory: %w", err)
	}

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return fmt.Errorf("open stats history database: %w", err)
	}
	defer func() {
		if errClose := db.Close(); errClose != nil {
			log.Warnf("failed to close stats history database: %v", errClose)
		}
	}()

	if err = ensureUsageStatsHistorySchema(db); err != nil {
		return err
	}

	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("begin stats history transaction: %w", err)
	}
	defer func() {
		if errRollback := tx.Rollback(); errRollback != nil && errRollback != sql.ErrTxDone {
			log.Debugf("failed to roll back stats history transaction: %v", errRollback)
		}
	}()

	stmt, err := tx.Prepare(`
INSERT INTO usage_7d_window_stats (
	api_key, window_start, window_end,
	request_count, success_count, failure_count,
	first_request_at, last_request_at, latency_ms,
	read_tokens, write_tokens, reasoning_tokens, cache_read_tokens, total_tokens,
	provider_stats_json, updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(api_key, window_start) DO UPDATE SET
	window_end = excluded.window_end,
	request_count = excluded.request_count,
	success_count = excluded.success_count,
	failure_count = excluded.failure_count,
	first_request_at = excluded.first_request_at,
	last_request_at = excluded.last_request_at,
	latency_ms = excluded.latency_ms,
	read_tokens = excluded.read_tokens,
	write_tokens = excluded.write_tokens,
	reasoning_tokens = excluded.reasoning_tokens,
	cache_read_tokens = excluded.cache_read_tokens,
	total_tokens = excluded.total_tokens,
	provider_stats_json = excluded.provider_stats_json,
	updated_at = excluded.updated_at`)
	if err != nil {
		return fmt.Errorf("prepare stats history upsert: %w", err)
	}
	defer func() {
		if errClose := stmt.Close(); errClose != nil {
			log.Debugf("failed to close stats history statement: %v", errClose)
		}
	}()

	updatedAt := time.Now().UTC().Format(time.RFC3339Nano)
	for _, stat := range stats {
		if stat.APIKey == "" || stat.Window.Start.IsZero() || stat.Window.End.IsZero() || stat.SevenDay.RequestCount == 0 {
			continue
		}
		providerStatsJSON, errMarshal := json.Marshal(stat.SevenDay.ProviderStats)
		if errMarshal != nil {
			return fmt.Errorf("marshal provider stats for %q: %w", stat.APIKey, errMarshal)
		}
		if _, err = stmt.Exec(
			normalizedUsageStatsAPIKey(stat.APIKey),
			stat.Window.Start.UTC().Format(time.RFC3339Nano),
			stat.Window.End.UTC().Format(time.RFC3339Nano),
			stat.SevenDay.RequestCount,
			stat.SevenDay.SuccessCount,
			stat.SevenDay.FailureCount,
			stat.SevenDay.FirstRequest.UTC().Format(time.RFC3339Nano),
			stat.SevenDay.LastRequest.UTC().Format(time.RFC3339Nano),
			stat.SevenDay.LatencyMs,
			stat.SevenDay.Tokens.ReadTokens,
			stat.SevenDay.Tokens.WriteTokens,
			stat.SevenDay.Tokens.ReasoningTokens,
			stat.SevenDay.Tokens.CacheReadTokens,
			stat.SevenDay.Tokens.TotalTokens,
			string(providerStatsJSON),
			updatedAt,
		); err != nil {
			return fmt.Errorf("upsert stats history for %q: %w", stat.APIKey, err)
		}
	}

	if err = tx.Commit(); err != nil {
		return fmt.Errorf("commit stats history transaction: %w", err)
	}
	return nil
}

func ensureUsageStatsHistorySchema(db *sql.DB) error {
	_, err := db.Exec(`
CREATE TABLE IF NOT EXISTS usage_7d_window_stats (
	api_key TEXT NOT NULL,
	window_start TEXT NOT NULL,
	window_end TEXT NOT NULL,
	request_count INTEGER NOT NULL,
	success_count INTEGER NOT NULL,
	failure_count INTEGER NOT NULL,
	first_request_at TEXT NOT NULL,
	last_request_at TEXT NOT NULL,
	latency_ms INTEGER NOT NULL,
	read_tokens INTEGER NOT NULL,
	write_tokens INTEGER NOT NULL,
	reasoning_tokens INTEGER NOT NULL,
	cache_read_tokens INTEGER NOT NULL,
	total_tokens INTEGER NOT NULL,
	provider_stats_json TEXT NOT NULL,
	updated_at TEXT NOT NULL,
	PRIMARY KEY (api_key, window_start)
)`)
	if err != nil {
		return fmt.Errorf("create stats history table: %w", err)
	}
	return nil
}

func readUsageStatsHistory(dbPath string) ([]ClientUsageHistoricalStat, error) {
	if dbPath == "" {
		return nil, nil
	}
	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		return nil, nil
	} else if err != nil {
		return nil, fmt.Errorf("stat stats history database: %w", err)
	}

	absolutePath, err := filepath.Abs(dbPath)
	if err != nil {
		return nil, err
	}
	dsn := (&url.URL{Scheme: "file", Path: filepath.ToSlash(absolutePath)}).String() + "?mode=ro"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open stats history database: %w", err)
	}
	defer func() {
		if errClose := db.Close(); errClose != nil {
			log.Warnf("failed to close stats history database: %v", errClose)
		}
	}()

	var tableCount int
	if err = db.QueryRow("SELECT count(*) FROM sqlite_master WHERE type='table' AND name='usage_7d_window_stats'").Scan(&tableCount); err != nil {
		return nil, err
	}
	if tableCount == 0 {
		return nil, nil
	}

	rows, err := db.Query(`
SELECT
	api_key, window_start, window_end,
	request_count, success_count, failure_count,
	first_request_at, last_request_at, latency_ms,
	read_tokens, write_tokens, reasoning_tokens, cache_read_tokens, total_tokens,
	provider_stats_json
FROM usage_7d_window_stats`)
	if err != nil {
		return nil, fmt.Errorf("query stats history: %w", err)
	}
	defer func() {
		if errClose := rows.Close(); errClose != nil {
			log.Debugf("failed to close stats history rows: %v", errClose)
		}
	}()

	var out []ClientUsageHistoricalStat
	for rows.Next() {
		var stat ClientUsageHistoricalStat
		var windowStart, windowEnd, firstRequest, lastRequest, providerStatsJSON string
		if err = rows.Scan(
			&stat.APIKey,
			&windowStart,
			&windowEnd,
			&stat.SevenDay.RequestCount,
			&stat.SevenDay.SuccessCount,
			&stat.SevenDay.FailureCount,
			&firstRequest,
			&lastRequest,
			&stat.SevenDay.LatencyMs,
			&stat.SevenDay.Tokens.ReadTokens,
			&stat.SevenDay.Tokens.WriteTokens,
			&stat.SevenDay.Tokens.ReasoningTokens,
			&stat.SevenDay.Tokens.CacheReadTokens,
			&stat.SevenDay.Tokens.TotalTokens,
			&providerStatsJSON,
		); err != nil {
			return nil, fmt.Errorf("scan stats history row: %w", err)
		}
		if stat.Window.Start, err = parseUsageStatsTime(windowStart); err != nil {
			return nil, fmt.Errorf("parse stats history window start: %w", err)
		}
		if stat.Window.End, err = parseUsageStatsTime(windowEnd); err != nil {
			return nil, fmt.Errorf("parse stats history window end: %w", err)
		}
		if stat.SevenDay.FirstRequest, err = parseUsageStatsTime(firstRequest); err != nil {
			return nil, fmt.Errorf("parse stats history first request: %w", err)
		}
		if stat.SevenDay.LastRequest, err = parseUsageStatsTime(lastRequest); err != nil {
			return nil, fmt.Errorf("parse stats history last request: %w", err)
		}
		if err = json.Unmarshal([]byte(providerStatsJSON), &stat.SevenDay.ProviderStats); err != nil {
			return nil, fmt.Errorf("parse stats history provider stats: %w", err)
		}
		normalizeHistoricalBilling(&stat.SevenDay)
		stat.APIKey = normalizedUsageStatsAPIKey(stat.APIKey)
		out = append(out, stat)
	}
	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate stats history rows: %w", err)
	}
	sortHistoricalSevenDayStats(out)
	return out, nil
}

func isUsageStatsTempFile(name string) bool {
	return strings.HasPrefix(name, ".") && strings.HasSuffix(name, ".tmp")
}

func isUsageStatsHistoryFile(dir, name string) bool {
	// Offline migrations read a copied directory, not the runtime global path.
	if name == usageStatsHistoryDBFileName || name == usageStatsHistoryDBFileName+"-wal" || name == usageStatsHistoryDBFileName+"-shm" || name == usageStatsHistoryDBFileName+"-journal" {
		return true
	}
	candidatePath, errCandidate := filepath.Abs(filepath.Join(dir, name))
	if errCandidate != nil {
		return false
	}
	historyPath, errHistory := filepath.Abs(usageStatsHistoryDBPath)
	if errHistory != nil {
		return false
	}
	if candidatePath == historyPath {
		return true
	}
	for _, suffix := range []string{"-journal", "-wal", "-shm"} {
		if candidatePath == historyPath+suffix {
			return true
		}
	}
	return false
}

func usageStatsFileName(apiKey string) string {
	escaped := url.PathEscape(normalizedUsageStatsAPIKey(apiKey))
	if escaped == "" {
		return "unknown"
	}
	return escaped
}

func defaultUsageStatsHistoryDBPath() string {
	return filepath.Join(defaultUsageStatsPersistenceDir, usageStatsHistoryDBFileName)
}

func normalizedUsageStatsAPIKey(apiKey string) string {
	apiKey = strings.TrimSpace(apiKey)
	if apiKey == "" {
		return "unknown"
	}
	return apiKey
}

func parseUsageStatsTime(value string) (time.Time, error) {
	if strings.TrimSpace(value) == "" {
		return time.Time{}, nil
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}, err
	}
	return parsed.UTC(), nil
}

func (s *usageStatsStore) pruneLocked() {
	if len(s.events) == 0 {
		return
	}

	weekly := s.windows.SevenDay
	kept := s.events[:0]
	for _, event := range s.events {
		if event.Timestamp.IsZero() || event.Timestamp.Before(weekly.Start) || !event.Timestamp.Before(weekly.End) {
			continue
		}
		kept = append(kept, event)
	}
	s.events = kept
}

func (s *usageStatsStore) captureClosedSevenDayStatsFromEventsLocked() {
	if len(s.events) == 0 || s.windows.SevenDay.Start.IsZero() {
		return
	}

	aggregatesByWindow := make(map[string]map[string]*clientUsageAggregate)
	windowsByKey := make(map[string]UsageWindowInfo)
	for _, event := range s.events {
		if event.Timestamp.IsZero() || !event.Timestamp.Before(s.windows.SevenDay.Start) {
			continue
		}
		apiKey := normalizedUsageStatsAPIKey(event.APIKey)
		event.APIKey = apiKey
		event = normalizeBilledEvent(event)
		window := currentUsageStatsWindows(event.Timestamp).SevenDay
		if window.Start.IsZero() || !window.End.After(window.Start) || window.End.After(s.windows.SevenDay.Start) {
			continue
		}
		windowKey := window.Start.UTC().Format(time.RFC3339Nano)
		aggregates := aggregatesByWindow[windowKey]
		if aggregates == nil {
			aggregates = make(map[string]*clientUsageAggregate)
			aggregatesByWindow[windowKey] = aggregates
			windowsByKey[windowKey] = window
		}
		aggregate := aggregates[apiKey]
		if aggregate == nil {
			aggregate = &clientUsageAggregate{}
			aggregates[apiKey] = aggregate
		}
		addUsageEventToClientWindowStat(&aggregate.SevenDay, event)
	}

	for windowKey, aggregates := range aggregatesByWindow {
		s.captureClosedSevenDayStatsLocked(windowsByKey[windowKey], aggregates)
	}
}

func (s *usageStatsStore) captureClosedSevenDayStatsLocked(window UsageWindowInfo, aggregates map[string]*clientUsageAggregate) {
	if window.Start.IsZero() || window.End.IsZero() || len(aggregates) == 0 {
		return
	}
	if s.history == nil {
		s.history = make(map[string][]ClientUsageHistoricalStat)
	}
	for apiKey, aggregate := range aggregates {
		if aggregate == nil || aggregate.SevenDay.RequestCount == 0 {
			continue
		}
		stat := ClientUsageHistoricalStat{
			APIKey:   normalizedUsageStatsAPIKey(apiKey),
			Window:   window,
			SevenDay: cloneClientUsageWindowStat(aggregate.SevenDay),
		}
		sortProviderUsageStats(stat.SevenDay.ProviderStats)
		s.upsertHistoricalSevenDayStatLocked(stat)
	}
}

func (s *usageStatsStore) upsertHistoricalSevenDayStatLocked(stat ClientUsageHistoricalStat) {
	apiKey := normalizedUsageStatsAPIKey(stat.APIKey)
	stat.APIKey = apiKey
	stats := s.history[apiKey]
	for i := range stats {
		if stats[i].Window.Start.Equal(stat.Window.Start) {
			stats[i] = stat
			s.history[apiKey] = stats
			return
		}
	}
	s.history[apiKey] = append(stats, stat)
}

func (s *usageStatsStore) historicalSevenDayStatsLocked() []ClientUsageHistoricalStat {
	if len(s.history) == 0 {
		return nil
	}
	out := make([]ClientUsageHistoricalStat, 0)
	for _, stats := range s.history {
		for _, stat := range stats {
			stat.SevenDay = cloneClientUsageWindowStat(stat.SevenDay)
			sortProviderUsageStats(stat.SevenDay.ProviderStats)
			out = append(out, stat)
		}
	}
	sortHistoricalSevenDayStats(out)
	return out
}

func makeUsageStatsHistoryMap(stats []ClientUsageHistoricalStat) map[string][]ClientUsageHistoricalStat {
	if len(stats) == 0 {
		return nil
	}
	out := make(map[string][]ClientUsageHistoricalStat)
	for _, stat := range stats {
		apiKey := normalizedUsageStatsAPIKey(stat.APIKey)
		stat.APIKey = apiKey
		stat.SevenDay = cloneClientUsageWindowStat(stat.SevenDay)
		sortProviderUsageStats(stat.SevenDay.ProviderStats)
		out[apiKey] = append(out[apiKey], stat)
	}
	for apiKey := range out {
		sortHistoricalSevenDayStats(out[apiKey])
	}
	return out
}

func sortHistoricalSevenDayStats(stats []ClientUsageHistoricalStat) {
	sort.Slice(stats, func(i, j int) bool {
		left := stats[i]
		right := stats[j]
		if !left.Window.Start.Equal(right.Window.Start) {
			return left.Window.Start.After(right.Window.Start)
		}
		return left.APIKey < right.APIKey
	})
}

type providerStatsKey struct {
	sessionAffinityID string
	provider          string
	model             string
	alias             string
	endpoint          string
}

func (s *usageStatsStore) ensureWindowsLocked(now time.Time) {
	next := currentUsageStatsWindows(now)
	if s.windows.TwelveHour.Start.Equal(next.TwelveHour.Start) && s.windows.SevenDay.Start.Equal(next.SevenDay.Start) {
		if s.aggregates == nil {
			s.rebuildAggregatesLocked()
		}
		return
	}
	if !s.windows.SevenDay.Start.IsZero() && !s.windows.SevenDay.Start.Equal(next.SevenDay.Start) {
		if s.aggregates == nil {
			s.rebuildAggregatesLocked()
		}
		s.captureClosedSevenDayStatsLocked(s.windows.SevenDay, s.aggregates)
	}
	s.windows = next
	s.pruneLocked()
	s.rebuildAggregatesLocked()
}

func (s *usageStatsStore) rebuildAggregatesLocked() {
	s.aggregates = make(map[string]*clientUsageAggregate)
	sort.SliceStable(s.events, func(i, j int) bool {
		return s.events[i].Timestamp.Before(s.events[j].Timestamp)
	})
	for index := range s.events {
		event := s.events[index]
		event.APIKey = normalizedUsageStatsAPIKey(event.APIKey)
		event = normalizeBilledEvent(event)
		s.events[index].APIKey = event.APIKey
		s.events[index] = event
		if isWithinWindow(event.Timestamp, s.windows.TwelveHour) {
			s.addToAggregateWindowLocked(event.APIKey, event, "12h")
		}
		if isWithinWindow(event.Timestamp, s.windows.SevenDay) {
			s.addToAggregateWindowLocked(event.APIKey, event, "7d")
		}
	}
}

func (s *usageStatsStore) addToAggregateWindowLocked(apiKey string, event usageStatsEvent, window string) {
	aggregate := s.aggregates[apiKey]
	if aggregate == nil {
		aggregate = &clientUsageAggregate{}
		if s.aggregates == nil {
			s.aggregates = make(map[string]*clientUsageAggregate)
		}
		s.aggregates[apiKey] = aggregate
	}
	switch window {
	case "12h":
		addUsageEventToClientWindowStat(&aggregate.TwelveHour, event)
	case "7d":
		addUsageEventToClientWindowStat(&aggregate.SevenDay, event)
	}
}

func currentUsageStatsWindows(now time.Time) ClientUsageWindows {
	now = now.UTC()
	twelveHourStartHour := 0
	if now.Hour() >= 12 {
		twelveHourStartHour = 12
	}
	twelveHourStart := time.Date(now.Year(), now.Month(), now.Day(), twelveHourStartHour, 0, 0, 0, time.UTC)

	weekStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC).
		AddDate(0, 0, -int(now.Weekday()))

	return ClientUsageWindows{
		TwelveHour: UsageWindowInfo{
			Start: twelveHourStart,
			End:   twelveHourStart.Add(12 * time.Hour),
		},
		SevenDay: UsageWindowInfo{
			Start: weekStart,
			End:   weekStart.AddDate(0, 0, 7),
		},
	}
}

func isWithinWindow(timestamp time.Time, window UsageWindowInfo) bool {
	if timestamp.IsZero() || window.Start.IsZero() || window.End.IsZero() {
		return false
	}
	timestamp = timestamp.UTC()
	return !timestamp.Before(window.Start) && timestamp.Before(window.End)
}

func addUsageEventToClientWindowStat(stat *ClientUsageWindowStat, event usageStatsEvent) {
	stat.CostUSD = billing.Add(stat.CostUSD, event.CostUSD)
	stat.RequestCount++
	if event.Failed {
		stat.FailureCount++
	} else {
		stat.SuccessCount++
	}
	stat.LatencyMs += event.LatencyMs
	addTokenStats(&stat.Tokens, event.Tokens)
	updateUsageStatTimes(&stat.FirstRequest, &stat.LastRequest, event.Timestamp)

	key := providerStatsKey{
		sessionAffinityID: event.SessionAffinityID,
		provider:          event.Provider,
		model:             event.Model,
		alias:             event.Alias,
		endpoint:          event.Endpoint,
	}
	for i := range stat.ProviderStats {
		if providerUsageStatsKey(stat.ProviderStats[i]) == key {
			addUsageEventToProviderStat(&stat.ProviderStats[i], event)
			return
		}
	}
	breakdown := ProviderUsageStat{
		SessionAffinityID: event.SessionAffinityID,
		Provider:          event.Provider,
		Model:             event.Model,
		Alias:             event.Alias,
		Endpoint:          event.Endpoint,
	}
	addUsageEventToProviderStat(&breakdown, event)
	stat.ProviderStats = append(stat.ProviderStats, breakdown)
}

func addUsageEventToProviderStat(stat *ProviderUsageStat, event usageStatsEvent) {
	stat.CostUSD = billing.Add(stat.CostUSD, event.CostUSD)
	stat.BillingVersion = 1
	stat.RequestCount++
	if event.Failed {
		stat.FailureCount++
	} else {
		stat.SuccessCount++
	}
	stat.LatencyMs += event.LatencyMs
	addTokenStats(&stat.Tokens, event.Tokens)
	updateUsageStatTimes(&stat.FirstRequest, &stat.LastRequest, event.Timestamp)
}

func addTokenStats(total *usageStatTokens, next usageStatTokens) {
	total.ReadTokens = addNonnegativeTokens(total.ReadTokens, next.ReadTokens)
	total.WriteTokens = addNonnegativeTokens(total.WriteTokens, next.WriteTokens)
	total.ReasoningTokens = addNonnegativeTokens(total.ReasoningTokens, next.ReasoningTokens)
	total.CacheReadTokens = addNonnegativeTokens(total.CacheReadTokens, next.CacheReadTokens)
	total.CacheWriteTokens = addNonnegativeTokens(total.CacheWriteTokens, next.CacheWriteTokens)
	total.TotalTokens = addNonnegativeTokens(total.TotalTokens, next.TotalTokens)
}

func usageStatTokensFromQueueTokens(tokens tokenStats) usageStatTokens {
	return normalizeUsageStatTokens(usageStatTokens{
		ReadTokens:       tokens.ReadTokens,
		WriteTokens:      tokens.WriteTokens,
		ReasoningTokens:  tokens.ReasoningTokens,
		CacheReadTokens:  tokens.CacheReadTokens,
		CacheWriteTokens: tokens.CacheCreationTokens,
		TotalTokens:      tokens.TotalTokens,
	})
}

func normalizeUsageStatTokens(tokens usageStatTokens) usageStatTokens {
	if tokens.ReadTokens == 0 && tokens.WriteTokens == 0 && tokens.ReasoningTokens == 0 && tokens.CacheReadTokens == 0 {
		return tokens
	}
	tokens.TotalTokens = addNonnegativeTokens(tokens.ReadTokens, tokens.WriteTokens)
	return tokens
}

func updateUsageStatTimes(first *time.Time, last *time.Time, timestamp time.Time) {
	if timestamp.IsZero() {
		return
	}
	if first.IsZero() || timestamp.Before(*first) {
		*first = timestamp
	}
	if last.IsZero() || timestamp.After(*last) {
		*last = timestamp
	}
}

func providerUsageSortKey(stat ProviderUsageStat) string {
	return stat.SessionAffinityID + "\x00" + stat.Provider + "\x00" + stat.Model + "\x00" + stat.Alias + "\x00" + stat.Endpoint
}

func providerUsageStatsKey(stat ProviderUsageStat) providerStatsKey {
	return providerStatsKey{
		sessionAffinityID: stat.SessionAffinityID,
		provider:          stat.Provider,
		model:             stat.Model,
		alias:             stat.Alias,
		endpoint:          stat.Endpoint,
	}
}

func sortProviderUsageStats(stats []ProviderUsageStat) {
	sort.Slice(stats, func(i, j int) bool {
		left := stats[i]
		right := stats[j]
		if left.Tokens.TotalTokens != right.Tokens.TotalTokens {
			return left.Tokens.TotalTokens > right.Tokens.TotalTokens
		}
		if !left.LastRequest.Equal(right.LastRequest) {
			return left.LastRequest.After(right.LastRequest)
		}
		return providerUsageSortKey(left) < providerUsageSortKey(right)
	})
}

func cloneClientUsageWindowStat(stat ClientUsageWindowStat) ClientUsageWindowStat {
	stat.ProviderStats = append([]ProviderUsageStat(nil), stat.ProviderStats...)
	return stat
}

func clientCostLimitsSnapshot() map[string]ClientCostLimits {
	clientCostLimitsMu.RLock()
	defer clientCostLimitsMu.RUnlock()
	if len(clientCostLimits) == 0 {
		return nil
	}
	out := make(map[string]ClientCostLimits, len(clientCostLimits))
	for apiKey, limits := range clientCostLimits {
		out[apiKey] = limits
	}
	return out
}

func clientCostLimitsConfigured() bool {
	clientCostLimitsMu.RLock()
	defer clientCostLimitsMu.RUnlock()
	return len(clientCostLimits) > 0
}

func usageStatsTrackingEnabled() bool {
	return UsageStatisticsEnabled() || clientCostLimitsConfigured()
}

func normalizedRetentionSeconds() int64 {
	windowSeconds := retentionSeconds.Load()
	if windowSeconds <= 0 {
		return defaultRetentionSeconds
	}
	if windowSeconds > maxRetentionSeconds {
		return maxRetentionSeconds
	}
	return windowSeconds
}
