// Package appmetrics extracts the per-app Prometheus fetch used by
// cmd/apid/handlers_metrics.go (issue #273 / ADR-042) and the
// dashboard's renderAppDetail so meterd (issue #396 / ADR-045) can
// call the same implementation from cmd/meterd/... in PR 4. Zero
// behaviour change for the apid caller; the public surface is the
// function signatures and the closed-set range vocabulary.
//
// The seven PromQL builders + percentile loop + degraded-source
// helpers were lifted verbatim from cmd/apid/handlers_metrics.go.
// The CodeQL go/log-injection sanitiser pattern (two-call
// strings.ReplaceAll for CR then LF, inline at the log call site)
// is preserved per the precedent at handlers_metrics.go:189-193
// (alert #117) — the dataflow path stays unambiguous to CodeQL.
package appmetrics

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"reflect"
	"strings"

	"github.com/onebox-faas/faas/pkg/api"
)

// SourcePrometheus is the canonical "Source" value emitted on a
// healthy Prometheus response. The three production emitters
// (handlers_metrics, handlers_dashboard, status) plus the future
// meterd caller all compare against this constant — goconst catches
// drift if a future rename happens in only one place.
const SourcePrometheus = "prometheus"

// SourceDegradedPrefix is the prefix every degraded response carries.
// The dashboard and the public /status/slo.json both render the
// "degraded:" branch off this prefix.
const SourceDegradedPrefix = "degraded: "

// DefaultRange is the range the server applies when the caller passes
// no range. Matches the §12 status-page window so customers don't see
// two different "current" periods on the same dashboard.
const DefaultRange = "5m"

// metricsRanges is the closed vocabulary for the range argument.
// Bounded by Prometheus retention (`prom_retention_days: 15` in
// deploy/ansible/roles/prometheus/defaults/main.yml). 5m is the
// default the server applies when the client omits the param.
var metricsRanges = []string{"5m", "15m", "1h", "6h", "24h", "7d", "15d"}

// PromQL is the minimal interface Fetch needs. pkg/promql.Client
// satisfies it; tests pass a stub. Mirrors the testable surface
// pkg/promql exposes (HTTPDoer) so the seam is one method, not two.
type PromQL interface {
	QueryScalar(ctx context.Context, query string) (float64, error)
}

// Fetch runs the per-app PromQL queries and assembles an
// AppMetricsResponse. Returns the response and a Source string
// ("prometheus" on success, "degraded: <reason>" on failure). Safe
// when fetcher is nil — every field is zeroed and the source is
// "degraded: prometheus not configured".
//
// log is the destination for per-query failure warnings. nil falls
// back to slog.Default() so callers that wire a no-log setup don't
// have to special-case the zero value (mirrors scheddgrpc/server.go
// nil coercion).
//
// The percentile window (5m by default) matches the public
// /status/slo.json. The error-rate and cold-start windows match the
// same window. The wake p95 is FLEET (gateway_wake_latency_seconds
// is unlabeled) and labelled as such in the UI.
func Fetch(ctx context.Context, fetcher PromQL, log *slog.Logger, appID, rng string) (api.AppMetricsResponse, string) {
	if log == nil {
		log = slog.Default()
	}
	resp := api.AppMetricsResponse{}
	// Guard against both the bare nil interface AND the typed-nil
	// gotcha: *promql.Client(nil) satisfies PromQL with a non-nil
	// interface value (type info without data). The handler does the
	// concrete-type check too, but defending in the package keeps the
	// "zero behaviour change" promise intact for future meterd callers
	// that might also pass a nil *promql.Client.
	if fetcher == nil || isTypedNil(fetcher) {
		return resp, SourceDegradedPrefix + "prometheus not configured"
	}

	// 1. Request count.
	countQ := fmt.Sprintf(`sum(increase(gateway_requests_total{app=%q}[%s]))`, appID, rng)
	if v, err := fetcher.QueryScalar(ctx, countQ); err == nil {
		resp.RequestCount = int64(SafeRoundNonNeg(v))
	} else {
		return degradedFromErr(resp, err, log, "request_count")
	}

	// 2-4. P50 / P95 / P99 over 2xx class only. histogram_quantile
	// returns NaN on an empty window — SafeFloat coerces to 0.
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
		v, err := fetcher.QueryScalar(ctx, q)
		if err != nil {
			return degradedFromErr(resp, err, log, p.label)
		}
		*p.dest = SafeFloat(v)
	}

	// 5. Error rate %.
	errQ := fmt.Sprintf(
		`sum(rate(gateway_requests_total{app=%q,code=~"[45].."}[%s])) / sum(rate(gateway_requests_total{app=%q}[%s])) * 100`,
		appID, rng, appID, rng)
	if v, err := fetcher.QueryScalar(ctx, errQ); err == nil {
		resp.ErrorRatePct = SafePercent(v)
	} else {
		return degradedFromErr(resp, err, log, "error_rate")
	}

	// 6. Cold start %.
	coldQ := fmt.Sprintf(
		`sum(rate(gateway_cold_boot_total{app=%q}[%s])) / sum(rate(gateway_requests_total{app=%q}[%s])) * 100`,
		appID, rng, appID, rng)
	if v, err := fetcher.QueryScalar(ctx, coldQ); err == nil {
		resp.ColdStartPct = SafePercent(v)
	} else {
		return degradedFromErr(resp, err, log, "cold_start")
	}

	// 7. Fleet wake p95 (the unlabeled gateway_wake_latency_seconds).
	wakeQ := fmt.Sprintf(
		`histogram_quantile(0.95, sum by (le) (rate(gateway_wake_latency_seconds_bucket[%s]))) * 1000`, rng)
	if v, err := fetcher.QueryScalar(ctx, wakeQ); err == nil {
		resp.WakeP95MS = SafeFloat(v)
	} else {
		return degradedFromErr(resp, err, log, "wake_p95")
	}

	return resp, SourcePrometheus
}

// degradedFromErr returns the zeroed response with a
// "degraded: <err>" Source. Logs the failure so operators can tell
// which query failed (the dashboard shows the generic message; the
// server log has the detail).
//
// CodeQL go/log-injection (alert #117): the err string is
// user-controllable (the PromQL `range=` query param flows into the
// query body that produced the error). CodeQL's sanitiser model
// only recognises the two-call pattern below — see
// memory/codeql-go-log-injection-sanitisers.md for the full
// precedent. The CR/LF strip is inline at the call site (NOT inside
// a helper) so the dataflow path is unambiguous to CodeQL.
func degradedFromErr(resp api.AppMetricsResponse, err error, log *slog.Logger, label string) (api.AppMetricsResponse, string) {
	if log != nil {
		msg := strings.ReplaceAll(err.Error(), "\r", "")
		msg = strings.ReplaceAll(msg, "\n", "")
		log.Warn("appmetrics: query failed", "label", label, "err", msg)
	}
	// Fall back to zeroed fields rather than partially-populated
	// numbers — the dashboard's empty-state message depends on
	// RequestCount being 0 when degraded.
	return api.AppMetricsResponse{}, SourceDegradedPrefix + err.Error()
}

// Ranges returns a copy of the closed-set vocabulary for the range
// argument. A copy is returned so callers cannot mutate the package
// state (mirrors the pkg/oci counter-labels accessor pattern).
func Ranges() []string {
	out := make([]string, len(metricsRanges))
	copy(out, metricsRanges)
	return out
}

// IsValidRange returns true iff rng is in the closed set returned
// by Ranges(). The HTTP handler validates ?range= via this helper
// so the dashboard and any future caller share the same vocabulary.
func IsValidRange(rng string) bool {
	for _, r := range metricsRanges {
		if r == rng {
			return true
		}
	}
	return false
}

// SafeFloat coerces NaN/Inf to 0 and clamps negative values to 0.
// histogram_quantile on an empty window returns NaN; a custom
// histogram with negative bucket observations (impossible here but
// defensive) would return a negative result.
func SafeFloat(v float64) float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
		return 0
	}
	return v
}

// SafePercent is SafeFloat plus a [0,100] clamp. A division-by-zero
// fallback yields 0 from promql; NaN from histogram_quantile yields 0
// from SafeFloat; over-100 from arithmetic wrap-around is clamped here.
func SafePercent(v float64) float64 {
	x := SafeFloat(v)
	if x > 100 {
		x = 100
	}
	return x
}

// SafeRoundNonNeg is SafeFloat under a name that documents intent:
// used for `request_count` which is integral on the wire but comes
// back as a float from promql (increase() returns a float). The
// call site does the int-conversion (int64(...)) so the rounding
// policy (currently a float-truncating cast) lives at the call
// site; keeping this as its own helper means a future change to
// banker's rounding has one site to touch. Issue #273 / ADR-042.
func SafeRoundNonNeg(v float64) float64 {
	return SafeFloat(v)
}

// isTypedNil reports whether v is a non-nil interface value that
// wraps a typed nil pointer. The Go FAQ entry "Why is my nil error
// value not equal to nil?" documents this gotcha. A future refactor
// to accept the concrete *promql.Client here would obviate this
// helper; the package keeps the interface to make the seam
// stub-friendly.
func isTypedNil(v PromQL) bool {
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Ptr, reflect.Map, reflect.Chan, reflect.Func, reflect.Interface, reflect.Slice:
		return rv.IsNil()
	}
	return false
}
