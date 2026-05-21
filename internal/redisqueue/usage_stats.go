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

	log "github.com/sirupsen/logrus"
)

const (
	defaultUsageStatsPersistenceDir = "/tmp/.cliproxyapi/stats"
	usageStatsPersistenceInterval   = 5 * time.Minute
)

type UsageStatsSnapshot struct {
	GeneratedAt   time.Time         `json:"generated_at"`
	WindowSeconds int64             `json:"window_seconds"`
	APIKeys       []ClientUsageStat `json:"api_keys"`
}

type ClientUsageStat struct {
	APIKey        string              `json:"api_key"`
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
	Provider     string          `json:"provider"`
	Model        string          `json:"model"`
	Alias        string          `json:"alias"`
	Endpoint     string          `json:"endpoint"`
	RequestCount int64           `json:"request_count"`
	SuccessCount int64           `json:"success_count"`
	FailureCount int64           `json:"failure_count"`
	FirstRequest time.Time       `json:"first_request_at"`
	LastRequest  time.Time       `json:"last_request_at"`
	LatencyMs    int64           `json:"latency_ms"`
	Tokens       usageStatTokens `json:"tokens"`
}

type usageStatsEvent struct {
	Timestamp time.Time       `json:"timestamp"`
	APIKey    string          `json:"api_key"`
	Provider  string          `json:"provider"`
	Model     string          `json:"model"`
	Alias     string          `json:"alias"`
	Endpoint  string          `json:"endpoint"`
	LatencyMs int64           `json:"latency_ms"`
	Tokens    usageStatTokens `json:"tokens"`
	Failed    bool            `json:"failed"`
}

type usageStatTokens struct {
	ReadTokens      int64 `json:"read_tokens"`
	WriteTokens     int64 `json:"write_tokens"`
	ReasoningTokens int64 `json:"reasoning_tokens"`
	CacheReadTokens int64 `json:"cache_read_tokens"`
	TotalTokens     int64 `json:"total_tokens"`
}

type usageStatsStore struct {
	mu     sync.Mutex
	events []usageStatsEvent
}

type usageStatsPersistedFile struct {
	APIKey string            `json:"api_key"`
	Events []usageStatsEvent `json:"events"`
}

var (
	globalUsageStats          usageStatsStore
	usageStatsPersistenceDir  = defaultUsageStatsPersistenceDir
	usageStatsPersistenceOnce sync.Once
)

func RecordUsageStat(detail queuedUsageDetail) {
	if !Enabled() || !UsageStatisticsEnabled() {
		return
	}

	timestamp := detail.Timestamp
	if timestamp.IsZero() {
		timestamp = time.Now()
	}

	event := usageStatsEvent{
		Timestamp: timestamp,
		APIKey:    strings.TrimSpace(detail.APIKey),
		Provider:  strings.TrimSpace(detail.Provider),
		Model:     strings.TrimSpace(detail.Model),
		Alias:     strings.TrimSpace(detail.Alias),
		Endpoint:  strings.TrimSpace(detail.Endpoint),
		LatencyMs: detail.LatencyMs,
		Tokens:    usageStatTokensFromQueueTokens(detail.Tokens),
		Failed:    detail.Failed,
	}

	globalUsageStats.add(event, time.Now())
}

func StartUsageStatsPersistence(ctx context.Context) {
	if ctx == nil {
		ctx = context.Background()
	}
	usageStatsPersistenceOnce.Do(func() {
		if UsageStatisticsEnabled() {
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

func ClearUsageStats() {
	globalUsageStats.clear()
}

func (s *usageStatsStore) add(event usageStatsEvent, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.pruneLocked(now)
	s.events = append(s.events, event)
}

func (s *usageStatsStore) snapshot(now time.Time) UsageStatsSnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.pruneLocked(now)

	byAPIKey := make(map[string]*ClientUsageStat)
	providerStats := make(map[string]map[providerStatsKey]*ProviderUsageStat)
	for _, event := range s.events {
		apiKey := event.APIKey
		if apiKey == "" {
			apiKey = "unknown"
		}

		stat := byAPIKey[apiKey]
		if stat == nil {
			stat = &ClientUsageStat{APIKey: apiKey}
			byAPIKey[apiKey] = stat
		}
		addUsageEventToClientStat(stat, event)

		apiProviderStats := providerStats[apiKey]
		if apiProviderStats == nil {
			apiProviderStats = make(map[providerStatsKey]*ProviderUsageStat)
			providerStats[apiKey] = apiProviderStats
		}
		key := providerStatsKey{
			provider: event.Provider,
			model:    event.Model,
			alias:    event.Alias,
			endpoint: event.Endpoint,
		}
		breakdown := apiProviderStats[key]
		if breakdown == nil {
			breakdown = &ProviderUsageStat{
				Provider: event.Provider,
				Model:    event.Model,
				Alias:    event.Alias,
				Endpoint: event.Endpoint,
			}
			apiProviderStats[key] = breakdown
		}
		addUsageEventToProviderStat(breakdown, event)
	}

	out := UsageStatsSnapshot{
		GeneratedAt:   now,
		WindowSeconds: normalizedUsageStatsWindowSeconds(),
		APIKeys:       make([]ClientUsageStat, 0, len(byAPIKey)),
	}
	for apiKey, stat := range byAPIKey {
		breakdowns := providerStats[apiKey]
		stat.ProviderStats = make([]ProviderUsageStat, 0, len(breakdowns))
		for _, breakdown := range breakdowns {
			stat.ProviderStats = append(stat.ProviderStats, *breakdown)
		}
		sort.Slice(stat.ProviderStats, func(i, j int) bool {
			left := stat.ProviderStats[i]
			right := stat.ProviderStats[j]
			if left.Tokens.TotalTokens != right.Tokens.TotalTokens {
				return left.Tokens.TotalTokens > right.Tokens.TotalTokens
			}
			if !left.LastRequest.Equal(right.LastRequest) {
				return left.LastRequest.After(right.LastRequest)
			}
			return providerUsageSortKey(left) < providerUsageSortKey(right)
		})
		out.APIKeys = append(out.APIKeys, *stat)
	}
	sort.Slice(out.APIKeys, func(i, j int) bool {
		left := out.APIKeys[i]
		right := out.APIKeys[j]
		if left.Tokens.TotalTokens != right.Tokens.TotalTokens {
			return left.Tokens.TotalTokens > right.Tokens.TotalTokens
		}
		if !left.LastRequest.Equal(right.LastRequest) {
			return left.LastRequest.After(right.LastRequest)
		}
		return left.APIKey < right.APIKey
	})

	return out
}

func (s *usageStatsStore) clear() {
	s.mu.Lock()
	s.events = nil
	s.mu.Unlock()
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
			if !UsageStatisticsEnabled() {
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
	s.pruneLocked(now)
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
	s.pruneLocked(now)
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

func (s *usageStatsStore) pruneLocked(now time.Time) {
	if len(s.events) == 0 {
		return
	}

	cutoff := now.Add(-time.Duration(normalizedUsageStatsWindowSeconds()) * time.Second)
	keepFrom := 0
	for keepFrom < len(s.events) && s.events[keepFrom].Timestamp.Before(cutoff) {
		keepFrom++
	}
	if keepFrom == 0 {
		return
	}
	if keepFrom >= len(s.events) {
		s.events = nil
		return
	}
	copy(s.events, s.events[keepFrom:])
	s.events = s.events[:len(s.events)-keepFrom]
}

type providerStatsKey struct {
	provider string
	model    string
	alias    string
	endpoint string
}

func addUsageEventToClientStat(stat *ClientUsageStat, event usageStatsEvent) {
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
	return usageStatTokens{
		ReadTokens:      tokens.ReadTokens,
		WriteTokens:     tokens.WriteTokens,
		ReasoningTokens: tokens.ReasoningTokens,
		CacheReadTokens: tokens.CacheReadTokens,
		TotalTokens:     tokens.TotalTokens,
	}
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
	return stat.Provider + "\x00" + stat.Model + "\x00" + stat.Alias + "\x00" + stat.Endpoint
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

func normalizedUsageStatsWindowSeconds() int64 {
	windowSeconds := usageStatsWindowSeconds.Load()
	if windowSeconds <= 0 {
		return defaultUsageStatsWindowSeconds
	}
	if windowSeconds > maxUsageStatsWindowSeconds {
		return maxUsageStatsWindowSeconds
	}
	return windowSeconds
}
