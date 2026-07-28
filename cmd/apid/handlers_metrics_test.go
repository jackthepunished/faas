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
