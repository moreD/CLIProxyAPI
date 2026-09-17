package auth

import (
	"context"
	"net/http"
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
		m.refreshQuotaAuth(ctx, authID, retryDelay, false)
	}
}

// RefreshQuotaAsync starts an immediate forced quota refresh for one auth. The
// result is written back to the manager and scheduler through storeQuotaRefreshResult.
func (m *Manager) RefreshQuotaAsync(ctx context.Context, authID string) {
	if m == nil || strings.TrimSpace(authID) == "" {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	go m.refreshQuotaAuth(context.WithoutCancel(ctx), authID, 0, true)
}

func (m *Manager) refreshQuotaAfterAuthChange(authID string) {
	if m == nil || strings.TrimSpace(authID) == "" {
		return
	}
	m.mu.RLock()
	ctx := m.refreshCtx
	m.mu.RUnlock()
	if ctx == nil || ctx.Err() != nil {
		return
	}
	go m.refreshQuotaAuth(ctx, authID, 0, true)
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
	if !quotaStateSupportedAuth(auth) {
		return false
	}
	exec := m.executors[auth.Provider]
	_, ok := exec.(QuotaRefresher)
	return ok
}

func (m *Manager) refreshQuotaAuth(ctx context.Context, authID string, retryDelay time.Duration, force bool) {
	if m == nil || strings.TrimSpace(authID) == "" {
		return
	}
	if m.refreshQuotaAuthAttempt(ctx, authID, force) {
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
	_ = m.refreshQuotaAuthAttempt(ctx, authID, force)
}

func (m *Manager) refreshQuotaAuthAttempt(ctx context.Context, authID string, force bool) bool {
	auth, refresher := m.quotaRefreshSnapshot(authID)
	if auth == nil || refresher == nil {
		return true
	}
	now := time.Now()
	if !force && !quotaRefreshStale(auth, backgroundQuotaMaxAge, now) {
		return true
	}
	previousQuota := auth.RuntimeQuota.Clone()
	log.WithField("auth_id", authID).Info("codex quota refresh started")
	quota, err := refresher.RefreshQuota(ctx, auth)
	if err != nil && ctx.Err() != nil {
		return true
	}
	stored := false
	if quota != nil && quota.HasAny() {
		m.storeQuotaRefreshResult(ctx, authID, quota, time.Now())
		stored = true
	}
	if err != nil {
		m.storeQuotaRefreshError(ctx, authID, err)
		if isUnauthorizedError(err) {
			log.WithField("auth_id", authID).Infof("codex quota refresh unauthorized; stopping background quota refresh: %v", err)
			m.markQuotaAuthUnauthorized(ctx, auth, err)
			return true
		}
		if !stored {
			log.WithField("auth_id", authID).Infof("codex quota refresh failed: %v", err)
		} else {
			log.WithField("auth_id", authID).Infof("codex quota refresh stored partial quota despite error: %v", err)
		}
		return stored
	}
	m.storeQuotaRefreshError(ctx, authID, nil)
	log.WithField("auth_id", authID).Info("codex quota refresh completed")
	m.refreshWorkspaceMetadata(ctx, authID, auth, refresher)
	if !weeklyQuotaWindowRefreshed(previousQuota, quota) {
		return stored
	}
	// Countdown probing generates tokens; quota metadata GETs above remain
	// available while a dollar cap is exhausted so its reset can be discovered.
	if blocked, _ := authCostBlocked(auth, time.Now()); blocked {
		return stored
	}
	log.WithField("auth_id", authID).Info("codex quota probe started")
	probeQuota, probeErr := refresher.ProbeQuotaCountdown(ctx, auth)
	if probeQuota != nil && probeQuota.HasAny() {
		stored = true
	}
	m.storeProbeResult(ctx, authID, probeQuota, time.Now())
	if probeErr != nil {
		m.storeQuotaRefreshError(ctx, authID, probeErr)
		if isUnauthorizedError(probeErr) {
			log.WithField("auth_id", authID).Infof("codex quota probe unauthorized; stopping background quota refresh: %v", probeErr)
			m.markQuotaAuthUnauthorized(ctx, auth, probeErr)
			return true
		}
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

func (m *Manager) refreshWorkspaceMetadata(ctx context.Context, authID string, auth *Auth, refresher QuotaRefresher) {
	workspaceRefresher, ok := refresher.(WorkspaceMetadataRefresher)
	if !ok || workspaceRefresher == nil || !workspaceMetadataMissing(auth) {
		return
	}
	workspaceNames, err := workspaceRefresher.RefreshWorkspaceMetadata(ctx, auth)
	if err != nil {
		log.WithField("auth_id", authID).Infof("codex workspace metadata refresh failed: %v", err)
		return
	}
	if updated := m.storeWorkspaceNames(ctx, workspaceNames); updated > 0 {
		log.WithField("auth_id", authID).Infof("codex workspace metadata refresh updated %d auths", updated)
	}
}

func workspaceMetadataMissing(auth *Auth) bool {
	if auth == nil || authWorkspaceName(auth) != "" {
		return false
	}
	planType := ""
	if auth.Attributes != nil {
		planType = strings.ToLower(strings.TrimSpace(auth.Attributes["plan_type"]))
	}
	switch planType {
	case "team", "business", "enterprise", "edu", "education":
		return true
	default:
		return false
	}
}

func (m *Manager) storeWorkspaceNames(ctx context.Context, workspaceNames map[string]string) int {
	if m == nil || len(workspaceNames) == 0 {
		return 0
	}
	normalized := make(map[string]string, len(workspaceNames))
	for accountID, workspaceName := range workspaceNames {
		accountID = strings.TrimSpace(accountID)
		workspaceName = strings.TrimSpace(workspaceName)
		if accountID != "" && workspaceName != "" {
			normalized[accountID] = workspaceName
		}
	}
	if len(normalized) == 0 {
		return 0
	}

	now := time.Now()
	snapshots := make([]*Auth, 0, len(normalized))
	m.mu.Lock()
	for _, auth := range m.auths {
		if auth == nil || !strings.EqualFold(strings.TrimSpace(auth.Provider), "codex") {
			continue
		}
		accountID := authMetadataString(auth, "account_id")
		if accountID == "" {
			accountID = authMetadataString(auth, "accountId")
		}
		workspaceName := normalized[accountID]
		if workspaceName == "" || authWorkspaceName(auth) == workspaceName {
			continue
		}
		if auth.Metadata == nil {
			auth.Metadata = make(map[string]any)
		}
		auth.Metadata[MetadataWorkspaceName] = workspaceName
		auth.UpdatedAt = now
		snapshots = append(snapshots, auth.Clone())
	}
	m.mu.Unlock()

	for _, snapshot := range snapshots {
		if m.scheduler != nil {
			m.scheduler.upsertAuth(snapshot)
		}
		if errPersist := m.persist(ctx, snapshot); errPersist != nil {
			log.WithField("auth_id", snapshot.ID).Warnf("persist Codex workspace name: %v", errPersist)
		}
		m.hook.OnAuthUpdated(ctx, snapshot.Clone())
	}
	return len(snapshots)
}

func authWorkspaceName(auth *Auth) string {
	if auth == nil || auth.Metadata == nil {
		return ""
	}
	workspaceName, _ := auth.Metadata[MetadataWorkspaceName].(string)
	return strings.TrimSpace(workspaceName)
}

func (m *Manager) storeQuotaRefreshError(ctx context.Context, authID string, refreshErr error) {
	if m == nil || strings.TrimSpace(authID) == "" {
		return
	}
	message := ""
	status := 0
	if refreshErr != nil {
		message = refreshErr.Error()
		status = statusCodeFromError(refreshErr)
		if status == 0 && isUnauthorizedError(refreshErr) {
			status = http.StatusUnauthorized
		}
	}

	var snapshot *Auth
	m.mu.Lock()
	if auth := m.auths[authID]; auth != nil && (auth.QuotaRefreshError != message || auth.QuotaRefreshErrorStatus != status) {
		auth.QuotaRefreshError = message
		auth.QuotaRefreshErrorStatus = status
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
	_ = m.persist(ctx, snapshot)
	m.hook.OnAuthUpdated(ctx, snapshot.Clone())
}

func weeklyQuotaWindowRefreshed(previous, current *QuotaInfo) bool {
	if current == nil || !current.Weekly.known() {
		return false
	}
	if previous == nil || !previous.Weekly.known() {
		return true
	}
	if !current.Weekly.NextFreshAt.IsZero() && current.Weekly.NextFreshAt.After(previous.Weekly.NextFreshAt) {
		return true
	}
	return current.Weekly.Used > 0 && previous.Weekly.Used > 0 && current.Weekly.Used < previous.Weekly.Used
}

func (m *Manager) markQuotaAuthUnauthorized(ctx context.Context, auth *Auth, err error) {
	if m == nil || auth == nil || err == nil {
		return
	}
	m.MarkResult(ctx, Result{
		AuthID:   auth.ID,
		Provider: auth.Provider,
		Success:  false,
		Error:    refreshErrorFromError(err),
	})
}

func (m *Manager) storeQuotaRefreshResult(ctx context.Context, authID string, quota *QuotaInfo, seenAt time.Time) *QuotaInfo {
	if m == nil || strings.TrimSpace(authID) == "" {
		return nil
	}
	if seenAt.IsZero() {
		seenAt = time.Now()
	}
	var snapshot *Auth
	var merged *QuotaInfo
	m.mu.Lock()
	if auth := m.auths[authID]; auth != nil {
		if quota != nil && quota.HasAny() {
			auth.RuntimeQuota = quota.Clone()
		}
		auth.LastQuotaSeenAt = seenAt
		merged = auth.RuntimeQuota.Clone()
		auth.UpdatedAt = seenAt
		snapshot = auth.Clone()
	}
	m.mu.Unlock()
	if snapshot == nil {
		return merged
	}
	_ = ObserveAuthCostWindow(snapshot, seenAt)
	if m.scheduler != nil {
		m.scheduler.upsertAuth(snapshot)
	}
	_ = m.persist(ctx, snapshot)
	m.hook.OnAuthUpdated(ctx, snapshot.Clone())
	return merged
}

func (m *Manager) storeProbeResult(ctx context.Context, authID string, quota *QuotaInfo, probedAt time.Time) *QuotaInfo {
	if m == nil || strings.TrimSpace(authID) == "" {
		return nil
	}
	if probedAt.IsZero() {
		probedAt = time.Now()
	}
	var snapshot *Auth
	var merged *QuotaInfo
	m.mu.Lock()
	if auth := m.auths[authID]; auth != nil {
		auth.LastProbedAt = probedAt
		if quota != nil && quota.HasAny() {
			auth.RuntimeQuota = MergeQuotaInfo(auth.RuntimeQuota, quota)
		}
		merged = auth.RuntimeQuota.Clone()
		auth.UpdatedAt = probedAt
		snapshot = auth.Clone()
	}
	m.mu.Unlock()
	if snapshot == nil {
		return merged
	}
	_ = ObserveAuthCostWindow(snapshot, probedAt)
	if m.scheduler != nil {
		m.scheduler.upsertAuth(snapshot)
	}
	_ = m.persist(ctx, snapshot)
	m.hook.OnAuthUpdated(ctx, snapshot.Clone())
	return merged
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
