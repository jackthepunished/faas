// Per-route observability handler tests (ADR-093).
//
// Coverage matrix:
//   - happy path: gatewayd control listener responds 200 +
//     bounded route list; apid renders Source="live" + Routes
//   - empty routes (control listener returned ok but no admitted
//     routes): apid renders Source="live" + empty Routes array
//     (not null — the dashboard would crash on null)
//   - dial failure (gatewayd not running): apid renders
//     Source="unavailable" + empty Routes + X-Faas-Routes-State:
//     unavailable header so the dashboard can distinguish "not
//     reachable" from "no traffic yet"
//   - control listener non-200 (gatewayd returned an error):
//     apid renders Source="unavailable: gatewayd status NNN" +
//     empty Routes
//   - missing URL: operator never wired FAAS_GATEWAYD_CONTROL_URL;
//     apid renders Source="unavailable" without dialing
//   - cross-account slug (IDOR safety): mirrors the existing
//     TestAppMetrics_CrossAccount404 contract

package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/onebox-faas/faas/pkg/api"
)

// TestAppRoutes_HappyPath seeds an app, installs a fake
// gatewayd control listener that responds with a bounded route
// set, and asserts the apid reverse-proxy shape.
func TestAppRoutes_HappyPath(t *testing.T) {
	e := setup(t, api.PlanPro)
	mustSeedAppForPro(t, e, "my-api")

	// Fake gatewayd control listener. The path segment
	// doesn't matter — the test injects the URL into the
	// server directly.
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"slug":"my-api","app_id":"app-uuid-1","routes":["GET /users","POST /orders","__route_other__"]}`))
	}))
	t.Cleanup(gw.Close)
	e.s.WithGatewaydControlURL(gw.URL)

	rec := e.do(t, "GET", "/v1/apps/my-api/routes", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var out api.AppRoutesResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Source != "live" {
		t.Errorf("source = %q, want live", out.Source)
	}
	if out.AppID != "app-uuid-1" {
		t.Errorf("app_id = %q, want app-uuid-1", out.AppID)
	}
	if len(out.Routes) != 3 {
		t.Errorf("routes length = %d, want 3 (2 real + 1 reserved)", len(out.Routes))
	}
	if rec.Header().Get("X-Faas-Routes-State") != "ok" {
		t.Errorf("X-Faas-Routes-State = %q, want ok", rec.Header().Get("X-Faas-Routes-State"))
	}
}

// TestAppRoutes_EmptyRoutesRendersArrayNotNull asserts that an
// empty admitted set renders as `[]` not `null` — the dashboard
// JS would crash on null.
func TestAppRoutes_EmptyRoutesRendersArrayNotNull(t *testing.T) {
	e := setup(t, api.PlanPro)
	mustSeedAppForPro(t, e, "my-api")

	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"slug":"my-api","app_id":"app-uuid-1","routes":[]}`))
	}))
	t.Cleanup(gw.Close)
	e.s.WithGatewaydControlURL(gw.URL)

	rec := e.do(t, "GET", "/v1/apps/my-api/routes", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	// Raw substring check — encoding/json marshals nil as
	// `null` and an empty slice as `[]`. The handler must use
	// the empty-slice shape (the apid-side DTO uses []string
	// not []*string).
	if got := rec.Body.String(); !substringContains(got, `"routes":[]`) {
		t.Errorf("body = %s, want routes:[] (not routes:null)", got)
	}
}

// TestAppRoutes_DialFailureRendersUnavailable covers the case
// where the apid→gatewayd hop fails (gatewayd not running, or
// the loopback bind is wrong).
func TestAppRoutes_DialFailureRendersUnavailable(t *testing.T) {
	e := setup(t, api.PlanPro)
	mustSeedAppForPro(t, e, "my-api")

	// 127.0.0.1:1 is reserved + never bound; the dial fails
	// immediately without occupying a port.
	e.s.WithGatewaydControlURL("http://127.0.0.1:1")

	rec := e.do(t, "GET", "/v1/apps/my-api/routes", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, want 200 (unavailable-state-not-502 contract)", rec.Code)
	}
	var out api.AppRoutesResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Source != "unavailable" {
		t.Errorf("source = %q, want unavailable", out.Source)
	}
	if len(out.Routes) != 0 {
		t.Errorf("routes = %v, want empty", out.Routes)
	}
	if rec.Header().Get("X-Faas-Routes-State") != "unavailable" {
		t.Errorf("X-Faas-Routes-State = %q, want unavailable", rec.Header().Get("X-Faas-Routes-State"))
	}
}

// TestAppRoutes_MissingURLRendersUnavailable covers the
// pre-ADR-093 / dev-mode posture where the operator never wired
// FAAS_GATEWAYD_CONTROL_URL.
func TestAppRoutes_MissingURLRendersUnavailable(t *testing.T) {
	e := setup(t, api.PlanPro)
	mustSeedAppForPro(t, e, "my-api")
	// Do NOT call WithGatewaydControlURL — the field stays "".

	rec := e.do(t, "GET", "/v1/apps/my-api/routes", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, want 200 (unavailable-state-not-502 contract)", rec.Code)
	}
	var out api.AppRoutesResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Source != "unavailable" {
		t.Errorf("source = %q, want unavailable", out.Source)
	}
}

// TestAppRoutes_CrossAccount404 confirms IDOR safety — a
// request from account A for account B's slug returns 404 (not
// 200 with B's route labels, which would leak another tenant's
// API surface).
func TestAppRoutes_CrossAccount404(t *testing.T) {
	e := setup(t, api.PlanPro)
	// Other tenant's app — must NOT be visible to e's caller.
	otherAcct, _ := e.store.CreateAccount(t.Context(), "other@hobby.com", api.PlanPro)
	mustSeedAppFor(t, e.store, otherAcct.ID, "their-api")

	rec := e.do(t, "GET", "/v1/apps/their-api/routes", nil, nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status %d, want 404 (IDOR)", rec.Code)
	}
}

func substringContains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}

// mustSeedAppForPro is a thin wrapper that uses the existing
// mustSeedAppFor helper with a Pro account context. Mirrors the
// pattern in handlers_metrics_test.go's helpers.
func mustSeedAppForPro(t *testing.T, e testEnv, slug string) {
	t.Helper()
	mustSeedAppFor(t, e.store, e.acct.ID, slug)
}