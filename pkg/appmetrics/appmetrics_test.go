package appmetrics_test

// Issue #396 / ADR-045 PR 2 — pkg/appmetrics tests.
//
// Coverage matrix (mirrors the TestAppMetrics_* naming convention
// from cmd/apid/handlers_metrics_test.go so future readers find
// corresponding tests by name):
//   - happy path: all 7 PromQL queries land in the response
//   - degraded fallback: one query errors → zeroed fields + "degraded:" Source
//   - NaN guard: histogram_quantile over an empty window returns NaN → coerced to 0
//   - nil-client path: Fetch with nil PromQL → "degraded: prometheus not configured"
//   - query failure: per-query error → degraded source
//   - closed-set vocabulary: Ranges()/IsValidRange()
//   - SafeFloat / SafePercent / SafeRoundNonNeg boundary cases
//   - log-injection guard: err string with \r\n → captured slog JSON has neither

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"strings"
	"testing"

	"github.com/onebox-faas/faas/pkg/api"
	"github.com/onebox-faas/faas/pkg/appmetrics"
	"github.com/onebox-faas/faas/pkg/promql"
)

// stubPromQL is the test double for appmetrics.PromQL. The fn
// callback returns (value, error) for each QueryScalar call; tests
// set the per-scenario behaviour.
type stubPromQL struct {
	fn func(query string) (float64, error)
}

func (s *stubPromQL) QueryScalar(_ context.Context, query string) (float64, error) {
	return s.fn(query)
}

// captureLog returns a logger that writes JSON to the returned buffer.
// Tests assert on the buffer.
func captureLog(t *testing.T) (*slog.Logger, *bytes.Buffer) {
	t.Helper()
	buf := &bytes.Buffer{}
	return slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})), buf
}

// okStub returns a fixed scalar for every PromQL query — happy path.
func okStub(v float64) *stubPromQL {
	return &stubPromQL{fn: func(_ string) (float64, error) { return v, nil }}
}

// TestAppMetrics_Fetch_HappyPath stubs every query to return 42 and
// asserts every field landed in the response and Source == "prometheus".
func TestAppMetrics_Fetch_HappyPath(t *testing.T) {
	log, _ := captureLog(t)
	stub := okStub(42)

	resp, src := appmetrics.Fetch(context.Background(), stub, log, "app-1", "5m")
	if src != appmetrics.SourcePrometheus {
		t.Errorf("src = %q, want %q", src, appmetrics.SourcePrometheus)
	}
	if resp.RequestCount != 42 {
		t.Errorf("RequestCount = %d, want 42", resp.RequestCount)
	}
	if resp.LatencyP50MS != 42 || resp.LatencyP95MS != 42 || resp.LatencyP99MS != 42 {
		t.Errorf("latencies not all 42: %+v", resp)
	}
	if resp.ErrorRatePct != 42 || resp.ColdStartPct != 42 {
		t.Errorf("percentages not 42: %+v", resp)
	}
	if resp.WakeP95MS != 42 {
		t.Errorf("WakeP95MS = %v, want 42", resp.WakeP95MS)
	}
}

// TestAppMetrics_Fetch_DegradedFallback: one query errors and the
// whole response is zeroed + Source is the "degraded:" form.
func TestAppMetrics_Fetch_DegradedFallback(t *testing.T) {
	log, _ := captureLog(t)
	stub := &stubPromQL{fn: func(q string) (float64, error) {
		// Match the error-rate query by its unique label-set
		// (code=~"[45].."). No other query contains this regex.
		if strings.Contains(q, "[45]..") {
			return 0, errors.New("prometheus 503: down for maintenance")
		}
		return 7, nil
	}}

	resp, src := appmetrics.Fetch(context.Background(), stub, log, "app-1", "5m")
	if !strings.HasPrefix(src, appmetrics.SourceDegradedPrefix) {
		t.Errorf("src = %q, want prefix %q", src, appmetrics.SourceDegradedPrefix)
	}
	// Whole response zeroed on degraded (line 197 of the prior
	// implementation discards pre-populated fields).
	if resp.RequestCount != 0 {
		t.Errorf("RequestCount = %d, want 0 (degraded must zero)", resp.RequestCount)
	}
	if resp.ErrorRatePct != 0 {
		t.Errorf("ErrorRatePct = %v, want 0 (degraded must zero)", resp.ErrorRatePct)
	}
}

// TestAppMetrics_Fetch_NaNGuard_Float: stub returns "NaN" — SafeFloat
// coerces to 0.
func TestAppMetrics_Fetch_NaNGuard_Float(t *testing.T) {
	log, _ := captureLog(t)
	stub := &stubPromQL{fn: func(q string) (float64, error) {
		if strings.Contains(q, "histogram_quantile") {
			// Real PromQL returns NaN for histogram_quantile over
			// an empty window; we mirror that by returning NaN here.
			return math.NaN(), nil
		}
		return 0, nil
	}}

	resp, src := appmetrics.Fetch(context.Background(), stub, log, "app-1", "5m")
	if src != appmetrics.SourcePrometheus {
		t.Errorf("src = %q, want %q", src, appmetrics.SourcePrometheus)
	}
	if math.IsNaN(resp.LatencyP50MS) || resp.LatencyP50MS != 0 {
		t.Errorf("LatencyP50MS = %v, want 0 (NaN must be coerced)", resp.LatencyP50MS)
	}
	if math.IsNaN(resp.WakeP95MS) || resp.WakeP95MS != 0 {
		t.Errorf("WakeP95MS = %v, want 0 (NaN must be coerced)", resp.WakeP95MS)
	}
}

// TestAppMetrics_Fetch_NaNGuard_Percent: stub returns NaN for the
// error-rate query — SafePercent clamps to 0.
func TestAppMetrics_Fetch_NaNGuard_Percent(t *testing.T) {
	log, _ := captureLog(t)
	stub := &stubPromQL{fn: func(q string) (float64, error) {
		if strings.Contains(q, "[45]..") {
			return math.NaN(), nil
		}
		return 0, nil
	}}

	resp, src := appmetrics.Fetch(context.Background(), stub, log, "app-1", "5m")
	if src != appmetrics.SourcePrometheus {
		t.Errorf("src = %q, want %q", src, appmetrics.SourcePrometheus)
	}
	if math.IsNaN(resp.ErrorRatePct) || resp.ErrorRatePct != 0 {
		t.Errorf("ErrorRatePct = %v, want 0 (NaN must be coerced)", resp.ErrorRatePct)
	}
}

// TestAppMetrics_Fetch_NilClient: passing nil fetcher must NOT panic
// and must return the "degraded: prometheus not configured" form.
func TestAppMetrics_Fetch_NilClient(t *testing.T) {
	log, _ := captureLog(t)

	resp, src := appmetrics.Fetch(context.Background(), nil, log, "app-1", "5m")
	if !strings.Contains(src, "prometheus not configured") {
		t.Errorf("src = %q, want contains 'prometheus not configured'", src)
	}
	if resp.RequestCount != 0 || resp.WakeP95MS != 0 {
		t.Errorf("nil-client must zero: %+v", resp)
	}
}

// TestAppMetrics_Fetch_TypedNilClient pins the Go-typed-nil gotcha
// at the package seam. *promql.Client(nil) satisfies the PromQL
// interface with a non-nil interface value (type info without data);
// a bare `fetcher == nil` comparison returns false in that case.
// Without the typed-nil guard the call would dispatch into
// (*promql.Client)(nil).QueryScalar and surface as "promql: client
// not configured" — which is a wire-shape change vs. the original
// handlers_metrics.go behaviour. This test pins the "degraded:
// prometheus not configured" message even when the apid handler
// passes a typed-nil client through.
func TestAppMetrics_Fetch_TypedNilClient(t *testing.T) {
	log, _ := captureLog(t)

	var typedNil *promql.Client
	resp, src := appmetrics.Fetch(context.Background(), typedNil, log, "app-1", "5m")
	if !strings.Contains(src, "prometheus not configured") {
		t.Errorf("src = %q, want contains 'prometheus not configured'", src)
	}
	if resp.RequestCount != 0 {
		t.Errorf("typed-nil must zero: %+v", resp)
	}
}

// TestAppMetrics_Fetch_QueryFailure: each query failure mode degrades
// the response with a label that names the failed query.
func TestAppMetrics_Fetch_QueryFailure(t *testing.T) {
	cases := []struct {
		label        string
		failingQuery string
	}{
		{"request_count", "gateway_requests_total"},
		{"error_rate", "[45].."},
		{"cold_start", "gateway_cold_boot_total"},
		{"wake_p95", "gateway_wake_latency_seconds"},
		{"p50", "rate(gateway_request_duration_seconds_bucket"},
	}
	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
			log, _ := captureLog(t)
			stub := &stubPromQL{fn: func(q string) (float64, error) {
				if strings.Contains(q, tc.failingQuery) {
					return 0, errors.New("upstream timeout")
				}
				return 7, nil
			}}
			_, src := appmetrics.Fetch(context.Background(), stub, log, "app-1", "5m")
			if !strings.HasPrefix(src, appmetrics.SourceDegradedPrefix) {
				t.Errorf("src = %q, want degraded prefix", src)
			}
		})
	}
}

// TestAppMetrics_Ranges_ClosedSet pins the closed-set vocabulary in
// document order so a future reorder is a visible diff.
func TestAppMetrics_Ranges_ClosedSet(t *testing.T) {
	want := []string{"5m", "15m", "1h", "6h", "24h", "7d", "15d"}
	got := appmetrics.Ranges()
	if len(got) != len(want) {
		t.Fatalf("Ranges() len = %d, want %d (%v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("Ranges()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
	// Ranges must return a copy: mutating the returned slice MUST NOT
	// affect subsequent calls. Closes the silent-drift surface a
	// future caller would otherwise expose.
	got[0] = "999h"
	again := appmetrics.Ranges()
	if again[0] != "5m" {
		t.Errorf("Ranges() must return a copy; got[0] mutated to %q", again[0])
	}
}

// TestAppMetrics_IsValidRange table-drives the closed-set check.
func TestAppMetrics_IsValidRange(t *testing.T) {
	cases := []struct {
		rng  string
		want bool
	}{
		{"5m", true},
		{"15d", true},
		{"30d", false},
		{"", false},
		{"banana", false},
		{"5M", false}, // case-sensitive on purpose
	}
	for _, tc := range cases {
		if got := appmetrics.IsValidRange(tc.rng); got != tc.want {
			t.Errorf("IsValidRange(%q) = %v, want %v", tc.rng, got, tc.want)
		}
	}
}

// TestSafeFloat_BoundaryCases pins NaN/Inf/negative coercion.
func TestSafeFloat_BoundaryCases(t *testing.T) {
	cases := []struct {
		in, want float64
	}{
		{math.NaN(), 0},
		{math.Inf(1), 0},
		{math.Inf(-1), 0},
		{-1.0, 0},
		{0.0, 0},
		{1.5, 1.5},
	}
	for _, tc := range cases {
		if got := appmetrics.SafeFloat(tc.in); got != tc.want {
			t.Errorf("SafeFloat(%v) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

// TestSafePercent_BoundaryCases pins the [0,100] clamp.
//
// SafePercent composes SafeFloat first: NaN / +Inf / -Inf /
// negative inputs are coerced to 0 before the clamp runs, so
// +Inf lands at 0 (matching the byte-for-byte behaviour of the
// original cmd/apid/handlers_metrics.go safePercent). 100 is
// the upper edge; values > 100 clamp to 100.
func TestSafePercent_BoundaryCases(t *testing.T) {
	cases := []struct {
		in, want float64
	}{
		{math.NaN(), 0},   // NaN → 0 via SafeFloat
		{math.Inf(1), 0},  // +Inf → 0 via SafeFloat (not 100 — SafeFloat clamps Inf first)
		{math.Inf(-1), 0}, // -Inf → 0 via SafeFloat
		{-1.0, 0},         // negative → 0 via SafeFloat
		{50.0, 50},        // in-range unchanged
		{100.0, 100},      // edge
		{150.0, 100},      // over-100 clamps
	}
	for _, tc := range cases {
		if got := appmetrics.SafePercent(tc.in); got != tc.want {
			t.Errorf("SafePercent(%v) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

// TestSafeRoundNonNeg_BoundaryCases mirrors TestSafeFloat — the
// helper exists for documentation, not for different behaviour.
func TestSafeRoundNonNeg_BoundaryCases(t *testing.T) {
	cases := []struct {
		in, want float64
	}{
		{math.NaN(), 0},
		{-5.0, 0},
		{7.4, 7.4},
		{7.6, 7.6}, // rounding policy lives at the int64(...) call site
	}
	for _, tc := range cases {
		if got := appmetrics.SafeRoundNonNeg(tc.in); got != tc.want {
			t.Errorf("SafeRoundNonNeg(%v) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

// TestFetch_LogInjectionGuard asserts the CodeQL two-call sanitiser
// pattern (alert #117) — err string with \r\n in the message must
// NOT be logged with those bytes intact. We capture the slog JSON
// output and assert neither CR nor LF appear in the `err` field.
func TestFetch_LogInjectionGuard(t *testing.T) {
	log, buf := captureLog(t)
	stub := &stubPromQL{fn: func(_ string) (float64, error) {
		// CR/LF is the injection vector CodeQL alert #117 pins.
		return 0, fmt.Errorf("evil msg\r\nfake log line 2")
	}}

	_, _ = appmetrics.Fetch(context.Background(), stub, log, "app-1", "5m")

	// Find every JSON line; assert none of them contain a raw CR or LF.
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		// Each line is a single slog JSON record; the CR/LF inside
		// the err field should have been stripped.
		if strings.Contains(line, "\r") {
			t.Errorf("log line contains CR: %q", line)
		}
		// Decode just the `err` field to assert it's free of \n.
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("log line not JSON: %v (line=%q)", err, line)
		}
		if errStr, ok := rec["err"].(string); ok {
			if strings.Contains(errStr, "\n") || strings.Contains(errStr, "\r") {
				t.Errorf("log err field contains CR/LF: %q", errStr)
			}
		}
	}
}

// TestFetch_NilLogger: passing nil logger must NOT panic and must
// fall back to slog.Default(). Cheap coverage for the nil-coercion
// path that handlers in production also rely on.
func TestFetch_NilLogger(t *testing.T) {
	stub := okStub(7)

	resp, src := appmetrics.Fetch(context.Background(), stub, nil, "app-1", "5m")
	if src != appmetrics.SourcePrometheus {
		t.Errorf("src = %q, want %q", src, appmetrics.SourcePrometheus)
	}
	if resp.RequestCount != 7 {
		t.Errorf("RequestCount = %d, want 7", resp.RequestCount)
	}
}

// Compile-time check that api.AppMetricsResponse is the wire struct
// we expect. A future pkg/api change that splits the struct would
// surface here.
var _ = func() bool { var x api.AppMetricsResponse; return x.AppID == "" }()
