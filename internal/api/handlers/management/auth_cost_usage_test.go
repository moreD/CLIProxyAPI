package management

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/authusage"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/billing"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestAuthCostUsageAPI(t *testing.T) {
	store, err := authusage.NewStore(filepath.Join(t.TempDir(), "auth-usage.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	t.Cleanup(authusage.SetDefault(store))
	manager := coreauth.NewManager(nil, nil, nil)
	reset := time.Now().Add(51 * time.Hour).Truncate(time.Second)
	registerAuthForLookupTest(t, manager, &coreauth.Auth{ID: "first", Index: "first-index", Provider: "codex", RuntimeQuota: &coreauth.QuotaInfo{Weekly: coreauth.QuotaWindow{NextFreshAt: reset, RefreshedAt: time.Now(), LimitWindowSeconds: 604800}}})
	registerAuthForLookupTest(t, manager, &coreauth.Auth{ID: "second", Index: "second-index", Provider: "codex"})
	h := &Handler{authManager: manager}
	router := gin.New()
	router.GET("/auth-files/cost-usage", h.GetAuthCostUsage)
	router.PATCH("/auth-files/cost-usage", h.PatchAuthCostLimit)
	request := func(method, path, body string, want int) authusage.State {
		t.Helper()
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		router.ServeHTTP(rec, req)
		if rec.Code != want {
			t.Fatalf("%s %s: status %d want %d: %s", method, path, rec.Code, want, rec.Body.String())
		}
		var state authusage.State
		if want == http.StatusOK {
			if err := json.Unmarshal(rec.Body.Bytes(), &state); err != nil {
				t.Fatal(err)
			}
		}
		return state
	}
	path := "/auth-files/cost-usage"
	state := request("PATCH", path, `{"auth_index":"first-index","limit_usd":1.23456789}`, 200)
	if state.LimitUSD != billing.USD(1234567890) || !state.Window.End.Equal(reset) {
		t.Fatalf("unexpected limit/window: %+v", state)
	}
	if err := store.Record("first-index", billing.USD(2*billing.Scale), time.Now()); err != nil {
		t.Fatal(err)
	}
	state = request("GET", path+"?auth_index=first-index", "", 200)
	if !state.Exceeded || state.CostUSD != billing.USD(2*billing.Scale) {
		t.Fatalf("usage missing: %+v", state)
	}
	state = request("GET", path+"?auth_index=second-index", "", 200)
	if state.CostUSD != 0 || state.LimitUSD != 0 || state.Exceeded {
		t.Fatalf("cost leaked across auths: %+v", state)
	}
	state = request("PATCH", path, `{"auth_index":"first-index","limit_usd":0}`, 200)
	if state.Exceeded || state.CostUSD != billing.USD(2*billing.Scale) {
		t.Fatalf("removing cap reset usage or kept auth blocked: %+v", state)
	}
	for _, body := range []string{
		`{"auth_index":"first-index"}`, `{"auth_index":"first-index","limit_usd":null}`,
		`{"auth_index":"first-index","limit_usd":-1}`, `{"auth_index":"first-index","limit_usd":0.0000000001}`,
		`{"auth_index":"first-index","limit_usd":"1"}`, `{"auth_index":"","limit_usd":1}`,
	} {
		request("PATCH", path, body, 400)
	}
	request("PATCH", path, `{"auth_index":"missing","limit_usd":1}`, 404)
	request("GET", path, "", 400)
	request("GET", path+"?auth_index=missing", "", 404)
}
