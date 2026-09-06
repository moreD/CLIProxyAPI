package redisqueue

import (
	"github.com/router-for-me/CLIProxyAPI/v7/internal/billing"
	"strings"
)

func eventCost(event usageStatsEvent, contextKnown bool) billing.USD {
	t := event.Tokens
	return billing.Price(event.Model, event.ServiceTier, billing.Tokens{Input: t.ReadTokens, Output: t.WriteTokens, CacheRead: t.CacheReadTokens, CacheWrite: t.CacheWriteTokens}, contextKnown)
}

// Legacy snapshots did not retain service tiers or cache-write counts. Recover
// raw input/output semantics using the provider, then price at Standard rates.
func normalizeLegacyTokens(t usageStatTokens, provider string) usageStatTokens {
	p := strings.ToLower(provider)
	t.ReadTokens = max(t.ReadTokens, 0)
	t.WriteTokens = max(t.WriteTokens, 0)
	t.ReasoningTokens = max(t.ReasoningTokens, 0)
	t.CacheReadTokens = max(t.CacheReadTokens, 0)
	if strings.Contains(p, "claude") || strings.Contains(p, "anthropic") {
		t.ReadTokens = addNonnegativeTokens(t.ReadTokens, t.CacheReadTokens, t.CacheWriteTokens)
		t.WriteTokens = addNonnegativeTokens(t.WriteTokens, t.ReasoningTokens)
	} else if strings.Contains(p, "gemini") || strings.Contains(p, "vertex") || strings.Contains(p, "antigravity") || strings.Contains(p, "aistudio") {
		t.WriteTokens = addNonnegativeTokens(t.WriteTokens, t.ReasoningTokens)
	} else {
		t.WriteTokens = max(t.WriteTokens, t.ReasoningTokens)
	}
	// Rows with only a legacy total cannot be reconstructed. Keep their observed
	// total as input rather than silently dropping recorded usage.
	if t.ReadTokens == 0 && t.WriteTokens == 0 && t.TotalTokens > 0 {
		t.ReadTokens = t.TotalTokens
	}
	t.ReadTokens = max(t.ReadTokens, addNonnegativeTokens(t.CacheReadTokens, t.CacheWriteTokens))
	return normalizeUsageStatTokens(t)
}

func addNonnegativeTokens(values ...int64) int64 {
	var total int64
	for _, v := range values {
		v = max(v, 0)
		if total > (1<<63-1)-v {
			return 1<<63 - 1
		}
		total += v
	}
	return total
}

func normalizeBilledEvent(event usageStatsEvent) usageStatsEvent {
	if event.BillingVersion >= 1 {
		return event
	}
	event.Tokens = normalizeLegacyTokens(event.Tokens, event.Provider)
	event.CostUSD = eventCost(event, true)
	event.BillingVersion = 1
	return event
}

func normalizeHistoricalBilling(stat *ClientUsageWindowStat) {
	stat.CostUSD = 0
	var tokens usageStatTokens
	for i := range stat.ProviderStats {
		p := &stat.ProviderStats[i]
		if p.BillingVersion < 1 {
			p.Tokens = normalizeLegacyTokens(p.Tokens, p.Provider)
			// Aggregate tokens cannot establish per-request long-context thresholds.
			p.CostUSD = eventCost(usageStatsEvent{Model: p.Model, Tokens: p.Tokens}, false)
			p.BillingVersion = 1
		}
		stat.CostUSD = billing.Add(stat.CostUSD, p.CostUSD)
		addTokenStats(&tokens, p.Tokens)
	}
	if len(stat.ProviderStats) > 0 {
		stat.Tokens = tokens
	}
}
