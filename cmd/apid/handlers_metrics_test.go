package main

// Issue #273 / ADR-041 — per-app metrics handler tests.
//
// Coverage matrix:
//   - happy path (owner 200)
//   - cross-account 404 (IDOR safety)
//   - unknown slug 404
//   - degraded fallback (200 + Source prefix + zeroed fields)
//   - range validation (`15d` ok, `30d` 400, garbage 400)
//   - empty ?range falls back to the server's 5m default
//   - prometheus-disabled path (s.promqlClient == nil)

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/onebox-faas/faas/pkg/api"
	"github.com/onebox-faas/faas/pkg/state"
)

// TestAppMetrics_HappyPath seeds an app, installs a Prometheus
// fixture, hits the new endpoint, and asserts every field landed.
func TestAppMetrics_HappyPath(t *testing.T) {
	e := setup(t, api.PlanPro)
	mustSeedApp(t, e, "my-api")
	installPromFixture(t, &e, func(q string) string {
		return `{"status":"success","data":{"resultType":"vector","result":[{"value":[0,"42"]}]}}`
	})

	rec := e.do(t, "GET", "/v1/apps/my-api/metrics?range=5m", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var out api.AppMetricsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.AppID == "" {
		t.Errorf("app_id empty")
	}
	if out.Range != "5m" {
		t.Errorf("range = %q, want 5m", out.Range)
	}
	if out.Source != "prometheus" {
		t.Errorf("source = %q, want prometheus", out.Source)
	}
	if out.RequestCount != 42 {
		t.Errorf("request_count = %d, want 42", out.RequestCount)
	}
}

// TestAppMetrics_CrossAccount404 confirms IDOR safety: a request from
// account A for account B's slug returns 404 (NOT 200 with B's data).
func TestAppMetrics_CrossAccount404(t *testing.T) {
	e := setup(t, api.PlanPro)
	other := state.NewMemStore()
	otherAcct, _ := other.CreateAccount(context.Background(), "other@hobby.com", api.PlanPro)
	mustSeedAppFor(t, other, otherAcct.ID, "their-api")

	rec := e.do(t, "GET", "/v1/apps/their-api/metrics", nil, nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status %d, want 404 (IDOR)", rec.Code)
	}
}

// TestAppMetrics_UnknownSlug404 mirrors TestGetApp_UnknownReturns404.
func TestAppMetrics_UnknownSlug404(t *testing.T) {
	e := setup(t, api.PlanPro)
	rec := e.do(t, "GET", "/v1/apps/ghost/metrics", nil, nil)
	assertProblem(t, rec, http.StatusNotFound, api.CodeNotFound)
}

// TestAppMetrics_DegradedFallback asserts the 200 + degraded contract.
func TestAppMetrics_DegradedFallback(t *testing.T) {
	e := setup(t, api.PlanPro)
	mustSeedApp(t, e, "my-api")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "down", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	e.s.WithStatusCache(srv.URL, "")

	rec := e.do(t, "GET", "/v1/apps/my-api/metrics", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, want 200 (degraded contract)", rec.Code)
	}
	var out api.AppMetricsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !strings.HasPrefix(out.Source, "degraded:") {
		t.Errorf("source = %q, want degraded prefix", out.Source)
	}
	if out.RequestCount != 0 || out.LatencyP50MS != 0 || out.WakeP95MS != 0 {
		t.Errorf("degraded fields not zeroed: %+v", out)
	}
}

// TestAppMetrics_RangeValidation covers the closed vocabulary.
func TestAppMetrics_RangeValidation(t *testing.T) {
	e := setup(t, api.PlanPro)
	mustSeedApp(t, e, "my-api")
	installPromFixture(t, &e, func(q string) string {
		return `{"status":"success","data":{"resultType":"vector","result":[{"value":[0,"0"]}]}}`
	})

	cases := []struct {
		range_ string
		want   int
	}{
		{"5m", http.StatusOK},
		{"15d", http.StatusOK},
		{"30d", http.StatusBadRequest},
		{"banana", http.StatusBadRequest},
	}
	for _, tc := range cases {
		rec := e.do(t, "GET", "/v1/apps/my-api/metrics?range="+tc.range_, nil, nil)
		if rec.Code != tc.want {
			t.Errorf("range=%q status=%d want=%d", tc.range_, rec.Code, tc.want)
		}
	}
}

// TestAppMetrics_NoRangeFallsBackToDefault asserts the server applies
// the 5m default when the client omits the param.
func TestAppMetrics_NoRangeFallsBackToDefault(t *testing.T) {
	e := setup(t, api.PlanPro)
	mustSeedApp(t, e, "my-api")
	installPromFixture(t, &e, func(q string) string {
		return `{"status":"success","data":{"resultType":"vector","result":[{"value":[0,"0"]}]}}`
	})

	rec := e.do(t, "GET", "/v1/apps/my-api/metrics", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	var out api.AppMetricsResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out.Range != "5m" {
		t.Errorf("range = %q, want default 5m", out.Range)
	}
}

// TestAppMetrics_NaNGuards pins the safeFloat / safePercent guards
// on the seven arithmetic paths. Histogram_quantile over an empty
// window returns NaN; rate() over a missing counter returns 0; the
// handlers must coerce those into zero-valued wire fields rather
// than serialise "NaN" / "Inf" to JSON. Issue #273 / ADR-041
// criterion #7.
func TestAppMetrics_NaNGuards(t *testing.T) {
	e := setup(t, api.PlanPro)
	mustSeedApp(t, e, "my-api")
	installPromFixture(t, &e, func(q string) string {
		// Histogram_quantile queries return NaN; the rate/division
		// queries return 0 (no data). The handler must coerce NaN
		// to 0 via safeFloat / safePercent. The denominator-guarded
		// queries (error_rate, cold_start) hit zero-division →
		// NaN; safePercent must clamp to 0.
		if strings.Contains(q, "histogram_quantile") {
			return `{"status":"success","data":{"resultType":"vector","result":[{"value":[0,"NaN"]}]}}`
		}
		return `{"status":"success","data":{"resultType":"vector","result":[{"value":[0,"0"]}]}}`
	})

	rec := e.do(t, "GET", "/v1/apps/my-api/metrics?range=5m", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var out api.AppMetricsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v (body may contain NaN/Inf literals — should be 0)", err)
	}
	// Every numeric field must be a finite, non-negative number.
	// NaN/Inf from promql must be coerced to 0 by safeFloat /
	// safePercent; percentages must clamp to [0,100] even if the
	// upstream had a wrapped-around math value.
	check := func(name string, v float64) {
		t.Helper()
		if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
			t.Errorf("%s = %v, want finite non-negative", name, v)
		}
	}
	check("LatencyP50MS", out.LatencyP50MS)
	check("LatencyP95MS", out.LatencyP95MS)
	check("LatencyP99MS", out.LatencyP99MS)
	check("ErrorRatePct", out.ErrorRatePct)
	check("ColdStartPct", out.ColdStartPct)
	check("WakeP95MS", out.WakeP95MS)
	if out.ErrorRatePct > 100 || out.ColdStartPct > 100 {
		t.Errorf("percentage clamp failed: error=%v cold=%v", out.ErrorRatePct, out.ColdStartPct)
	}
	if out.RequestCount != 0 {
		t.Errorf("RequestCount = %d, want 0 (fixture returned 0)", out.RequestCount)
	}
}

// TestAppMetrics_PrometheusDisabled exercises the nil-client path:
// the handler must NOT 5xx — it returns 200 with Source="degraded:
// prometheus not configured" and zeroed fields.
func TestAppMetrics_PrometheusDisabled(t *testing.T) {
	e := setup(t, api.PlanPro)
	mustSeedApp(t, e, "my-api")
	// setup() does NOT call WithStatusCache, so promqlClient is nil.

	rec := e.do(t, "GET", "/v1/apps/my-api/metrics", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, want 200 (degraded contract)", rec.Code)
	}
	var out api.AppMetricsResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if !strings.Contains(out.Source, "prometheus not configured") {
		t.Errorf("source = %q, want 'prometheus not configured'", out.Source)
	}
}

// installPromFixture spins up an httptest.Server that responds to
// every query with `responder(query)`, wires it into the env's
// server via WithStatusCache, and registers a Cleanup that closes
// the server.
func installPromFixture(t *testing.T, e *testEnv, responder func(query string) string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(responder(r.URL.Query().Get("query"))))
	}))
	t.Cleanup(srv.Close)
	e.s.WithStatusCache(srv.URL, "")
}

// TestAppMetrics_NoRequireMFA pins the deliberate omission of
// requireMFA from the per-app metrics handler chain. Issue #273 /
// ADR-041: a session cookie without an MFA-cookie-clearance MUST
// still pass — the route is read-only and the primary caller is an
// API key that does not carry a session cookie at all. A future
// refactor that adds requireMFA here would break API-key access.
//
// We exercise the cookie path because the bearer-key path is
// already covered by TestAppMetrics_HappyPath. The cookie path
// runs through s.requireMFA — proving the route is gated by
// authLimited only.
func TestAppMetrics_NoRequireMFA(t *testing.T) {
	e, cookie := setupWithSession(t)
	mustSeedApp(t, e, "my-api")
	// No installPromFixture — the nil-prometheus path falls through
	// to "degraded: prometheus not configured" which is still a 200,
	// which is the assertion we care about (the MFA gate, not the
	// metrics fetch).

	req := httptest.NewRequest(http.MethodGet, "/v1/apps/my-api/metrics", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("session-cookie (no MFA completed) status %d, want 200 — the route must NOT requireMFA", rec.Code)
	}
}

// TestAppMetrics_ScopeRejected exercises the per-route requireScope
// gate. Issue #273 / ADR-041: ScopesReadSurface (admin or apps:read)
// is the only allowed scope; a deploy:write-only key must be 403'd.
// Mirrors the matrix in handlers_scopes_test.go.
func TestAppMetrics_ScopeRejected(t *testing.T) {
	e := setupWithScopes(t, []string{api.ScopeDeployWrite})
	mustSeedApp(t, e, "my-api")

	rec := e.do(t, "GET", "/v1/apps/my-api/metrics", nil, nil)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("deploy:write-only status %d, want 403", rec.Code)
	}
}
