package auth

import (
	"context"
	"net/http"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
)

const (
	quotaRefreshInterval        = 15 * time.Minute
	quotaRefreshRetryDelay      = 30 * time.Second
	requestQuotaRefreshMaxAge   = 5 * time.Minute
	backgroundQuotaMaxAge       = 15 * time.Minute
	fiveHourQuotaMaxUsedPercent = 90.0
	weeklyQuotaMaxUsedPercent   = 98.0
)

// QuotaRefresher exposes provider-specific quota refresh and probe hooks.
type QuotaRefresher interface {
	RefreshQuota(ctx context.Context, auth *Auth) (*QuotaInfo, error)
	ProbeQuotaCountdown(ctx context.Context, auth *Auth) (*QuotaInfo, error)
}

// WorkspaceMetadataRefresher exposes optional provider workspace labels keyed
// by account ID. The manager persists returned labels into matching auths.
type WorkspaceMetadataRefresher interface {
	RefreshWorkspaceMetadata(ctx context.Context, auth *Auth) (map[string]string, error)
}

func (m *Manager) refreshQuotaBeforeRequest(ctx context.Context, auth *Auth, executor ProviderExecutor) (bool, error) {
	if m == nil || auth == nil || executor == nil || strings.TrimSpace(auth.ID) == "" {
		return false, nil
	}
	refresher, ok := executor.(QuotaRefresher)
	if !ok || refresher == nil || !quotaStateSupportedAuth(auth) {
		return false, nil
	}

	current := m.quotaStateAuthSnapshot(auth.ID)
	if current == nil || hasUnauthorizedAuthFailure(current) {
		return true, nil
	}

	now := time.Now()
	quota := current.RuntimeQuota.Clone()
	if quotaRefreshStale(current, requestQuotaRefreshMaxAge, now) {
		log.WithField("auth_id", auth.ID).Info("codex quota request refresh started")
		refreshedQuota, refreshErr := refresher.RefreshQuota(ctx, auth)
		if errContext := ctx.Err(); errContext != nil {
			return false, errContext
		}
		now = time.Now()
		quota = m.storeQuotaRefreshResult(ctx, auth.ID, refreshedQuota, now)
		if refreshErr != nil {
			m.storeQuotaRefreshError(ctx, auth.ID, refreshErr)
			if isUnauthorizedError(refreshErr) {
				log.WithField("auth_id", auth.ID).Infof("codex quota request refresh unauthorized; switching auth: %v", refreshErr)
				m.markQuotaAuthUnauthorized(ctx, auth, refreshErr)
				return true, nil
			}
			log.WithField("auth_id", auth.ID).Infof("codex quota request refresh failed; falling back to stored quota state: %v", refreshErr)
		} else {
			m.storeQuotaRefreshError(ctx, auth.ID, nil)
			log.WithField("auth_id", auth.ID).Info("codex quota request refresh completed")
		}
	}

	if reached, retryAfter := quotaLimitReached(quota, now); reached {
		m.markQuotaReached(ctx, auth, retryAfter)
		return true, nil
	}
	return false, nil
}

func quotaLimitSwitchError() error {
	return &Error{
		Code:       "quota_reached",
		Message:    "reached limit",
		HTTPStatus: http.StatusTooManyRequests,
	}
}

func quotaStateSupportedAuth(auth *Auth) bool {
	if auth == nil || auth.Disabled || auth.Status == StatusDisabled || hasUnauthorizedAuthFailure(auth) {
		return false
	}
	if !strings.EqualFold(strings.TrimSpace(auth.Provider), "codex") {
		return false
	}
	accountType, _ := auth.AccountInfo()
	return strings.EqualFold(accountType, "oauth")
}

func (m *Manager) quotaStateAuthSnapshot(authID string) *Auth {
	m.mu.RLock()
	defer m.mu.RUnlock()
	auth := m.auths[authID]
	if !quotaStateSupportedAuth(auth) {
		return nil
	}
	return auth.Clone()
}

func quotaRefreshStale(auth *Auth, maxAge time.Duration, now time.Time) bool {
	if auth == nil || maxAge <= 0 || auth.RuntimeQuota == nil || !auth.RuntimeQuota.HasAny() {
		return true
	}
	return auth.LastQuotaSeenAt.IsZero() || now.Sub(auth.LastQuotaSeenAt) > maxAge
}

func quotaLimitReached(quota *QuotaInfo, now time.Time) (bool, *time.Duration) {
	if quota == nil {
		return false, nil
	}
	var nextFreshAt time.Time
	reached := false
	consider := func(window QuotaWindow, maxUsedPercent float64) {
		if !window.usedKnown() || window.UsedPercent < maxUsedPercent {
			return
		}
		reached = true
		if !window.NextFreshAt.IsZero() && window.NextFreshAt.After(now) && (nextFreshAt.IsZero() || window.NextFreshAt.After(nextFreshAt)) {
			nextFreshAt = window.NextFreshAt
		}
	}
	consider(quota.FiveHour, fiveHourQuotaMaxUsedPercent)
	consider(quota.Weekly, weeklyQuotaMaxUsedPercent)
	if !reached {
		return false, nil
	}
	retryAfter := requestQuotaRefreshMaxAge
	if !nextFreshAt.IsZero() {
		retryAfter = nextFreshAt.Sub(now)
	}
	if retryAfter < time.Second {
		retryAfter = requestQuotaRefreshMaxAge
	}
	return true, &retryAfter
}

func (m *Manager) markQuotaReached(ctx context.Context, auth *Auth, retryAfter *time.Duration) {
	if m == nil || auth == nil {
		return
	}
	m.MarkResult(ctx, Result{
		AuthID:     auth.ID,
		Provider:   auth.Provider,
		Success:    false,
		RetryAfter: retryAfter,
		Error: &Error{
			Code:       "quota_reached",
			Message:    "reached limit",
			HTTPStatus: http.StatusTooManyRequests,
		},
	})
}
