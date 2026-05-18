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
	refreshQueue []quotaRefreshResult
	probeQueue   []quotaProbeResult
}

type quotaRefreshResult struct {
	quota     *QuotaInfo
	refreshed bool
	err       error
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

func (e *quotaRefreshTestExecutor) Execute(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
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

func (e *quotaRefreshTestExecutor) RefreshQuota(_ context.Context, auth *Auth) (*QuotaInfo, bool, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.refreshCalls = append(e.refreshCalls, auth.ID)
	if len(e.refreshQueue) == 0 {
		return nil, false, nil
	}
	result := e.refreshQueue[0]
	e.refreshQueue = e.refreshQueue[1:]
	return result.quota, result.refreshed, result.err
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

func TestQuotaRefreshLoopFiltersAndStoresProbeQuota(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	exec := &quotaRefreshTestExecutor{
		refreshQueue: []quotaRefreshResult{{
			quota:     testQuota(now.Add(time.Hour), 50),
			refreshed: true,
		}},
		probeQueue: []quotaProbeResult{{
			quota: testQuota(now.Add(2*time.Hour), 90),
		}},
	}
	manager := NewManager(nil, &StickyRoundRobinSelector{}, nil)
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
	if runtimeQuota == nil || runtimeQuota.Weekly.UsagePercent != 90 {
		t.Fatalf("stored runtime quota = %#v, want probe quota with 90 percent", runtimeQuota)
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
	manager := NewManager(nil, &StickyRoundRobinSelector{}, nil)
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
	if runtimeQuota == nil || runtimeQuota.Weekly.UsagePercent != oldQuota.Weekly.UsagePercent {
		t.Fatalf("runtime quota = %#v, want previous quota preserved", runtimeQuota)
	}
}
