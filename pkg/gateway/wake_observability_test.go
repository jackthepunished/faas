// wake_observability_test.go — coverage for the wake-timing
// helpers in pkg/gateway/wake_timing.go (§12
// `gateway_wake_latency_seconds`).
//
// wake_timing.go owns the firstByteRecorder + firstByteRoundTripper
// plumbing that lets the handler observe "request-received to first
// upstream response byte" via httptrace.ClientTrace.GotFirstResponseByte.
// No existing test file covered this surface (zero coverage on the
// recorder's mutex-guarded state, the nil-ctx path on
// WithFirstByteRecorder, the nil-request path on FirstByteFrom, or
// the RoundTrip trace wiring).
//
// Whitebox test (package gateway) matching the existing test files.
package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestFirstByteRecorder_RecordRead exercises the happy path on
// the recorder: stamp a timestamp, read it back. Pins the
// set=true transition (otherwise FirstByteFrom returns ok=false
// and the handler falls back to full-duration observation).
func TestFirstByteRecorder_RecordRead(t *testing.T) {
	rec := &firstByteRecorder{}
	now := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	rec.record(now)
	got, ok := rec.read()
	if !ok {
		t.Fatal("read after record: ok = false, want true")
	}
	if !got.Equal(now) {
		t.Errorf("read at = %v, want %v", got, now)
	}
}

// TestFirstByteRecorder_ReadBeforeRecord asserts that a fresh
// recorder returns ok=false — the handler MUST distinguish
// "not stamped yet" (no first byte received) from "stamped at
// zero" (theoretical epoch observation) so it can fall back to
// a full-duration observation + warning log.
func TestFirstByteRecorder_ReadBeforeRecord(t *testing.T) {
	rec := &firstByteRecorder{}
	at, ok := rec.read()
	if ok {
		t.Errorf("read on fresh recorder: ok = true, want false (set=false sentinel)")
	}
	if !at.IsZero() {
		t.Errorf("read on fresh recorder: at = %v, want zero", at)
	}
}

// TestFirstByteRecorder_ConcurrentRecordRead is the -race
// surface. The recorder's mutex is the only synchronisation
// between the trace callback (RoundTrip goroutine) and the
// handler's read. Multiple recorders concurrent with reads must
// not trip the race detector.
func TestFirstByteRecorder_ConcurrentRecordRead(t *testing.T) {
	rec := &firstByteRecorder{}
	const n = 200
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(2)
		go func(i int) {
			defer wg.Done()
			rec.record(time.Unix(int64(i), 0))
		}(i)
		go func() {
			defer wg.Done()
			_, _ = rec.read()
		}()
	}
	wg.Wait()
	if _, ok := rec.read(); !ok {
		t.Error("after concurrent: ok = false, want true (last writer wins)")
	}
}

// TestWithFirstByteRecorder_NilRecNoOp pins the nil-recorder
// short-circuit: passing nil returns ctx unchanged instead of
// allocating a recorder nobody will use.
func TestWithFirstByteRecorder_NilRecNoOp(t *testing.T) {
	ctx := context.Background()
	got := WithFirstByteRecorder(ctx, nil)
	if got != ctx {
		t.Error("WithFirstByteRecorder(nil): ctx was replaced; want no-op")
	}
}

// TestWithFirstByteRecorder_InstallsValue pins the happy path:
// a non-nil recorder is installed and readable via
// FirstByteFrom. The recorder is what the handler reads back
// to compute the wake-latency delta.
func TestWithFirstByteRecorder_InstallsValue(t *testing.T) {
	ctx := context.Background()
	rec := &firstByteRecorder{}
	got := WithFirstByteRecorder(ctx, rec)

	req, err := http.NewRequestWithContext(got, http.MethodGet, "http://example.com", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	at, ok := FirstByteFrom(req)
	if ok {
		t.Error("FirstByteFrom on unstamped recorder: ok = true, want false")
	}
	if !at.IsZero() {
		t.Errorf("FirstByteFrom: at = %v, want zero", at)
	}
}

// TestFirstByteFrom_NilRequest pins the nil-request guard. A nil
// *http.Request is the "this shouldn't happen but if it does
// don't panic" guard. Returns zero time + ok=false.
func TestFirstByteFrom_NilRequest(t *testing.T) {
	at, ok := FirstByteFrom(nil)
	if ok {
		t.Error("FirstByteFrom(nil): ok = true, want false")
	}
	if !at.IsZero() {
		t.Errorf("FirstByteFrom(nil): at = %v, want zero", at)
	}
}

// TestFirstByteFrom_NoRecorderInContext pins the
// "context missing the key" branch. The request was not
// prepared via WithFirstByteRecorder, so the read falls back
// to ok=false (handler should full-duration observation).
func TestFirstByteFrom_NoRecorderInContext(t *testing.T) {
	req, err := http.NewRequest(http.MethodGet, "http://example.com", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	at, ok := FirstByteFrom(req)
	if ok {
		t.Error("FirstByteFrom on plain context: ok = true, want false")
	}
	if !at.IsZero() {
		t.Errorf("FirstByteFrom: at = %v, want zero", at)
	}
}

// TestFirstByteRoundTripper_StampsOnFirstByte is the integration
// test: a real http.RoundTripper that uses the
// firstByteRoundTripper wrapper must stamp the recorder at the
// moment the first upstream response byte arrives.
//
// We use httptest.NewServer (loopback, no TCP) so the test
// exercises the real RoundTripper code path including
// httptrace.WithClientTrace plumbing.
func TestFirstByteRoundTripper_StampsOnFirstByte(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// Sleep so the test is robust against clock-tick jitter.
		// The trace fires the moment the handler's WriteHeader
		// drains the headers into the response writer.
		time.Sleep(20 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	t.Cleanup(srv.Close)

	rec := &firstByteRecorder{}
	ctx := WithFirstByteRecorder(context.Background(), rec)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}

	rt := newFirstByteRoundTripper(http.DefaultTransport)
	start := time.Now()
	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	defer resp.Body.Close()

	at, ok := rec.read()
	if !ok {
		t.Fatal("recorder was not stamped after RoundTrip; trace didn't fire")
	}
	// The stamp must land AFTER start (when the trace fired) and
	// BEFORE we read it now.
	if at.Before(start) {
		t.Errorf("stamped at %v, before request started at %v", at, start)
	}
	if at.After(time.Now()) {
		t.Errorf("stamped at %v, after read at %v", at, time.Now())
	}
	// FirstByteFrom on the inbound request returns the same stamp.
	got, ok := FirstByteFrom(req)
	if !ok {
		t.Error("FirstByteFrom: ok = false, want true")
	}
	if !got.Equal(at) {
		t.Errorf("FirstByteFrom = %v, want %v", got, at)
	}
}

// TestFirstByteRoundTripper_NoRecorderStillProxies covers the
// no-recorder branch: a request without WithFirstByteRecorder
// must still round-trip successfully (the trace is a no-op
// when rec is nil; the test guards against a future regression
// where the wrapper would short-circuit).
func TestFirstByteRoundTripper_NoRecorderStillProxies(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("hello"))
	}))
	t.Cleanup(srv.Close)

	req, err := http.NewRequest(http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	rt := newFirstByteRoundTripper(http.DefaultTransport)
	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip without recorder: %v", err)
	}
	defer resp.Body.Close()
	buf := make([]byte, 16)
	n, _ := resp.Body.Read(buf)
	if got := string(buf[:n]); got != "hello" {
		t.Errorf("body = %q, want %q", got, "hello")
	}
}

// TestFirstByteRoundTripper_UpstreamErrorStillClean covers the
// "upstream connection refused before first byte" branch:
// httptrace.ClientTrace.GotFirstResponseByte never fires, so
// the recorder stays unset. RoundTrip must surface the error
// to the caller (handler falls back to full-duration
// observation + warning log).
func TestFirstByteRoundTripper_UpstreamErrorStillClean(t *testing.T) {
	rec := &firstByteRecorder{}
	// Use an unreachable address (test never binds). httptest
	// reserved the port on this loopback range, but a
	// deliberately-wrong port is more deterministic.
	req, err := http.NewRequestWithContext(
		WithFirstByteRecorder(context.Background(), rec),
		http.MethodGet,
		"http://127.0.0.1:1/never",
		nil,
	)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	rt := newFirstByteRoundTripper(&http.Transport{})
	resp, err := rt.RoundTrip(req)
	// RoundTrip may return a non-nil resp with a non-nil error
	// (per http.RoundTripper contract). Close any returned body
	// defensively to satisfy bodyclose; in this test the
	// unreachable-host path typically leaves resp == nil.
	if resp != nil {
		defer resp.Body.Close()
	}
	if err == nil {
		t.Fatal("RoundTrip to unreachable host: err = nil, want non-nil")
	}
	// The trace never fired (no first byte ever arrived).
	if _, ok := rec.read(); ok {
		t.Error("recorder was stamped despite upstream failure; trace shouldn't fire")
	}
}

// TestNewFirstByteRoundTripper_WrapsInner pins the constructor:
// the inner transport is preserved on the returned wrapper.
func TestNewFirstByteRoundTripper_WrapsInner(t *testing.T) {
	inner := http.DefaultTransport
	rt := newFirstByteRoundTripper(inner)
	if rt.inner != inner {
		t.Errorf("inner = %p, want %p", rt.inner, inner)
	}
}

// TestRecorderSetAfterTraceFires is an adversarial fixture:
// the test reads body bytes that confirm "hello\n" landed on
// the wire, then reads the recorder. The recorder's
// GotFirstResponseByte callback fires when the response headers
// arrive, which happens before the body — so by the time
// resp.Body.Read returns, the recorder MUST be stamped.
//
// We assert this ordering invariant (headers arrive before
// body bytes) because the §12 SLO depends on it.
func TestRecorderSetAfterTraceFires(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// Write a non-empty body so the body read returns
		// something and the trace clearly fired before.
		_, _ = w.Write([]byte(strings.Repeat("a", 1024)))
	}))
	t.Cleanup(srv.Close)

	rec := &firstByteRecorder{}
	req, err := http.NewRequestWithContext(
		WithFirstByteRecorder(context.Background(), rec),
		http.MethodGet,
		srv.URL,
		nil,
	)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	rt := newFirstByteRoundTripper(http.DefaultTransport)
	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	defer resp.Body.Close()
	buf := make([]byte, 16)
	if _, err := resp.Body.Read(buf); err != nil {
		t.Fatalf("body read: %v", err)
	}
	if _, ok := rec.read(); !ok {
		t.Error("recorder not stamped by the time body bytes were read")
	}
}
