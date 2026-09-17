package auth

import (
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/authusage"
)

func authCostLimitError() error {
	return &Error{Code: "auth_cost_limit", Message: "auth dollar quota is exhausted or unavailable", HTTPStatus: http.StatusTooManyRequests}
}

// ObserveAuthCostWindow aligns local dollar accounting with the upstream weekly
// quota. Token refreshes and the shorter request window must not reset spending.
func ObserveAuthCostWindow(auth *Auth, now time.Time) error {
	if auth == nil || auth.RuntimeQuota == nil {
		return nil
	}
	window := auth.RuntimeQuota.Weekly
	if window.NextFreshAt.IsZero() || (window.LimitWindowSeconds != 0 && window.LimitWindowSeconds != 7*24*60*60) {
		return nil
	}
	observedAt := window.RefreshedAt
	if observedAt.IsZero() {
		observedAt = auth.LastQuotaSeenAt
	}
	used, known := authWeeklyUsedPercent(window)
	return authusage.ObserveWeeklyQuota(authCostIndex(auth), window.NextFreshAt, observedAt, now, used, known)
}

func authWeeklyUsedPercent(window QuotaWindow) (float64, bool) {
	percent := window.UsedPercent
	if (window.UsedPercentKnown || percent != 0) && !math.IsNaN(percent) && !math.IsInf(percent, 0) && percent >= 0 && percent <= 100 {
		return percent, true
	}
	if window.Limit > 0 && window.Used >= 0 && window.Used <= window.Limit {
		return float64(window.Used) / float64(window.Limit) * 100, true
	}
	return 0, false
}

func authCostIndex(auth *Auth) string {
	if auth == nil {
		return ""
	}
	if index := strings.TrimSpace(auth.Index); index != "" {
		return index
	}
	// Selection snapshots are shared with readers; never assign an index in place.
	return auth.Clone().EnsureIndex()
}

func authCostBlocked(auth *Auth, now time.Time) (bool, time.Time) {
	if auth == nil {
		return false, time.Time{}
	}
	if err := ObserveAuthCostWindow(auth, now); err != nil {
		return true, time.Time{}
	}
	return authusage.Check(authCostIndex(auth), now)
}
