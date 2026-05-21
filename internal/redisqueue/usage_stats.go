package redisqueue

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	log "github.com/sirupsen/logrus"
)

const (
	defaultUsageStatsPersistenceDir = "/tmp/.cliproxyapi/stats"
	usageStatsPersistenceInterval   = 5 * time.Minute
)

type UsageStatsSnapshot struct {
	GeneratedAt time.Time          `json:"generated_at"`
	Windows     ClientUsageWindows `json:"windows"`
	APIKeys     []ClientUsageStat  `json:"api_keys"`
}

type ClientUsageStat struct {
	APIKey     string                `json:"api_key"`
	TwelveHour ClientUsageWindowStat `json:"12h"`
	SevenDay   ClientUsageWindowStat `json:"7d"`
	Limits     ClientTokenLimits     `json:"limits,omitempty"`
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
	ReadTokens      int64 `json:"read_tokens"`
	WriteTokens     int64 `json:"write_tokens"`
	ReasoningTokens int64 `json:"reasoning_tokens"`
	CacheReadTokens int64 `json:"cache_read_tokens"`
	TotalTokens     int64 `json:"total_tokens"`
}

type usageStatsStore struct {
	mu         sync.Mutex
	events     []usageStatsEvent
	windows    ClientUsageWindows
	aggregates map[string]*clientUsageAggregate
}

type clientUsageAggregate struct {
	TwelveHour ClientUsageWindowStat
	SevenDay   ClientUsageWindowStat
}

type ClientTokenLimits struct {
	TwelveHour int64 `json:"12h,omitempty"`
	SevenDay   int64 `json:"7d,omitempty"`
}

type ClientUsageLimitDecision struct {
	Exceeded bool      `json:"exceeded"`
	Window   string    `json:"window,omitempty"`
	Used     int64     `json:"used,omitempty"`
	Limit    int64     `json:"limit,omitempty"`
	ResetsAt time.Time `json:"resets_at,omitempty"`
}

type usageStatsPersistedFile struct {
	APIKey string            `json:"api_key"`
	Events []usageStatsEvent `json:"events"`
}

var (
	globalUsageStats          usageStatsStore
	usageStatsPersistenceDir  = defaultUsageStatsPersistenceDir
	usageStatsPersistenceOnce sync.Once
	clientTokenLimitsMu       sync.RWMutex
	clientTokenLimits         = make(map[string]ClientTokenLimits)
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

	globalUsageStats.add(event, time.Now())
}

func StartUsageStatsPersistence(ctx context.Context) {
	if ctx == nil {
		ctx = context.Background()
	}
	usageStatsPersistenceOnce.Do(func() {
		if usageStatsTrackingEnabled() {
			if err := globalUsageStats.recoverFromDisk(time.Now()); err != nil {
				log.Warnf("failed to recover client usage stats: %v", err)
			}
		}
		go runUsageStatsPersistenceLoop(ctx)
	})
}

func UsageStatsSnapshotNow() UsageStatsSnapshot {
	return globalUsageStats.snapshot(time.Now())
}

func SetClientTokenLimits(entries []config.APIKeyEntry) {
	next := make(map[string]ClientTokenLimits)
	for _, entry := range config.NormalizeAPIKeyEntries(entries) {
		limits := ClientTokenLimits{
			TwelveHour: entry.TokenLimits.TwelveHour,
			SevenDay:   entry.TokenLimits.SevenDay,
		}
		if limits.TwelveHour <= 0 && limits.SevenDay <= 0 {
			continue
		}
		next[entry.APIKey] = limits
	}

	clientTokenLimitsMu.Lock()
	clientTokenLimits = next
	clientTokenLimitsMu.Unlock()
	if len(next) == 0 && !UsageStatisticsEnabled() {
		ClearUsageStats()
	}
}

func CheckClientTokenLimit(apiKey string, now time.Time) ClientUsageLimitDecision {
	apiKey = normalizedUsageStatsAPIKey(apiKey)

	clientTokenLimitsMu.RLock()
	limits := clientTokenLimits[apiKey]
	clientTokenLimitsMu.RUnlock()
	if limits.TwelveHour <= 0 && limits.SevenDay <= 0 {
		return ClientUsageLimitDecision{}
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
	event.Tokens = normalizeUsageStatTokens(event.Tokens)
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
		GeneratedAt: now,
		Windows:     s.windows,
		APIKeys:     make([]ClientUsageStat, 0, len(s.aggregates)),
	}
	limits := clientTokenLimitsSnapshot()
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
		if left.SevenDay.Tokens.TotalTokens != right.SevenDay.Tokens.TotalTokens {
			return left.SevenDay.Tokens.TotalTokens > right.SevenDay.Tokens.TotalTokens
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
	s.mu.Unlock()
}

func (s *usageStatsStore) checkLimit(apiKey string, limits ClientTokenLimits, now time.Time) ClientUsageLimitDecision {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.ensureWindowsLocked(now)
	aggregate := s.aggregates[apiKey]
	if aggregate == nil {
		return ClientUsageLimitDecision{}
	}
	if limits.TwelveHour > 0 && aggregate.TwelveHour.Tokens.TotalTokens >= limits.TwelveHour {
		return ClientUsageLimitDecision{
			Exceeded: true,
			Window:   "12h",
			Used:     aggregate.TwelveHour.Tokens.TotalTokens,
			Limit:    limits.TwelveHour,
			ResetsAt: s.windows.TwelveHour.End,
		}
	}
	if limits.SevenDay > 0 && aggregate.SevenDay.Tokens.TotalTokens >= limits.SevenDay {
		return ClientUsageLimitDecision{
			Exceeded: true,
			Window:   "7d",
			Used:     aggregate.SevenDay.Tokens.TotalTokens,
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
	s.mu.Lock()
	s.ensureWindowsLocked(now)
	events := append([]usageStatsEvent(nil), s.events...)
	s.mu.Unlock()

	return writeUsageStatsEvents(usageStatsPersistenceDir, events)
}

func (s *usageStatsStore) recoverFromDisk(now time.Time) error {
	events, err := readUsageStatsEvents(usageStatsPersistenceDir)
	if err != nil {
		return err
	}

	sort.SliceStable(events, func(i, j int) bool {
		return events[i].Timestamp.Before(events[j].Timestamp)
	})

	s.mu.Lock()
	s.events = events
	s.windows = currentUsageStatsWindows(now)
	s.pruneLocked()
	s.rebuildAggregatesLocked()
	s.mu.Unlock()
	return nil
}

func writeUsageStatsEvents(dir string, events []usageStatsEvent) error {
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

	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("read stats directory: %w", err)
	}
	for _, entry := range entries {
		if entry.IsDir() || isUsageStatsTempFile(entry.Name()) {
			continue
		}
		if _, ok := written[entry.Name()]; ok {
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
		if entry.IsDir() {
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

func isUsageStatsTempFile(name string) bool {
	return strings.HasPrefix(name, ".") && strings.HasSuffix(name, ".tmp")
}

func usageStatsFileName(apiKey string) string {
	escaped := url.PathEscape(normalizedUsageStatsAPIKey(apiKey))
	if escaped == "" {
		return "unknown"
	}
	return escaped
}

func normalizedUsageStatsAPIKey(apiKey string) string {
	apiKey = strings.TrimSpace(apiKey)
	if apiKey == "" {
		return "unknown"
	}
	return apiKey
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
		event.Tokens = normalizeUsageStatTokens(event.Tokens)
		s.events[index].APIKey = event.APIKey
		s.events[index].Tokens = event.Tokens
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
	total.ReadTokens += next.ReadTokens
	total.WriteTokens += next.WriteTokens
	total.ReasoningTokens += next.ReasoningTokens
	total.CacheReadTokens += next.CacheReadTokens
	total.TotalTokens += next.TotalTokens
}

func usageStatTokensFromQueueTokens(tokens tokenStats) usageStatTokens {
	return normalizeUsageStatTokens(usageStatTokens{
		ReadTokens:      tokens.ReadTokens,
		WriteTokens:     tokens.WriteTokens,
		ReasoningTokens: tokens.ReasoningTokens,
		CacheReadTokens: tokens.CacheReadTokens,
		TotalTokens:     tokens.TotalTokens,
	})
}

func normalizeUsageStatTokens(tokens usageStatTokens) usageStatTokens {
	if tokens.ReadTokens == 0 && tokens.WriteTokens == 0 && tokens.ReasoningTokens == 0 && tokens.CacheReadTokens == 0 {
		return tokens
	}
	cacheRead := max(tokens.CacheReadTokens, 0)
	nonCachedRead := max(tokens.ReadTokens-cacheRead, 0)
	reasoning := max(tokens.ReasoningTokens, 0)
	write := max(tokens.WriteTokens, 0)
	tokens.TotalTokens = cacheRead + nonCachedRead*10 + (reasoning+write)*60
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

func clientTokenLimitsSnapshot() map[string]ClientTokenLimits {
	clientTokenLimitsMu.RLock()
	defer clientTokenLimitsMu.RUnlock()
	if len(clientTokenLimits) == 0 {
		return nil
	}
	out := make(map[string]ClientTokenLimits, len(clientTokenLimits))
	for apiKey, limits := range clientTokenLimits {
		out[apiKey] = limits
	}
	return out
}

func clientTokenLimitsConfigured() bool {
	clientTokenLimitsMu.RLock()
	defer clientTokenLimitsMu.RUnlock()
	return len(clientTokenLimits) > 0
}

func usageStatsTrackingEnabled() bool {
	return UsageStatisticsEnabled() || clientTokenLimitsConfigured()
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
