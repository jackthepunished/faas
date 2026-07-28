package main

// Per-app metrics handlers (issue #273 / ADR-041).
//
// GET /v1/apps/{slug}/metrics?range=...
//
// Read-only, scoped to api.ScopesReadSurface (admin or apps:read).
// No MFA required — the primary caller is an API key. IDOR-safe via
// the existing loadApp (cross-account slug → 404, not 200 with
// another tenant's data).
//
// Range vocabulary is closed and bounded by Prometheus retention
// (`prom_retention_days: 15` in deploy/ansible/roles/prometheus/
// defaults/main.yml):
//
//	5m (default), 15m, 1h, 6h, 24h, 7d, 15d
//
// Anything else → 400 CodeValidation listing the closed set.
//
// Prometheus unreachable (s.promqlClient == nil, or a query failed)
// → HTTP 200 with zeroed fields and Source="degraded: <reason>",
// matching the public /status/slo.json contract so the dashboard
// has one empty-state path.
//
// Arithmetic safety (criterion #7): rate()/increase() are already
// counter-reset tolerant, but their numerators/denominators can
// still be NaN (histogram_quantile on an empty window), Inf, or
// zero. The safe helpers below run in this order so each guard
// short-circuits the rest: zero-denominator → 0; NaN/Inf → 0;
// clamp percentages to [0,100] and latencies to ≥0.

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/onebox-faas/faas/pkg/api"
	"github.com/onebox-faas/faas/pkg/state"
)

// metricsRanges is the closed vocabulary for the ?range= query param.
// Bounded by Prometheus retention (15d).
var metricsRanges = []string{"5m", "15m", "1h", "6h", "24h", "7d", "15d"}

// defaultMetricsRange is what the server applies when the client
// passes no range. Matches the §12 status-page window so customers
// don't see two different "current" periods on the same dashboard.
const defaultMetricsRange = "5m"

// sourcePrometheus is the canonical "Source" value emitted on a
// healthy Prometheus response. The degraded counterpart takes the
// shape "degraded: <reason>" — see fetchAppMetrics. Centralised
// here so a single rename (and goconst lint) keeps the three
// production emitters (handlers_metrics, handlers_dashboard,
// status) and the dashboard template in lockstep.
const sourcePrometheus = "prometheus"

// getAppMetrics serves GET /v1/apps/{slug}/metrics?range=.
// Mirrors getApp's auth chain (without requireMFA — read-only,
// primary caller is an API key with ScopesReadSurface).
func (s *server) getAppMetrics(w http.ResponseWriter, r *http.Request, acct state.Account) {
	app, ok := s.loadApp(w, r, acct, r.PathValue("slug")) //nolint:contextcheck // loadApp takes r and uses r.Context() for its own DB calls; the helper is shared across every per-app handler.
	if !ok {
		// loadApp already wrote the 404.
		return
	}

	rng := r.URL.Query().Get("range")
	if rng == "" {
		rng = defaultMetricsRange
	}
	if !isValidMetricsRange(rng) {
		api.WriteProblem(w, api.NewProblem(http.StatusBadRequest, api.CodeValidation,
			"invalid range",
			fmt.Sprintf("range must be one of: %s", strings.Join(metricsRanges, ", "))))
		return
	}

	resp, src := s.fetchAppMetrics(r.Context(), app.ID, rng)
	resp.AppID = app.ID
	resp.Range = rng
	resp.Source = src
	resp.AsOf = time.Now().UTC().Format(time.RFC3339Nano)
	writeJSON(w, http.StatusOK, resp)
}

// isValidMetricsRange returns true for closed-set membership.
func isValidMetricsRange(rng string) bool {
	for _, r := range metricsRanges {
		if r == rng {
			return true
		}
	}
	return false
}

// fetchAppMetrics runs the per-app PromQL queries and assembles an
// AppMetricsResponse. Returns the response and a Source string
// ("prometheus" on success, "degraded: <reason>" on failure). Safe
// when s.promqlClient is nil — every field is zeroed.
//
// The percentile window (5m by default) matches the public
// /status/slo.json. The error-rate and cold-start windows match
// the same window. The wake p95 is FLEET (gateway_wake_latency_seconds
// is unlabeled) and labelled as such in the UI.
func (s *server) fetchAppMetrics(ctx context.Context, appID, rng string) (api.AppMetricsResponse, string) {
	resp := api.AppMetricsResponse{}
	if s.promqlClient == nil {
		return resp, "degraded: prometheus not configured"
	}

	// 1. Request count.
	countQ := fmt.Sprintf(`sum(increase(gateway_requests_total{app=%q}[%s]))`, appID, rng)
	if v, err := s.promqlClient.QueryScalar(ctx, countQ); err == nil {
		resp.RequestCount = int64(safeRoundNonNeg(v))
	} else {
		return s.degradedFromErr(resp, err, "request_count")
	}

	// 2-4. P50 / P95 / P99 over 2xx class only. histogram_quantile
	// returns NaN on an empty window — safeFloat coerces to 0.
	for _, p := range []struct {
		q     float64
		dest  *float64
		label string
	}{
		{q: 0.50, dest: &resp.LatencyP50MS, label: "p50"},
		{q: 0.95, dest: &resp.LatencyP95MS, label: "p95"},
		{q: 0.99, dest: &resp.LatencyP99MS, label: "p99"},
	} {
		q := fmt.Sprintf(
			`histogram_quantile(%g, sum by (le) (rate(gateway_request_duration_seconds_bucket{app=%q,class="2xx"}[%s]))) * 1000`,
			p.q, appID, rng)
		v, err := s.promqlClient.QueryScalar(ctx, q)
		if err != nil {
			return s.degradedFromErr(resp, err, p.label)
		}
		*p.dest = safeFloat(v)
	}

	// 5. Error rate %.
	errQ := fmt.Sprintf(
		`sum(rate(gateway_requests_total{app=%q,code=~"[45].."}[%s])) / sum(rate(gateway_requests_total{app=%q}[%s])) * 100`,
		appID, rng, appID, rng)
	if v, err := s.promqlClient.QueryScalar(ctx, errQ); err == nil {
		resp.ErrorRatePct = safePercent(v)
	} else {
		return s.degradedFromErr(resp, err, "error_rate")
	}

	// 6. Cold start %.
	coldQ := fmt.Sprintf(
		`sum(rate(gateway_cold_boot_total{app=%q}[%s])) / sum(rate(gateway_requests_total{app=%q}[%s])) * 100`,
		appID, rng, appID, rng)
	if v, err := s.promqlClient.QueryScalar(ctx, coldQ); err == nil {
		resp.ColdStartPct = safePercent(v)
	} else {
		return s.degradedFromErr(resp, err, "cold_start")
	}

	// 7. Fleet wake p95 (the unlabeled gateway_wake_latency_seconds).
	wakeQ := fmt.Sprintf(
		`histogram_quantile(0.95, sum by (le) (rate(gateway_wake_latency_seconds_bucket[%s]))) * 1000`, rng)
	if v, err := s.promqlClient.QueryScalar(ctx, wakeQ); err == nil {
		resp.WakeP95MS = safeFloat(v)
	} else {
		return s.degradedFromErr(resp, err, "wake_p95")
	}

	return resp, sourcePrometheus
}

// degradedFromErr returns the (already populated) response with a
// "degraded: <err>" Source. Logs the failure so operators can tell
// which query failed (the dashboard shows the generic message; the
// server log has the detail).
func (s *server) degradedFromErr(resp api.AppMetricsResponse, err error, label string) (api.AppMetricsResponse, string) {
	if s.log != nil {
		s.log.Warn("apid: app-metrics query failed", "label", label, "err", err)
	}
	// Fall back to zeroed fields rather than partially-populated
	// numbers — the dashboard's empty-state message depends on
	// RequestCount being 0 when degraded.
	return api.AppMetricsResponse{}, "degraded: " + err.Error()
}

// safeFloat coerces NaN/Inf to 0 and clamps negative values to 0.
// histogram_quantile on an empty window returns NaN; a custom
// histogram with negative bucket observations (impossible here but
// defensive) would return a negative result.
func safeFloat(v float64) float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
		return 0
	}
	return v
}

// safePercent is safeFloat plus a [0,100] clamp. A division-by-zero
// fallback yields 0 from promql; NaN from histogram_quantile yields 0
// from safeFloat; over-100 from arithmetic wrap-around is clamped
// here.
func safePercent(v float64) float64 {
	x := safeFloat(v)
	if x > 100 {
		x = 100
	}
	return x
}

// safeRoundNonNeg is safeFloat under a name that documents intent:
// used for `request_count` which is integral on the wire but comes
// back as a float from promql (increase() returns a float). The
// call site does the int-conversion (int64(...)) so the rounding
// policy (currently a float-truncating cast) lives at the call
// site; keeping this as its own helper means a future change to
// banker's rounding has one site to touch. Issue #273 / ADR-041.
func safeRoundNonNeg(v float64) float64 {
	return safeFloat(v)
}
