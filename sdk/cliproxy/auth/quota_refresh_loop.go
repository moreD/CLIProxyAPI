package auth

import (
	"context"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
)

func (m *Manager) runQuotaRefreshLoop(ctx context.Context, interval time.Duration) {
	if m == nil {
		return
	}
	if interval <= 0 {
		interval = quotaRefreshInterval
	}
	m.runQuotaRefreshCycle(ctx, quotaRefreshRetryDelay)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.runQuotaRefreshCycle(ctx, quotaRefreshRetryDelay)
		}
	}
}

func (m *Manager) runQuotaRefreshCycle(ctx context.Context, retryDelay time.Duration) {
	if m == nil {
		return
	}
	authIDs := m.quotaRefreshAuthIDs()
	for _, authID := range authIDs {
		if err := ctx.Err(); err != nil {
			return
		}
		m.refreshQuotaAuth(ctx, authID, retryDelay)
	}
}

func (m *Manager) quotaRefreshAuthIDs() []string {
	if m == nil {
		return nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	authIDs := make([]string, 0)
	for id, auth := range m.auths {
		if !m.quotaRefreshSupportedLocked(auth) {
			continue
		}
		authIDs = append(authIDs, id)
	}
	return authIDs
}

func (m *Manager) quotaRefreshSupportedLocked(auth *Auth) bool {
	if auth == nil || auth.Disabled || auth.Status == StatusDisabled {
		return false
	}
	if !strings.EqualFold(strings.TrimSpace(auth.Provider), "codex") {
		return false
	}
	accountType, _ := auth.AccountInfo()
	if !strings.EqualFold(accountType, "oauth") {
		return false
	}
	exec := m.executors[auth.Provider]
	_, ok := exec.(QuotaRefresher)
	return ok
}

func (m *Manager) refreshQuotaAuth(ctx context.Context, authID string, retryDelay time.Duration) {
	if m == nil || strings.TrimSpace(authID) == "" {
		return
	}
	if m.refreshQuotaAuthAttempt(ctx, authID) {
		return
	}
	if retryDelay > 0 {
		timer := time.NewTimer(retryDelay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
	_ = m.refreshQuotaAuthAttempt(ctx, authID)
}

func (m *Manager) refreshQuotaAuthAttempt(ctx context.Context, authID string) bool {
	auth, refresher := m.quotaRefreshSnapshot(authID)
	if auth == nil || refresher == nil {
		return true
	}
	log.WithField("auth_id", authID).Info("codex quota refresh started")
	quota, refreshed, err := refresher.RefreshQuota(ctx, auth)
	if err != nil && ctx.Err() != nil {
		return true
	}
	stored := false
	if quota != nil && quota.HasAny() {
		m.storeRuntimeQuota(ctx, authID, quota)
		stored = true
	}
	if err != nil {
		if !stored {
			log.WithField("auth_id", authID).Infof("codex quota refresh failed: %v", err)
		} else {
			log.WithField("auth_id", authID).Infof("codex quota refresh stored partial quota despite error: %v", err)
		}
		return stored
	}
	if !refreshed {
		log.WithField("auth_id", authID).Info("codex quota refresh completed without refreshed countdown")
		return stored
	}
	log.WithField("auth_id", authID).Info("codex quota probe started")
	probeQuota, probeErr := refresher.ProbeQuotaCountdown(ctx, auth)
	if probeQuota != nil && probeQuota.HasAny() {
		m.mergeRuntimeQuota(ctx, authID, probeQuota)
		stored = true
	}
	if probeErr != nil {
		if !stored {
			log.WithField("auth_id", authID).Infof("codex quota probe failed: %v", probeErr)
		} else {
			log.WithField("auth_id", authID).Infof("codex quota probe stored partial quota despite error: %v", probeErr)
		}
		return stored
	}
	log.WithField("auth_id", authID).Info("codex quota probe completed")
	return stored
}

func (m *Manager) quotaRefreshSnapshot(authID string) (*Auth, QuotaRefresher) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	auth := m.auths[authID]
	if !m.quotaRefreshSupportedLocked(auth) {
		return nil, nil
	}
	refresher, _ := m.executors[auth.Provider].(QuotaRefresher)
	return auth.Clone(), refresher
}

func (m *Manager) storeRuntimeQuota(ctx context.Context, authID string, quota *QuotaInfo) {
	if m == nil || quota == nil {
		return
	}
	var snapshot *Auth
	m.mu.Lock()
	if auth := m.auths[authID]; auth != nil {
		auth.RuntimeQuota = quota.Clone()
		auth.UpdatedAt = time.Now()
		snapshot = auth.Clone()
	}
	m.mu.Unlock()
	if snapshot == nil {
		return
	}
	if m.scheduler != nil {
		m.scheduler.upsertAuth(snapshot)
	}
	m.hook.OnAuthUpdated(ctx, snapshot.Clone())
}

func (m *Manager) mergeRuntimeQuota(ctx context.Context, authID string, quota *QuotaInfo) {
	if m == nil || quota == nil {
		return
	}
	var snapshot *Auth
	m.mu.Lock()
	if auth := m.auths[authID]; auth != nil {
		auth.RuntimeQuota = MergeQuotaInfo(auth.RuntimeQuota, quota)
		auth.UpdatedAt = time.Now()
		snapshot = auth.Clone()
	}
	m.mu.Unlock()
	if snapshot == nil {
		return
	}
	if m.scheduler != nil {
		m.scheduler.upsertAuth(snapshot)
	}
	m.hook.OnAuthUpdated(ctx, snapshot.Clone())
}
