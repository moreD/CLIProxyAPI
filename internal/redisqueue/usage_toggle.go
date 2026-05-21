package redisqueue

import "sync/atomic"

var usageStatisticsEnabled atomic.Bool

func init() {
	usageStatisticsEnabled.Store(true)
}

// SetUsageStatisticsEnabled toggles whether usage records are enqueued and tracked in memory.
// This is controlled by the config field `usage-statistics-enabled` and the corresponding management API.
func SetUsageStatisticsEnabled(enabled bool) {
	usageStatisticsEnabled.Store(enabled)
	if !enabled {
		ClearUsageStats()
	}
}

// UsageStatisticsEnabled reports whether the usage queue plugin should publish records.
func UsageStatisticsEnabled() bool { return usageStatisticsEnabled.Load() }
