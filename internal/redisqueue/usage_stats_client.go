package redisqueue

import "time"

// ClientUsageSnapshot contains only the authenticated client's current windows.
type ClientUsageSnapshot struct {
	Currency    string             `json:"currency"`
	GeneratedAt time.Time          `json:"generated_at"`
	Windows     ClientUsageWindows `json:"windows"`
	Usage       ClientUsageSummary `json:"usage"`
}

type ClientUsageSummary struct {
	TwelveHour ClientUsageWindowStat `json:"12h"`
	SevenDay   ClientUsageWindowStat `json:"7d"`
	Limits     ClientCostLimits      `json:"limits"`
}

// ClientUsageSnapshotNow reads one client's totals without consuming usage events.
// The caller must authenticate the key before requesting its snapshot.
func ClientUsageSnapshotNow(apiKey string) (ClientUsageSnapshot, bool) {
	if usageStatsRecoveryFailed.Load() || !usageStatsTrackingEnabled() {
		return ClientUsageSnapshot{}, false
	}
	return globalUsageStats.clientSnapshot(normalizedUsageStatsAPIKey(apiKey), time.Now()), true
}

func (s *usageStatsStore) clientSnapshot(apiKey string, now time.Time) ClientUsageSnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ensureWindowsLocked(now)

	out := ClientUsageSnapshot{Currency: "USD", GeneratedAt: now, Windows: s.windows}
	clientCostLimitsMu.RLock()
	out.Usage.Limits = clientCostLimits[apiKey]
	clientCostLimitsMu.RUnlock()
	if aggregate := s.aggregates[apiKey]; aggregate != nil {
		out.Usage.TwelveHour = aggregate.TwelveHour
		out.Usage.SevenDay = aggregate.SevenDay
		// The public summary does not expose internal routing or session details.
		out.Usage.TwelveHour.ProviderStats = nil
		out.Usage.SevenDay.ProviderStats = nil
	}
	return out
}
