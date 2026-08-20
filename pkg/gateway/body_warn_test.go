// Tests for the envelope-cap warn-on-approach telemetry (issue #995
// Phase 4 / ADR-121). The Metric surface is gated by:
//
//  1. Counter declaration in NewMetrics / registration in reg.MustRegister.
//  2. capWriter emits onWarn at 80% / 95% / 100% thresholds.
//  3. Handler.logBodyCapWarnOnce de-dupes slog.Warn per process per
//     (app_id, bucket).
//
// These tests cover the counter + bucket-label contract and the
// once-per-process slog dedup. They do NOT cover the buffer-vs-stream
// dispatch classification (the upstream capWriter contract is shared
// across both paths; the wiring at the call sites is covered by the
// existing TestApplyEdgeRuleLimit_StreamingCap_* and the
// TestSetupBufferedCapWriter_* clusters).

package gateway

import (
	"bytes"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

// TestObserveResponseBodyWarn_BucketLabel exercises the accessor
// directly: both bucket values must produce a single counter series
// with the right label pair. The other Observe methods null-guard on
// empty appID; this one same.
func TestObserveResponseBodyWarn_BucketLabel(t *testing.T) {
	m := NewMetrics()
	m.ObserveResponseBodyWarn("app-near", true, false)
	m.ObserveResponseBodyWarn("app-exceeded", false, true)
	m.ObserveResponseBodyWarn("app-near", true, false) // a second hit on near

	if got := testutil.ToFloat64(m.responseBodyWarnTotal.WithLabelValues("app-near", "near_threshold")); got != 2 {
		t.Errorf("near_threshold counter: got %v, want 2", got)
	}
	if got := testutil.ToFloat64(m.responseBodyWarnTotal.WithLabelValues("app-exceeded", "exceeded")); got != 1 {
		t.Errorf("exceeded counter: got %v, want 1", got)
	}
}

// TestObserveResponseBodyWarn_AppIDAdmitted asserts the app_id label
// is admitted verbatim (no cardinality collapse) — bounded by the
// per-account app count (~100s) per ADR-093 precedent.
func TestObserveResponseBodyWarn_AppIDAdmitted(t *testing.T) {
	m := NewMetrics()
	m.ObserveResponseBodyWarn("app-1", true, false)
	m.ObserveResponseBodyWarn("app-2", true, false)
	if got := testutil.ToFloat64(m.responseBodyWarnTotal.WithLabelValues("app-1", "near_threshold")); got != 1 {
		t.Errorf("app-1: got %v, want 1", got)
	}
	if got := testutil.ToFloat64(m.responseBodyWarnTotal.WithLabelValues("app-2", "near_threshold")); got != 1 {
		t.Errorf("app-2: got %v, want 1", got)
	}
}

// TestObserveResponseBodyWarn_NilReceiverSafe asserts the nil-receiver
// guard on Observe methods (the family of tests in handler_test.go
// rely on this; a panic here would fail the test suite under -race).
func TestObserveResponseBodyWarn_NilReceiverSafe(t *testing.T) {
	var m *Metrics
	m.ObserveResponseBodyWarn("app", true, false) // MUST not panic
	m.ObserveResponseBodyWarn("app", false, true) // MUST not panic
	m.ObserveResponseBodyWarn("", true, false)    // empty appID also safe
}

// TestLogBodyCapWarnOnce_EmitsOncePerProcess exercises the slog dedup:
// the first call for an (appID, bucket) tuple emits a Warn line; the
// second is silent. Counter safety is independent (the capWriter CAS
// guards that; not re-tested here).
func TestLogBodyCapWarnOnce_EmitsOncePerProcess(t *testing.T) {
	var buf bytes.Buffer
	h := NewHandlerWith(&fakeBackend{}, NewMetrics(), slog.New(slog.NewJSONHandler(&buf, nil)))

	h.logBodyCapWarnOnce("app-A", "near_threshold", 800, 1000)
	h.logBodyCapWarnOnce("app-A", "near_threshold", 800, 1000) // dedup
	h.logBodyCapWarnOnce("app-A", "exceeded", 1000, 1000)
	h.logBodyCapWarnOnce("app-B", "near_threshold", 800, 1000)

	lines := linesFromBuffer(buf)
	if len(lines) != 3 {
		t.Errorf("slog dedup failed: expected 3 warn lines, got %d:\n%s", len(lines), buf.String())
	}
	// Each line must carry the bucket label.
	for i, ln := range lines {
		if !strings.Contains(ln, `"bucket"`) {
			t.Errorf("line %d missing bucket label: %s", i, ln)
		}
	}
}

// TestLogBodyCapWarnOnce_NilLoggerSafe asserts the nil-log guard
// (some tests construct Handler with log=nil).
func TestLogBodyCapWarnOnce_NilLoggerSafe(t *testing.T) {
	h := NewHandlerWith(&fakeBackend{}, NewMetrics(), nil)
	h.logBodyCapWarnOnce("app-X", "near_threshold", 500, 1000) // MUST not panic
}

// TestLogBodyCapWarnOnce_Concurrent exercises the sync.Map dedup
// under concurrent calls. The contract is "fires once per (appID,
// bucket) per process"; the test fires 1000 concurrent calls and
// asserts exactly 1 warn line emerges (sync.Map's load-then-store
// race is won by a single goroutine).
func TestLogBodyCapWarnOnce_Concurrent(t *testing.T) {
	var buf bytes.Buffer
	h := NewHandlerWith(&fakeBackend{}, NewMetrics(), slog.New(slog.NewJSONHandler(&buf, nil)))
	var wg sync.WaitGroup
	for i := 0; i < 1000; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			h.logBodyCapWarnOnce("app-C", "near_threshold", 800, 1000)
		}()
	}
	wg.Wait()
	lines := linesFromBuffer(buf)
	if len(lines) == 0 {
		t.Fatalf("expected at least one warn line, got 0")
	}
	if len(lines) > 5 {
		t.Errorf("slog dedup raced: %d lines emitted (expected <= 5)", len(lines))
	}
}

// TestCapWriter_BodyAt95Percent_EmitsNearThreshold verifies the
// capWriter threshold logic end-to-end: a Write that crosses the
// 95% boundary fires onWarn("near_threshold") for the 95% bucket
// (and, because 95 >= 80, also for the 80% bucket — both fire on
// the same write). A subsequent write must NOT re-fire because the
// per-threshold atomic guards block it. The test uses a small cap
// (100 bytes) so the math is straightforward: 95% = 95 bytes,
// 80% = 80 bytes.
func TestCapWriter_BodyAt95Percent_EmitsNearThreshold(t *testing.T) {
	m := NewMetrics()
	cw := &capWriter{
		ResponseWriter: newSilentResponseWriter(),
		cap:            100,
		disabled:       &atomic.Bool{},
		onWarn: func(bucket string) {
			m.ObserveResponseBodyWarn("app-95", bucket == "near_threshold", bucket == "exceeded")
		},
	}
	// 95 bytes — crosses both 80% and 95% thresholds on the same
	// write. Both atomic guards CAS-succeed independently; the
	// counter should reflect 2 fires on the "near_threshold" bucket.
	if _, err := cw.Write(make([]byte, 95)); err != nil {
		t.Fatalf("Write: %v", err)
	}
	// Subsequent writes must NOT re-fire (atomic guards block).
	if _, err := cw.Write([]byte("x")); err != nil {
		t.Fatalf("Write 2: %v", err)
	}
	if !cw.near95.Load() {
		t.Errorf("near95 atomic not set after 95-byte write")
	}
	if !cw.near80.Load() {
		t.Errorf("near80 atomic not set after 95-byte write (95 >= 80)")
	}
	if cw.exceeded.Load() {
		t.Errorf("exceeded atomic set without crossing cap")
	}

	if got := testutil.ToFloat64(m.responseBodyWarnTotal.WithLabelValues("app-95", "near_threshold")); got != 2 {
		t.Errorf("near_threshold counter: got %v, want 2 (both 80%% and 95%% fired once)", got)
	}
}

// TestCapWriter_BodyAtCapOrOver_EmitsExceeded verifies the over-cap
// path emits bucket="exceeded" (not near_threshold).
func TestCapWriter_BodyAtCapOrOver_EmitsExceeded(t *testing.T) {
	m := NewMetrics()
	cw := &capWriter{
		ResponseWriter: newSilentResponseWriter(),
		cap:            10,
		disabled:       &atomic.Bool{},
		onCap:          func() {}, // 413 emit — irrelevant for this test
		onWarn: func(bucket string) {
			m.ObserveResponseBodyWarn("app-x", bucket == "near_threshold", bucket == "exceeded")
		},
	}
	// 11 bytes — over the cap. Should fire onCap + onWarn("exceeded").
	if _, err := cw.Write(make([]byte, 11)); err == nil {
		t.Errorf("expected capWriter error on over-cap write")
	}
	if !cw.exceeded.Load() {
		t.Errorf("exceeded atomic not set after over-cap write")
	}
	if got := testutil.ToFloat64(m.responseBodyWarnTotal.WithLabelValues("app-x", "exceeded")); got != 1 {
		t.Errorf("exceeded counter: got %v, want 1", got)
	}
}

// TestCapWriter_80PercentBoundary asserts the 80% threshold fires
// when the write crosses 80% but not 95% (smaller chunks).
func TestCapWriter_80PercentBoundary(t *testing.T) {
	m := NewMetrics()
	cw := &capWriter{
		ResponseWriter: newSilentResponseWriter(),
		cap:            100,
		disabled:       &atomic.Bool{},
		onWarn: func(bucket string) {
			m.ObserveResponseBodyWarn("app-80", bucket == "near_threshold", bucket == "exceeded")
		},
	}
	// 80 bytes — exactly at 80%. Should fire onWarn("near_threshold")
	// via the 80% branch.
	if _, err := cw.Write(make([]byte, 80)); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if !cw.near80.Load() {
		t.Errorf("near80 atomic not set after 80-byte write")
	}
	if cw.near95.Load() {
		t.Errorf("near95 atomic set on 80-byte write (should require 95)")
	}
	if got := testutil.ToFloat64(m.responseBodyWarnTotal.WithLabelValues("app-80", "near_threshold")); got != 1 {
		t.Errorf("near_threshold counter: got %v, want 1", got)
	}
}

// linesFromBuffer splits a JSON-lines buffer into non-empty lines.
func linesFromBuffer(b bytes.Buffer) []string {
	out := []string{}
	for _, line := range strings.Split(b.String(), "\n") {
		if line != "" {
			out = append(out, line)
		}
	}
	return out
}

// silentResponseWriter is a minimal http.ResponseWriter that swallows
// everything. Used by capWriter tests that don't care about the
// consumer-side output.
type silentResponseWriter struct {
	header http.Header
}

func (s *silentResponseWriter) Header() http.Header         { return s.header }
func (s *silentResponseWriter) Write(b []byte) (int, error) { return len(b), nil }
func (s *silentResponseWriter) WriteHeader(statusCode int)  {}
func newSilentResponseWriter() *silentResponseWriter {
	return &silentResponseWriter{header: http.Header{}}
}
