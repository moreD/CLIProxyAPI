package redisqueue

// UsageStatsPersistenceDirectory returns the directory used by client statistics
// so independent accounting stores can share the configured deployment location.
func UsageStatsPersistenceDirectory() string { return usageStatsPersistenceDir }
