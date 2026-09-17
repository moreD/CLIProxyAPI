package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/billing"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/redisqueue"
)

func TestClientUsageRouteRequiresKeyAndReturnsOnlyItsLimits(t *testing.T) {
	previousEnabled := redisqueue.UsageStatisticsEnabled()
	server := newTestServer(t)
	redisqueue.SetUsageStatisticsEnabled(true)
	server.cfg.APIKeys = []config.APIKeyEntry{
		{APIKey: "client-a", CostLimits: config.APIKeyCostLimits{TwelveHour: billing.USD(billing.Scale)}},
		{APIKey: "client-b", CostLimits: config.APIKeyCostLimits{SevenDay: billing.USD(9 * billing.Scale)}},
	}
	server.applyAccessConfig(nil, server.cfg)
	redisqueue.SetClientCostLimits(server.cfg.APIKeys)
	t.Cleanup(func() {
		redisqueue.SetClientCostLimits(nil)
		redisqueue.SetUsageStatisticsEnabled(previousEnabled)
	})
	for _, tc := range []struct {
		name, authorization, query string
		status                     int
	}{
		{"missing", "", "", 401},
		{"invalid", "Bearer wrong", "", 401},
		{"wrong scheme", "Basic client-a", "", 401},
		{"query only", "", "?key=client-a", 401},
		{"own key", "Bearer client-a", "?key=client-b", 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/api/client-usage"+tc.query, nil)
			req.Header.Set("Authorization", tc.authorization)
			rr := httptest.NewRecorder()
			server.engine.ServeHTTP(rr, req)
			if rr.Code != tc.status || rr.Header().Get("Cache-Control") != "no-store" {
				t.Fatalf("status=%d, headers=%v", rr.Code, rr.Header())
			}
			if tc.status == 200 {
				var snapshot redisqueue.ClientUsageSnapshot
				if err := json.Unmarshal(rr.Body.Bytes(), &snapshot); err != nil {
					t.Fatal(err)
				}
				if snapshot.Usage.Limits.TwelveHour != billing.USD(billing.Scale) || snapshot.Usage.Limits.SevenDay != 0 {
					t.Fatalf("wrong client's limits: %+v", snapshot.Usage.Limits)
				}
				if strings.Contains(rr.Body.String(), "client-") || strings.Contains(rr.Body.String(), "api_key") {
					t.Fatal("response exposed API keys")
				}
			}
		})
	}
	server.accessManager.SetProviders(nil)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/client-usage", nil)
	req.Header.Set("Authorization", "Bearer client-a")
	server.engine.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatal("an unconfigured authenticator must not grant access")
	}
	server.applyAccessConfig(nil, server.cfg)
	redisqueue.SetClientCostLimits(nil)
	redisqueue.SetUsageStatisticsEnabled(false)
	rr = httptest.NewRecorder()
	server.engine.ServeHTTP(rr, req)
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatal("disabled statistics must not be presented as zero usage")
	}
}

func TestClientUsagePageServesOnlyInstalledAsset(t *testing.T) {
	staticDir := t.TempDir()
	t.Setenv("MANAGEMENT_STATIC_PATH", staticDir)
	server := newTestServer(t)
	rr := httptest.NewRecorder()
	server.engine.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/usage.html", nil))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("missing page status = %d", rr.Code)
	}
	if err := os.WriteFile(filepath.Join(staticDir, "usage.html"), []byte("<!doctype html><title>Usage</title>"), 0o600); err != nil {
		t.Fatal(err)
	}
	rr = httptest.NewRecorder()
	server.engine.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/usage.html", nil))
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "<title>Usage</title>") || rr.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("installed page status = %d", rr.Code)
	}
}
