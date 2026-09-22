package auth

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

type quotaRefreshTestExecutor struct {
	provider string

	mu           sync.Mutex
	refreshCalls []string
	probeCalls   []string
	executeCalls []string
	refreshQueue []quotaRefreshResult
	probeQueue   []quotaProbeResult
}

type quotaRefreshResult struct {
	quota *QuotaInfo
	err   error
}

type quotaProbeResult struct {
	quota *QuotaInfo
	err   error
}

func (e *quotaRefreshTestExecutor) Identifier() string {
	if e.provider != "" {
		return e.provider
	}
	return "codex"
}

func (e *quotaRefreshTestExecutor) Execute(_ context.Context, auth *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.executeCalls = append(e.executeCalls, auth.ID)
	return cliproxyexecutor.Response{Payload: []byte(auth.ID)}, nil
}

func (e *quotaRefreshTestExecutor) ExecuteStream(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	return nil, nil
}

func (e *quotaRefreshTestExecutor) Refresh(_ context.Context, auth *Auth) (*Auth, error) {
	return auth, nil
}

func (e *quotaRefreshTestExecutor) CountTokens(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}

func (e *quotaRefreshTestExecutor) HttpRequest(context.Context, *Auth, *http.Request) (*http.Response, error) {
	return nil, nil
}

func (e *quotaRefreshTestExecutor) RefreshQuota(_ context.Context, auth *Auth) (*QuotaInfo, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.refreshCalls = append(e.refreshCalls, auth.ID)
	if len(e.refreshQueue) == 0 {
		return nil, nil
	}
	result := e.refreshQueue[0]
	e.refreshQueue = e.refreshQueue[1:]
	return result.quota, result.err
}

func (e *quotaRefreshTestExecutor) ProbeQuotaCountdown(_ context.Context, auth *Auth) (*QuotaInfo, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.probeCalls = append(e.probeCalls, auth.ID)
	if len(e.probeQueue) == 0 {
		return nil, nil
	}
	result := e.probeQueue[0]
	e.probeQueue = e.probeQueue[1:]
	return result.quota, result.err
}

func TestStoreWorkspaceNamesUpdatesMatchingCodexAuths(t *testing.T) {
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	for _, auth := range []*Auth{
		{ID: "team-a", Provider: "codex", Metadata: map[string]any{"account_id": "acct-a"}},
		{ID: "team-b", Provider: "codex", Metadata: map[string]any{"account_id": "acct-b", MetadataWorkspaceName: "Old Name"}},
		{ID: "other", Provider: "gemini-cli", Metadata: map[string]any{"account_id": "acct-a"}},
	} {
		if _, errRegister := manager.Register(WithSkipPersist(context.Background()), auth); errRegister != nil {
			t.Fatal(errRegister)
		}
	}

	updated := manager.storeWorkspaceNames(context.Background(), map[string]string{
		"acct-a": "Workspace A",
		"acct-b": " Workspace B ",
		"acct-c": "",
	})
	if updated != 2 {
		t.Fatalf("storeWorkspaceNames() = %d, want 2", updated)
	}
	for authID, want := range map[string]string{"team-a": "Workspace A", "team-b": "Workspace B"} {
		auth, ok := manager.GetByID(authID)
		if !ok {
			t.Fatalf("GetByID(%q) not found", authID)
		}
		if got := authWorkspaceName(auth); got != want {
			t.Fatalf("authWorkspaceName(%q) = %q, want %q", authID, got, want)
		}
	}
	other, _ := manager.GetByID("other")
	if got := authWorkspaceName(other); got != "" {
		t.Fatalf("non-Codex workspace name = %q, want empty", got)
	}
}

func TestWorkspaceMetadataMissingOnlyForNamedWorkspacePlans(t *testing.T) {
	for _, test := range []struct {
		name string
		auth *Auth
		want bool
	}{
		{name: "team without name", auth: &Auth{Attributes: map[string]string{"plan_type": "team"}}, want: true},
		{name: "enterprise without name", auth: &Auth{Attributes: map[string]string{"plan_type": "Enterprise"}}, want: true},
		{name: "team with name", auth: &Auth{Attributes: map[string]string{"plan_type": "team"}, Metadata: map[string]any{MetadataWorkspaceName: "Workspace"}}},
		{name: "personal plan", auth: &Auth{Attributes: map[string]string{"plan_type": "plus"}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := workspaceMetadataMissing(test.auth); got != test.want {
				t.Fatalf("workspaceMetadataMissing() = %v, want %v", got, test.want)
			}
		})
	}
}

func TestStartAutoRefreshStartsQuotaRefreshLoop(t *testing.T) {
	now := time.Now()
	exec := &quotaRefreshTestExecutor{
		refreshQueue: []quotaRefreshResult{{
			quota: testQuota(now.Add(time.Hour), 17),
		}},
	}
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	manager.RegisterExecutor(exec)
	if _, errRegister := manager.Register(context.Background(), &Auth{
		ID:       "oauth",
		Provider: "codex",
		Metadata: map[string]any{"email": "a@example.com"},
	}); errRegister != nil {
		t.Fatalf("Register(oauth) error = %v", errRegister)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	manager.StartAutoRefresh(ctx, time.Hour)
	defer manager.StopAutoRefresh()

	deadline := time.After(2 * time.Second)
	for {
		updated, ok := manager.GetByID("oauth")
		if ok && updated.RuntimeQuota != nil && updated.RuntimeQuota.Weekly.UsedPercent == 17 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("timed out waiting for automatic quota refresh")
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func TestRegisterRefreshesQuotaImmediatelyAfterAutoRefreshStarts(t *testing.T) {
	now := time.Now()
	exec := &quotaRefreshTestExecutor{
		refreshQueue: []quotaRefreshResult{{
			quota: testQuota(now.Add(time.Hour), 21),
		}},
	}
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	manager.RegisterExecutor(exec)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	manager.StartAutoRefresh(ctx, time.Hour)
	defer manager.StopAutoRefresh()

	if _, errRegister := manager.Register(context.Background(), &Auth{
		ID:              "new-oauth",
		Provider:        "codex",
		LastRefreshedAt: now,
		Metadata: map[string]any{
			"email":         "new@example.com",
			"access_token":  "new-access-token",
			"refresh_token": "new-refresh-token",
		},
	}); errRegister != nil {
		t.Fatalf("Register(new-oauth) error = %v", errRegister)
	}

	deadline := time.After(2 * time.Second)
	for {
		updated, ok := manager.GetByID("new-oauth")
		if ok && updated.RuntimeQuota != nil && updated.RuntimeQuota.Weekly.UsedPercent == 21 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("timed out waiting for quota refresh after registration")
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func TestCredentialUpdateForcesFreshQuotaRefresh(t *testing.T) {
	now := time.Now()
	exec := &quotaRefreshTestExecutor{
		refreshQueue: []quotaRefreshResult{{
			quota: testQuota(now.Add(2*time.Hour), 34),
		}},
	}
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	manager.RegisterExecutor(exec)
	if _, errRegister := manager.Register(context.Background(), &Auth{
		ID:              "oauth",
		Provider:        "codex",
		LastRefreshedAt: now,
		LastQuotaSeenAt: now,
		RuntimeQuota:    testQuota(now.Add(time.Hour), 80),
		Metadata: map[string]any{
			"email":         "a@example.com",
			"access_token":  "old-access-token",
			"refresh_token": "old-refresh-token",
		},
	}); errRegister != nil {
		t.Fatalf("Register(oauth) error = %v", errRegister)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	manager.StartAutoRefresh(ctx, time.Hour)
	defer manager.StopAutoRefresh()
	if _, errUpdate := manager.Update(context.Background(), &Auth{
		ID:              "oauth",
		Provider:        "codex",
		LastRefreshedAt: now,
		Metadata: map[string]any{
			"email":         "a@example.com",
			"access_token":  "replacement-access-token",
			"refresh_token": "replacement-refresh-token",
		},
	}); errUpdate != nil {
		t.Fatalf("Update(oauth) error = %v", errUpdate)
	}

	deadline := time.After(2 * time.Second)
	for {
		updated, ok := manager.GetByID("oauth")
		if ok && updated.RuntimeQuota != nil && updated.RuntimeQuota.Weekly.UsedPercent == 34 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("timed out waiting for forced quota refresh after credential update")
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func TestQuotaRefreshCredentialChangedIgnoresUnrelatedMetadata(t *testing.T) {
	existing := &Auth{
		Provider: "codex",
		Metadata: map[string]any{
			"access_token":  "access-token",
			"refresh_token": "refresh-token",
			"email":         "old@example.com",
		},
	}
	updated := existing.Clone()
	updated.Metadata["email"] = "new@example.com"
	if quotaRefreshCredentialChanged(existing, updated) {
		t.Fatal("email-only update unexpectedly marked quota credentials as changed")
	}
	updated.Metadata["access_token"] = "replacement-access-token"
	if !quotaRefreshCredentialChanged(existing, updated) {
		t.Fatal("access-token update was not marked as a quota credential change")
	}
}

func TestQuotaRefreshLoopFiltersAndStoresProbeQuota(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	exec := &quotaRefreshTestExecutor{
		refreshQueue: []quotaRefreshResult{{
			quota: testQuota(now.Add(time.Hour), 50),
		}},
		probeQueue: []quotaProbeResult{{
			quota: testQuota(now.Add(2*time.Hour), 90),
		}},
	}
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	manager.RegisterExecutor(exec)
	entries := []*Auth{
		{ID: "oauth", Provider: "codex", Metadata: map[string]any{"email": "a@example.com"}},
		{ID: "disabled", Provider: "codex", Disabled: true, Metadata: map[string]any{"email": "b@example.com"}},
		{ID: "apikey", Provider: "codex", Attributes: map[string]string{"api_key": "sk-test"}},
		{ID: "gemini", Provider: "gemini", Metadata: map[string]any{"email": "g@example.com"}},
	}
	for _, auth := range entries {
		if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
			t.Fatalf("Register(%s) error = %v", auth.ID, errRegister)
		}
	}

	manager.runQuotaRefreshCycle(context.Background(), 0)

	exec.mu.Lock()
	refreshCalls := append([]string(nil), exec.refreshCalls...)
	probeCalls := append([]string(nil), exec.probeCalls...)
	exec.mu.Unlock()
	if got := fmt.Sprint(refreshCalls); got != "[oauth]" {
		t.Fatalf("refresh calls = %s, want [oauth]", got)
	}
	if got := fmt.Sprint(probeCalls); got != "[oauth]" {
		t.Fatalf("probe calls = %s, want [oauth]", got)
	}

	manager.mu.RLock()
	runtimeQuota := manager.auths["oauth"].RuntimeQuota.Clone()
	manager.mu.RUnlock()
	if runtimeQuota == nil || runtimeQuota.Weekly.UsedPercent != 90 {
		t.Fatalf("stored runtime quota = %#v, want probe quota with 90 percent", runtimeQuota)
	}
}

func TestQuotaRefreshLoopSkipsFreshStoredQuota(t *testing.T) {
	t.Parallel()

	now := time.Now()
	exec := &quotaRefreshTestExecutor{}
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	manager.RegisterExecutor(exec)
	if _, errRegister := manager.Register(context.Background(), &Auth{
		ID:              "oauth",
		Provider:        "codex",
		Metadata:        map[string]any{"email": "a@example.com"},
		LastQuotaSeenAt: now,
		RuntimeQuota:    testQuota(now.Add(time.Hour), 80),
	}); errRegister != nil {
		t.Fatalf("Register(oauth) error = %v", errRegister)
	}

	manager.runQuotaRefreshCycle(context.Background(), 0)

	exec.mu.Lock()
	refreshCalls := append([]string(nil), exec.refreshCalls...)
	probeCalls := append([]string(nil), exec.probeCalls...)
	exec.mu.Unlock()
	if got := fmt.Sprint(refreshCalls); got != "[]" {
		t.Fatalf("refresh calls = %s, want none", got)
	}
	if got := fmt.Sprint(probeCalls); got != "[]" {
		t.Fatalf("probe calls = %s, want none", got)
	}
}

func TestRefreshQuotaAsyncStoresQuotaForSchedulerSelection(t *testing.T) {
	t.Parallel()

	now := time.Now()
	exec := &quotaRefreshTestExecutor{
		refreshQueue: []quotaRefreshResult{{
			quota: testQuota(now.Add(time.Hour), 5),
		}},
	}
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	manager.RegisterExecutor(exec)
	if _, errRegister := manager.Register(context.Background(), &Auth{
		ID:              "used",
		Provider:        "codex",
		Metadata:        map[string]any{"email": "used@example.com"},
		LastQuotaSeenAt: now,
		RuntimeQuota:    testQuota(now.Add(time.Hour), 80),
	}); errRegister != nil {
		t.Fatalf("Register(used) error = %v", errRegister)
	}

	got, errPick := manager.scheduler.pickSingle(context.Background(), "codex", "", cliproxyexecutor.Options{}, nil)
	if errPick != nil {
		t.Fatalf("initial pickSingle() error = %v", errPick)
	}
	if got == nil || got.ID != "used" {
		t.Fatalf("initial pickSingle() = %v, want used", got)
	}

	if _, errRegister := manager.Register(context.Background(), &Auth{
		ID:       "new",
		Provider: "codex",
		Metadata: map[string]any{"email": "new@example.com"},
	}); errRegister != nil {
		t.Fatalf("Register(new) error = %v", errRegister)
	}
	manager.RefreshQuotaAsync(context.Background(), "new")

	deadline := time.After(2 * time.Second)
	for {
		updated, ok := manager.GetByID("new")
		if ok && updated.RuntimeQuota != nil && updated.RuntimeQuota.Weekly.UsedPercent == 5 {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for async quota refresh")
		case <-time.After(10 * time.Millisecond):
		}
	}

	got, errPick = manager.scheduler.pickSingle(context.Background(), "codex", "", cliproxyexecutor.Options{}, nil)
	if errPick != nil {
		t.Fatalf("pickSingle() after async quota refresh error = %v", errPick)
	}
	if got == nil || got.ID != "new" {
		t.Fatalf("pickSingle() after async quota refresh = %v, want new", got)
	}
}

func TestQuotaRefreshLoopProbesOnlyWhenWeeklyWindowRefreshes(t *testing.T) {
	t.Parallel()

	now := time.Now()
	oldQuota := testQuota(now.Add(time.Hour), 40)
	newQuota := testQuota(now.Add(time.Hour), 50)
	exec := &quotaRefreshTestExecutor{
		refreshQueue: []quotaRefreshResult{{quota: newQuota}},
	}
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	manager.RegisterExecutor(exec)
	if _, errRegister := manager.Register(context.Background(), &Auth{
		ID:              "oauth",
		Provider:        "codex",
		Metadata:        map[string]any{"email": "a@example.com"},
		LastQuotaSeenAt: now.Add(-20 * time.Minute),
		RuntimeQuota:    oldQuota,
	}); errRegister != nil {
		t.Fatalf("Register(oauth) error = %v", errRegister)
	}

	manager.runQuotaRefreshCycle(context.Background(), 0)

	exec.mu.Lock()
	refreshCalls := append([]string(nil), exec.refreshCalls...)
	probeCalls := append([]string(nil), exec.probeCalls...)
	exec.mu.Unlock()
	if got := fmt.Sprint(refreshCalls); got != "[oauth]" {
		t.Fatalf("refresh calls = %s, want [oauth]", got)
	}
	if got := fmt.Sprint(probeCalls); got != "[]" {
		t.Fatalf("probe calls = %s, want none", got)
	}
}

func TestQuotaRefreshLoopProbesWhenWeeklyWindowAdvances(t *testing.T) {
	t.Parallel()

	now := time.Now()
	oldQuota := testQuota(now.Add(time.Hour), 10)
	newQuota := testQuota(now.Add(7*24*time.Hour), 100)
	exec := &quotaRefreshTestExecutor{
		refreshQueue: []quotaRefreshResult{{quota: newQuota}},
		probeQueue:   []quotaProbeResult{{quota: newQuota}},
	}
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	manager.RegisterExecutor(exec)
	if _, errRegister := manager.Register(context.Background(), &Auth{
		ID:              "oauth",
		Provider:        "codex",
		Metadata:        map[string]any{"email": "a@example.com"},
		LastQuotaSeenAt: now.Add(-20 * time.Minute),
		RuntimeQuota:    oldQuota,
	}); errRegister != nil {
		t.Fatalf("Register(oauth) error = %v", errRegister)
	}

	manager.runQuotaRefreshCycle(context.Background(), 0)

	exec.mu.Lock()
	refreshCalls := append([]string(nil), exec.refreshCalls...)
	probeCalls := append([]string(nil), exec.probeCalls...)
	exec.mu.Unlock()
	if got := fmt.Sprint(refreshCalls); got != "[oauth]" {
		t.Fatalf("refresh calls = %s, want [oauth]", got)
	}
	if got := fmt.Sprint(probeCalls); got != "[oauth]" {
		t.Fatalf("probe calls = %s, want [oauth]", got)
	}
}

func TestQuotaRefreshLoopRetriesOnceAndPreservesPreviousQuota(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	oldQuota := testQuota(now.Add(time.Hour), 40)
	exec := &quotaRefreshTestExecutor{
		refreshQueue: []quotaRefreshResult{
			{err: fmt.Errorf("temporary refresh failure")},
			{err: fmt.Errorf("temporary refresh failure again")},
		},
	}
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	manager.RegisterExecutor(exec)
	if _, errRegister := manager.Register(context.Background(), &Auth{
		ID:           "oauth",
		Provider:     "codex",
		Metadata:     map[string]any{"email": "a@example.com"},
		RuntimeQuota: oldQuota,
	}); errRegister != nil {
		t.Fatalf("Register(oauth) error = %v", errRegister)
	}

	manager.runQuotaRefreshCycle(context.Background(), 0)

	exec.mu.Lock()
	refreshCalls := append([]string(nil), exec.refreshCalls...)
	exec.mu.Unlock()
	if got := fmt.Sprint(refreshCalls); got != "[oauth oauth]" {
		t.Fatalf("refresh calls = %s, want retry once", got)
	}
	manager.mu.RLock()
	runtimeQuota := manager.auths["oauth"].RuntimeQuota.Clone()
	manager.mu.RUnlock()
	if runtimeQuota == nil || runtimeQuota.Weekly.UsedPercent != oldQuota.Weekly.UsedPercent {
		t.Fatalf("runtime quota = %#v, want previous quota preserved", runtimeQuota)
	}
	updated, ok := manager.GetByID("oauth")
	if !ok || updated.QuotaRefreshError != "temporary refresh failure again" {
		t.Fatalf("quota refresh error = %q, want latest retry error", updated.QuotaRefreshError)
	}
}

func TestQuotaRefreshLoopClearsPreviousRefreshErrorOnSuccess(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	exec := &quotaRefreshTestExecutor{refreshQueue: []quotaRefreshResult{{
		quota: testFiveHourQuota(now.Add(time.Hour), 25),
	}}}
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	manager.RegisterExecutor(exec)
	if _, errRegister := manager.Register(context.Background(), &Auth{
		ID:                      "oauth",
		Provider:                "codex",
		Metadata:                map[string]any{"email": "a@example.com"},
		QuotaRefreshError:       "previous failure",
		QuotaRefreshErrorStatus: http.StatusUnauthorized,
	}); errRegister != nil {
		t.Fatalf("Register(oauth) error = %v", errRegister)
	}

	manager.runQuotaRefreshCycle(context.Background(), 0)

	updated, ok := manager.GetByID("oauth")
	if !ok {
		t.Fatal("expected oauth auth")
	}
	if updated.QuotaRefreshError != "" || updated.QuotaRefreshErrorStatus != 0 {
		t.Fatalf("quota refresh error = (%q, %d), want cleared", updated.QuotaRefreshError, updated.QuotaRefreshErrorStatus)
	}
}

func TestQuotaRefreshLoopUnauthorizedRefreshStopsFutureRefresh(t *testing.T) {
	t.Parallel()

	exec := &quotaRefreshTestExecutor{
		refreshQueue: []quotaRefreshResult{
			{err: fmt.Errorf("codex quota refresh: status 401: invalidated")},
			{err: fmt.Errorf("should not be retried")},
		},
	}
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	manager.RegisterExecutor(exec)
	if _, errRegister := manager.Register(context.Background(), &Auth{
		ID:       "oauth",
		Provider: "codex",
		Metadata: map[string]any{"email": "a@example.com"},
	}); errRegister != nil {
		t.Fatalf("Register(oauth) error = %v", errRegister)
	}

	manager.runQuotaRefreshCycle(context.Background(), 0)
	manager.runQuotaRefreshCycle(context.Background(), 0)

	exec.mu.Lock()
	refreshCalls := append([]string(nil), exec.refreshCalls...)
	probeCalls := append([]string(nil), exec.probeCalls...)
	exec.mu.Unlock()
	if got := fmt.Sprint(refreshCalls); got != "[oauth]" {
		t.Fatalf("refresh calls = %s, want single unauthorized attempt", got)
	}
	if got := fmt.Sprint(probeCalls); got != "[]" {
		t.Fatalf("probe calls = %s, want none", got)
	}
	updated, ok := manager.GetByID("oauth")
	if !ok {
		t.Fatal("expected oauth auth")
	}
	if !hasUnauthorizedAuthFailure(updated) {
		t.Fatalf("auth last error = %#v, want unauthorized", updated.LastError)
	}
	if got := updated.QuotaRefreshError; got != "codex quota refresh: status 401: invalidated" {
		t.Fatalf("quota refresh error = %q, want unauthorized refresh message", got)
	}
	if got := updated.QuotaRefreshErrorStatus; got != http.StatusUnauthorized {
		t.Fatalf("quota refresh error status = %d, want %d", got, http.StatusUnauthorized)
	}
}

func TestQuotaRefreshLoopUnauthorizedProbeStopsFutureRefresh(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	exec := &quotaRefreshTestExecutor{
		refreshQueue: []quotaRefreshResult{
			{quota: testQuota(now.Add(time.Hour), 50)},
			{err: fmt.Errorf("should not refresh again")},
		},
		probeQueue: []quotaProbeResult{
			{err: fmt.Errorf("codex quota probe: status 401: invalidated")},
		},
	}
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	manager.RegisterExecutor(exec)
	if _, errRegister := manager.Register(context.Background(), &Auth{
		ID:       "oauth",
		Provider: "codex",
		Metadata: map[string]any{"email": "a@example.com"},
	}); errRegister != nil {
		t.Fatalf("Register(oauth) error = %v", errRegister)
	}

	manager.runQuotaRefreshCycle(context.Background(), 0)
	manager.runQuotaRefreshCycle(context.Background(), 0)

	exec.mu.Lock()
	refreshCalls := append([]string(nil), exec.refreshCalls...)
	probeCalls := append([]string(nil), exec.probeCalls...)
	exec.mu.Unlock()
	if got := fmt.Sprint(refreshCalls); got != "[oauth]" {
		t.Fatalf("refresh calls = %s, want single refresh before unauthorized probe", got)
	}
	if got := fmt.Sprint(probeCalls); got != "[oauth]" {
		t.Fatalf("probe calls = %s, want single unauthorized probe", got)
	}
	updated, ok := manager.GetByID("oauth")
	if !ok {
		t.Fatal("expected oauth auth")
	}
	if !hasUnauthorizedAuthFailure(updated) {
		t.Fatalf("auth last error = %#v, want unauthorized", updated.LastError)
	}
}

func TestRequestQuotaRefreshRunsBeforeStaleAuthRequest(t *testing.T) {
	t.Parallel()

	now := time.Now()
	exec := &quotaRefreshTestExecutor{
		refreshQueue: []quotaRefreshResult{{
			quota: testFiveHourQuota(now.Add(time.Hour), 80),
		}},
	}
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	manager.RegisterExecutor(exec)
	if _, errRegister := manager.Register(context.Background(), &Auth{
		ID:       "oauth",
		Provider: "codex",
		Metadata: map[string]any{"email": "a@example.com"},
	}); errRegister != nil {
		t.Fatalf("Register(oauth) error = %v", errRegister)
	}

	resp, errExec := manager.Execute(context.Background(), []string{"codex"}, cliproxyexecutor.Request{}, cliproxyexecutor.Options{})
	if errExec != nil {
		t.Fatalf("Execute returned error: %v", errExec)
	}
	if string(resp.Payload) != "oauth" {
		t.Fatalf("response payload = %q, want oauth", string(resp.Payload))
	}

	exec.mu.Lock()
	refreshCalls := append([]string(nil), exec.refreshCalls...)
	probeCalls := append([]string(nil), exec.probeCalls...)
	executeCalls := append([]string(nil), exec.executeCalls...)
	exec.mu.Unlock()
	if got := fmt.Sprint(refreshCalls); got != "[oauth]" {
		t.Fatalf("refresh calls = %s, want [oauth]", got)
	}
	if got := fmt.Sprint(probeCalls); got != "[]" {
		t.Fatalf("probe calls = %s, want none", got)
	}
	if got := fmt.Sprint(executeCalls); got != "[oauth]" {
		t.Fatalf("execute calls = %s, want [oauth]", got)
	}
	updated, ok := manager.GetByID("oauth")
	if !ok || updated.LastQuotaSeenAt.IsZero() {
		t.Fatalf("LastQuotaSeenAt = %v, want recorded quota refresh time", updated)
	}
	if updated.RuntimeQuota == nil || updated.RuntimeQuota.FiveHour.UsedPercent != 80 {
		t.Fatalf("RuntimeQuota = %#v, want request quota refresh", updated.RuntimeQuota)
	}
}

func TestRequestQuotaRefreshSkipsFreshAuth(t *testing.T) {
	t.Parallel()

	exec := &quotaRefreshTestExecutor{}
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	manager.RegisterExecutor(exec)
	if _, errRegister := manager.Register(context.Background(), &Auth{
		ID:              "oauth",
		Provider:        "codex",
		Metadata:        map[string]any{"email": "a@example.com"},
		LastQuotaSeenAt: time.Now(),
		RuntimeQuota:    testFiveHourQuota(time.Now().Add(time.Hour), 80),
	}); errRegister != nil {
		t.Fatalf("Register(oauth) error = %v", errRegister)
	}

	if _, errExec := manager.Execute(context.Background(), []string{"codex"}, cliproxyexecutor.Request{}, cliproxyexecutor.Options{}); errExec != nil {
		t.Fatalf("Execute returned error: %v", errExec)
	}

	exec.mu.Lock()
	refreshCalls := append([]string(nil), exec.refreshCalls...)
	probeCalls := append([]string(nil), exec.probeCalls...)
	executeCalls := append([]string(nil), exec.executeCalls...)
	exec.mu.Unlock()
	if got := fmt.Sprint(refreshCalls); got != "[]" {
		t.Fatalf("refresh calls = %s, want none", got)
	}
	if got := fmt.Sprint(probeCalls); got != "[]" {
		t.Fatalf("probe calls = %s, want none", got)
	}
	if got := fmt.Sprint(executeCalls); got != "[oauth]" {
		t.Fatalf("execute calls = %s, want [oauth]", got)
	}
}

func TestRequestQuotaRefreshSwitchesWhenFiveHourUsedAtThreshold(t *testing.T) {
	t.Parallel()

	now := time.Now()
	exec := &quotaRefreshTestExecutor{
		refreshQueue: []quotaRefreshResult{{
			quota: testFiveHourQuota(now.Add(time.Hour), 90),
		}},
	}
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	manager.RegisterExecutor(exec)
	auths := []*Auth{
		{
			ID:       "a-high",
			Provider: "codex",
			Metadata: map[string]any{"email": "high@example.com"},
		},
		{
			ID:              "b-low",
			Provider:        "codex",
			Metadata:        map[string]any{"email": "low@example.com"},
			LastQuotaSeenAt: now,
			RuntimeQuota:    testFiveHourQuota(now.Add(time.Hour), 20),
		},
	}
	for _, auth := range auths {
		if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
			t.Fatalf("Register(%s) error = %v", auth.ID, errRegister)
		}
	}

	resp, errExec := manager.Execute(context.Background(), []string{"codex"}, cliproxyexecutor.Request{}, cliproxyexecutor.Options{})
	if errExec != nil {
		t.Fatalf("Execute returned error: %v", errExec)
	}
	if string(resp.Payload) != "b-low" {
		t.Fatalf("response payload = %q, want b-low", string(resp.Payload))
	}

	exec.mu.Lock()
	refreshCalls := append([]string(nil), exec.refreshCalls...)
	probeCalls := append([]string(nil), exec.probeCalls...)
	executeCalls := append([]string(nil), exec.executeCalls...)
	exec.mu.Unlock()
	if got := fmt.Sprint(refreshCalls); got != "[a-high]" {
		t.Fatalf("refresh calls = %s, want [a-high]", got)
	}
	if got := fmt.Sprint(probeCalls); got != "[]" {
		t.Fatalf("probe calls = %s, want none", got)
	}
	if got := fmt.Sprint(executeCalls); got != "[b-low]" {
		t.Fatalf("execute calls = %s, want [b-low]", got)
	}

	high, ok := manager.GetByID("a-high")
	if !ok {
		t.Fatal("expected a-high auth")
	}
	if !high.Unavailable || !high.Quota.Exceeded || !high.NextRetryAfter.After(time.Now()) {
		t.Fatalf("high auth state = unavailable:%v quota:%#v next:%v, want shared quota cooldown", high.Unavailable, high.Quota, high.NextRetryAfter)
	}
	blocked, reason, _ := isAuthBlockedForModel(high, "gpt-5.5", time.Now())
	if !blocked || reason != blockReasonCooldown {
		t.Fatalf("blocked=%v reason=%v, want quota cooldown", blocked, reason)
	}
}

func TestRequestQuotaRefreshSwitchesWhenWeeklyUsedAtThreshold(t *testing.T) {
	t.Parallel()

	now := time.Now()
	exec := &quotaRefreshTestExecutor{
		refreshQueue: []quotaRefreshResult{{
			quota: testQuotaWindows(now.Add(time.Hour), 20, now.Add(7*24*time.Hour), 98),
		}},
	}
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	manager.RegisterExecutor(exec)
	auths := []*Auth{
		{
			ID:       "a-high-weekly",
			Provider: "codex",
			Metadata: map[string]any{"email": "high-weekly@example.com"},
		},
		{
			ID:              "b-low",
			Provider:        "codex",
			Metadata:        map[string]any{"email": "low@example.com"},
			LastQuotaSeenAt: now,
			RuntimeQuota:    testQuotaWindows(now.Add(time.Hour), 20, now.Add(7*24*time.Hour), 20),
		},
	}
	for _, auth := range auths {
		if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
			t.Fatalf("Register(%s) error = %v", auth.ID, errRegister)
		}
	}

	resp, errExec := manager.Execute(context.Background(), []string{"codex"}, cliproxyexecutor.Request{}, cliproxyexecutor.Options{})
	if errExec != nil {
		t.Fatalf("Execute returned error: %v", errExec)
	}
	if string(resp.Payload) != "b-low" {
		t.Fatalf("response payload = %q, want b-low", string(resp.Payload))
	}

	exec.mu.Lock()
	refreshCalls := append([]string(nil), exec.refreshCalls...)
	probeCalls := append([]string(nil), exec.probeCalls...)
	executeCalls := append([]string(nil), exec.executeCalls...)
	exec.mu.Unlock()
	if got := fmt.Sprint(refreshCalls); got != "[a-high-weekly]" {
		t.Fatalf("refresh calls = %s, want [a-high-weekly]", got)
	}
	if got := fmt.Sprint(probeCalls); got != "[]" {
		t.Fatalf("probe calls = %s, want none", got)
	}
	if got := fmt.Sprint(executeCalls); got != "[b-low]" {
		t.Fatalf("execute calls = %s, want [b-low]", got)
	}
	high, ok := manager.GetByID("a-high-weekly")
	if !ok {
		t.Fatal("expected a-high-weekly auth")
	}
	if !high.Unavailable || !high.Quota.Exceeded || !high.NextRetryAfter.After(now.Add(6*24*time.Hour)) {
		t.Fatalf("high auth state = unavailable:%v quota:%#v next:%v, want weekly quota cooldown", high.Unavailable, high.Quota, high.NextRetryAfter)
	}
}

func TestRequestQuotaRefreshContinuesWhenWeeklyUsedBelowThreshold(t *testing.T) {
	t.Parallel()

	now := time.Now()
	exec := &quotaRefreshTestExecutor{
		refreshQueue: []quotaRefreshResult{{
			quota: testQuotaWindows(now.Add(time.Hour), 20, now.Add(7*24*time.Hour), 97),
		}},
	}
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	manager.RegisterExecutor(exec)
	if _, errRegister := manager.Register(context.Background(), &Auth{
		ID:       "oauth",
		Provider: "codex",
		Metadata: map[string]any{"email": "a@example.com"},
	}); errRegister != nil {
		t.Fatalf("Register(oauth) error = %v", errRegister)
	}

	resp, errExec := manager.Execute(context.Background(), []string{"codex"}, cliproxyexecutor.Request{}, cliproxyexecutor.Options{})
	if errExec != nil {
		t.Fatalf("Execute returned error: %v", errExec)
	}
	if string(resp.Payload) != "oauth" {
		t.Fatalf("response payload = %q, want oauth", string(resp.Payload))
	}
}

func TestRequestQuotaRefreshFailureStillUsesStoredHighQuota(t *testing.T) {
	t.Parallel()

	now := time.Now()
	exec := &quotaRefreshTestExecutor{
		refreshQueue: []quotaRefreshResult{{
			err: fmt.Errorf("temporary quota refresh failure"),
		}},
	}
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	manager.RegisterExecutor(exec)
	auths := []*Auth{
		{
			ID:              "a-high",
			Provider:        "codex",
			Metadata:        map[string]any{"email": "high@example.com"},
			LastQuotaSeenAt: now.Add(-10 * time.Minute),
			RuntimeQuota:    testFiveHourQuota(now.Add(time.Hour), 95),
		},
		{
			ID:              "b-low",
			Provider:        "codex",
			Metadata:        map[string]any{"email": "low@example.com"},
			LastQuotaSeenAt: now,
			RuntimeQuota:    testFiveHourQuota(now.Add(time.Hour), 20),
		},
	}
	for _, auth := range auths {
		if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
			t.Fatalf("Register(%s) error = %v", auth.ID, errRegister)
		}
	}

	resp, errExec := manager.Execute(context.Background(), []string{"codex"}, cliproxyexecutor.Request{}, cliproxyexecutor.Options{})
	if errExec != nil {
		t.Fatalf("Execute returned error: %v", errExec)
	}
	if string(resp.Payload) != "b-low" {
		t.Fatalf("response payload = %q, want b-low", string(resp.Payload))
	}

	exec.mu.Lock()
	refreshCalls := append([]string(nil), exec.refreshCalls...)
	probeCalls := append([]string(nil), exec.probeCalls...)
	executeCalls := append([]string(nil), exec.executeCalls...)
	exec.mu.Unlock()
	if got := fmt.Sprint(refreshCalls); got != "[a-high]" {
		t.Fatalf("refresh calls = %s, want [a-high]", got)
	}
	if got := fmt.Sprint(probeCalls); got != "[]" {
		t.Fatalf("probe calls = %s, want none", got)
	}
	if got := fmt.Sprint(executeCalls); got != "[b-low]" {
		t.Fatalf("execute calls = %s, want [b-low]", got)
	}
	high, ok := manager.GetByID("a-high")
	if !ok {
		t.Fatal("expected a-high auth")
	}
	if !high.Unavailable || !high.Quota.Exceeded {
		t.Fatalf("high auth state = unavailable:%v quota:%#v, want shared quota cooldown", high.Unavailable, high.Quota)
	}
}

func testFiveHourQuota(nextFreshAt time.Time, usedPercent float64) *QuotaInfo {
	return &QuotaInfo{
		FiveHour: QuotaWindow{
			UsedPercent:      usedPercent,
			UsedPercentKnown: true,
			NextFreshAt:      nextFreshAt,
			RefreshedAt:      nextFreshAt.Add(-time.Hour),
		},
	}
}

func testQuotaWindows(fiveHourNextFreshAt time.Time, fiveHourUsed float64, weeklyNextFreshAt time.Time, weeklyUsed float64) *QuotaInfo {
	return &QuotaInfo{
		FiveHour: QuotaWindow{
			UsedPercent:      fiveHourUsed,
			UsedPercentKnown: true,
			NextFreshAt:      fiveHourNextFreshAt,
			RefreshedAt:      fiveHourNextFreshAt.Add(-time.Hour),
		},
		Weekly: QuotaWindow{
			UsedPercent:      weeklyUsed,
			UsedPercentKnown: true,
			NextFreshAt:      weeklyNextFreshAt,
			RefreshedAt:      weeklyNextFreshAt.Add(-time.Hour),
		},
	}
}
