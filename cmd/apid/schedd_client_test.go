// Whitebox tests for the cmd/apid/schedd_client.go surface. The
// receive-pump refactor (issue #254 / PR-A Step 4) needs a
// controllable scheddClient so serveAppLogs can be exercised in
// isolation — the existing stubScheddClient (cmd/apid/schedd_client.go)
// returns codes.Unimplemented immediately and exits before the pump
// can be tested.
//
// This file stays in `package main` deliberately: scheddClient,
// schedLogStream, and schedLogFrame are unexported in server.go.
// Per project memory "whitebox-test-file-pattern", narrowly-scoped
// `package <x>` test files are preferred when the surface under test
// is unexported.
package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/onebox-faas/faas/pkg/state"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// controllableScheddClient is the whitebox seam for tests that need
// to drive serveAppLogs's receive pump against a programmable
// upstream. The single RPC returned by StreamAppLogs is a
// controllableScheddLogStream whose Recv blocks until the test sends
// a frame, an error, or closes the stream.
//
// Each test gets a fresh client with its own stream so concurrent
// tests do not interfere; the same client can be used for a single
// contiguous sequence of frames by sharing the stream handle.
type controllableScheddClient struct {
	stream schedLogStream
}

func (c *controllableScheddClient) StreamAppLogs(_ context.Context, _ string, _ int64) (schedLogStream, error) {
	return c.stream, nil
}

// controllableScheddLogStream is the per-test stream. Frames and
// errors are queued on buffered channels so the test can drive
// timing from the outside:
//
//   - frames: pushed via pushFrame; Recv returns them in order.
//   - errCh: closed (returns the configured error) by calling finish.
//   - closeCh: closed without an error by calling closeStream —
//     Recv returns io.EOF so the handler's clean-EOF branch fires.
//
// The select inside Recv makes this safe under ctx cancel: a
// production-style ctx-cancel unblocks the receive.
type controllableScheddLogStream struct {
	frames  chan schedLogFrame
	errCh   chan error
	closed  bool
	closeMu sync.Mutex
}

func newControllableScheddStream() *controllableScheddLogStream {
	return &controllableScheddLogStream{
		frames: make(chan schedLogFrame, 16),
		errCh:  make(chan error, 1),
	}
}

func (s *controllableScheddLogStream) pushFrame(f schedLogFrame) {
	s.frames <- f
}

func (s *controllableScheddLogStream) finish(err error) {
	s.closeMu.Lock()
	if s.closed {
		s.closeMu.Unlock()
		return
	}
	s.closed = true
	s.errCh <- err
	s.closeMu.Unlock()
}

func (s *controllableScheddLogStream) closeStream() {
	s.finish(io.EOF)
}

func (s *controllableScheddLogStream) Recv() (schedLogFrame, error) {
	// Prefer a queued frame; only fall through to errCh if no
	// frame is ready. This mirrors real gRPC behaviour: data
	// frames dominate; error/EOF closes the stream.
	select {
	case f, ok := <-s.frames:
		if !ok {
			// Channel was closed by the test = treated as EOF.
			return schedLogFrame{}, io.EOF
		}
		return f, nil
	default:
	}
	select {
	case f := <-s.frames:
		return f, nil
	case err := <-s.errCh:
		return schedLogFrame{}, err
	}
}

// runServeAppLogs drives serveAppLogs against a fresh
// controllableScheddClient and returns the recorder body it wrote
// plus the error (always nil — serveAppLogs doesn't return).
//
// appID is opaque; the function does not touch the store, so the
// "no running instance" 404 from loadApp does not fire here. This
// isolates the receive-pump paths from the rest of the handler.
func runServeAppLogs(t *testing.T, srv *server, stream *controllableScheddLogStream, heartbeat, backstop time.Duration) string {
	t.Helper()
	srv.appLogsHeartbeat = heartbeat
	srv.appLogsBackstop = backstop
	cli := &controllableScheddClient{stream: stream}
	srv.WithScheddClientForTest(cli)
	rec := newFlusherRecorder()
	done := make(chan struct{})
	go func() {
		srv.serveAppLogs(context.Background(), rec, rec, "app-1", 0)
		close(done)
	}()
	<-done
	return rec.body.String()
}

// --- helpers ------------------------------------------------------------

// flusherRecorder satisfies both http.ResponseWriter and
// http.Flusher so serveAppLogs's `if flusher != nil` checks (and
// the writeAppLogEvent path) exercise the same code they would in
// production. Body is captured into a strings.Builder-equivalent.
type flusherRecorder struct {
	body *safeBuffer
	h    http.Header
}

func newFlusherRecorder() *flusherRecorder {
	return &flusherRecorder{body: &safeBuffer{}, h: http.Header{}}
}

func (r *flusherRecorder) Header() http.Header         { return r.h }
func (r *flusherRecorder) Write(b []byte) (int, error) { return r.body.Write(b) }
func (r *flusherRecorder) WriteHeader(int)             {}

// Flush satisfies http.Flusher; the body has already been written.
func (r *flusherRecorder) Flush() {}

// safeBuffer is shared in `package main` from handlers_github_test.go;
// this file reuses it. See memory "e2etest safeBuffer" for the
// race-safe pattern.

// --- the seam setter ----------------------------------------------------

// WithScheddClientForTest installs a controllableScheddClient on the
// server under test. Production code uses WithScheddClient (PR-B /
// gatewayd-streaming follow-up); this helper exists so the receive-
// pump tests don't need a real gRPC dial. It bypasses the nil guard
// in WithScheddClient so tests can swap out the stub entirely.
func (s *server) WithScheddClientForTest(c scheddClient) {
	s.schedd = c
}

// --- the receive-pump cases --------------------------------------------

// TestServeAppLogs_BackstopFiresOnIdleStream pins the bug from
// pre-fix: the old `select { default: }` loop only checked timers
// between frames, so the backstop never fired on a quiet stream.
// With the fix, serveAppLogs returns within the backstop interval
// and emits `event: end {reason: timeout}`.
//
// Pre-fix output (for redox replay): the old code blocked on
// stream.Recv() indefinitely; this test would have hung until the
// httptest timeout fired.
func TestServeAppLogs_BackstopFiresOnIdleStream(t *testing.T) {
	srv := newServer(state.NewMemStore(), slog.New(slog.NewTextHandler(io.Discard, nil)), "example.com", noopNotifier{})
	stream := newControllableScheddStream()
	// Do NOT push any frame — the stream stays idle.
	body := runServeAppLogs(t, srv, stream,
		10*time.Second,      // heartbeat: long; we want the backstop to win
		50*time.Millisecond, // backstop: short enough to bound the test
	)
	if !strings.Contains(body, `event: end`) {
		t.Fatalf("no terminal end frame in body: %q", body)
	}
	if !strings.Contains(body, `"reason":"timeout"`) {
		t.Errorf("missing timeout reason: %q", body)
	}
}

// TestServeAppLogs_CtxCancelReturnsWithoutTerminalFrame pins the
// goroutine-leak guarantee: cancelling the request context exits the
// handler without emitting a terminal frame (the client is gone;
// writing `event: end` to a torn-down ResponseWriter is a no-op + a
// goroutine-wake-up trap).
func TestServeAppLogs_CtxCancelReturnsWithoutTerminalFrame(t *testing.T) {
	srv := newServer(state.NewMemStore(), slog.New(slog.NewTextHandler(io.Discard, nil)), "example.com", noopNotifier{})
	stream := newControllableScheddStream()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	srv.appLogsHeartbeat = 10 * time.Second
	srv.appLogsBackstop = 10 * time.Second
	srv.WithScheddClientForTest(&controllableScheddClient{stream: stream})
	rec := newFlusherRecorder()
	go func() {
		srv.serveAppLogs(ctx, rec, rec, "app-1", 0)
		close(done)
	}()
	// Let the receive goroutine settle into Recv().
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("serveAppLogs did not return after ctx cancel")
	}
	body := rec.body.String()
	if strings.Contains(body, "event: end") {
		t.Errorf("ctx cancel must not emit terminal frame; body=%q", body)
	}
}

// TestServeAppLogs_HeartbeatOnIdleStream pins the SSE liveness
// contract: htmx-ext-sse treats silence as a dead connection, so the
// heartbeat must fire on a quiet stream within the configured window.
func TestServeAppLogs_HeartbeatOnIdleStream(t *testing.T) {
	srv := newServer(state.NewMemStore(), slog.New(slog.NewTextHandler(io.Discard, nil)), "example.com", noopNotifier{})
	stream := newControllableScheddStream()

	// Emit exactly one frame, then sit idle. The handler should
	// emit the frame, then heartbeats until the backstop closes
	// the stream. Expect at least one `:` heartbeat before the
	// terminal `event: end`.
	srv.appLogsHeartbeat = 30 * time.Millisecond
	srv.appLogsBackstop = 10 * time.Second // long enough that heartbeats dominate
	srv.WithScheddClientForTest(&controllableScheddClient{stream: stream})
	done := make(chan struct{})
	go func() {
		// After the pump is reading, push one frame then sit idle.
		time.Sleep(10 * time.Millisecond)
		stream.pushFrame(schedLogFrame{
			InstanceID: "i-1", Seq: 1, Stream: "stdout",
			Line:      "hello\n",
			WrittenAt: time.Date(2026, 7, 29, 0, 0, 0, 0, time.UTC),
		})
		// Stay idle until the backstop fires; block on done so the
		// goroutine doesn't leak.
		<-done
	}()
	body := runServeAppLogs(t, srv, stream,
		30*time.Millisecond,
		200*time.Millisecond, // bound the test
	)
	close(done)
	if !strings.Contains(body, "event: log") {
		t.Errorf("missing log frame: %q", body)
	}
	heartbeats := strings.Count(body, ":\n\n")
	if heartbeats < 1 {
		t.Errorf("expected at least one heartbeat, got %d in body=%q", heartbeats, body)
	}
	if !strings.Contains(body, `"reason":"timeout"`) {
		t.Errorf("backstop should also fire; body=%q", body)
	}
}

// TestServeAppLogs_FramesRenderInOrder pins the contract that frames
// reach the client in the order Recv returned them. The bounded
// channel-of-1 introduces drop semantics; this test ensures the
// drop path doesn't reshuffle or skip.
func TestServeAppLogs_FramesRenderInOrder(t *testing.T) {
	srv := newServer(state.NewMemStore(), slog.New(slog.NewTextHandler(io.Discard, nil)), "example.com", noopNotifier{})
	stream := newControllableScheddStream()

	srv.appLogsHeartbeat = 10 * time.Second
	srv.appLogsBackstop = 200 * time.Millisecond
	srv.WithScheddClientForTest(&controllableScheddClient{stream: stream})
	rec := newFlusherRecorder()
	done := make(chan struct{})
	go func() {
		// Emit two frames early enough to land before the backstop.
		stream.pushFrame(schedLogFrame{InstanceID: "i-1", Seq: 1, Stream: "stdout", Line: "first\n", WrittenAt: time.Now()})
		stream.pushFrame(schedLogFrame{InstanceID: "i-1", Seq: 2, Stream: "stdout", Line: "second\n", WrittenAt: time.Now()})
		srv.serveAppLogs(context.Background(), rec, rec, "app-1", 0)
		close(done)
	}()
	<-done
	body := rec.body.String()
	// Search by Seq value (an int, never escaped) so we don't
	// depend on Go-map key iteration order or JSON escaping of
	// the line value.
	i1 := strings.Index(body, `"seq":1,`)
	i2 := strings.Index(body, `"seq":2,`)
	if i1 < 0 || i2 < 0 {
		t.Fatalf("missing frames: body=%q", body)
	}
	if i1 >= i2 {
		t.Errorf("frames out of order: first@%d second@%d", i1, i2)
	}
}

// TestServeAppLogs_CleanEmitsEmptyEndEvent pins the io.EOF path:
// schedd closes the stream cleanly (recv goroutine sends nil frame,
// receive goroutine exits and closes recvCh; handler emits empty
// event: end).
func TestServeAppLogs_CleanEndEmitsEmptyEndEvent(t *testing.T) {
	srv := newServer(state.NewMemStore(), slog.New(slog.NewTextHandler(io.Discard, nil)), "example.com", noopNotifier{})
	stream := newControllableScheddStream()

	srv.appLogsHeartbeat = 10 * time.Second
	srv.appLogsBackstop = 10 * time.Second
	srv.WithScheddClientForTest(&controllableScheddClient{stream: stream})
	rec := newFlusherRecorder()
	done := make(chan struct{})
	go func() {
		stream.pushFrame(schedLogFrame{InstanceID: "i-1", Seq: 1, Stream: "stdout", Line: "hi\n", WrittenAt: time.Now()})
		stream.closeStream() // -> io.EOF on Recv
		srv.serveAppLogs(context.Background(), rec, rec, "app-1", 0)
		close(done)
	}()
	<-done
	body := rec.body.String()
	if !strings.Contains(body, "event: log") {
		t.Errorf("missing log frame: %q", body)
	}
	if !strings.Contains(body, "event: end\ndata: {}\n\n") {
		t.Errorf("expected empty end frame on clean close: %q", body)
	}
	if strings.Contains(body, `"reason"`) {
		t.Errorf("clean close must not carry a reason: %q", body)
	}
}

// TestServeAppLogs_GenericErrorDelegatesToRenderAppLogsError pins the
// error-delegation contract: a non-EOF, non-grace-coded error from
// the stream flows through recvCh and out the renderAppLogsError
// path, which emits a degraded frame + terminal end with
// reason=schedd_unreachable. Mirrors the TestStreamAppLogs_StubDegradedFrame
// expectation but on the receive-pump path.
func TestServeAppLogs_GenericErrorDelegatesToRenderAppLogsError(t *testing.T) {
	srv := newServer(state.NewMemStore(), slog.New(slog.NewTextHandler(io.Discard, nil)), "example.com", noopNotifier{})
	stream := newControllableScheddStream()

	srv.appLogsHeartbeat = 10 * time.Second
	srv.appLogsBackstop = 10 * time.Second
	srv.WithScheddClientForTest(&controllableScheddClient{stream: stream})
	rec := newFlusherRecorder()
	done := make(chan struct{})
	go func() {
		stream.pushFrame(schedLogFrame{InstanceID: "i-1", Seq: 1, Stream: "stdout", Line: "first\n", WrittenAt: time.Now()})
		stream.finish(errors.New("vmmd dial failed"))
		srv.serveAppLogs(context.Background(), rec, rec, "app-1", 0)
		close(done)
	}()
	<-done
	body := rec.body.String()
	if !strings.Contains(body, "event: log") {
		t.Errorf("first frame dropped: %q", body)
	}
	if !strings.Contains(body, "event: degraded") {
		t.Errorf("missing degraded frame: %q", body)
	}
	if !strings.Contains(body, `"reason":"schedd_unreachable"`) {
		t.Errorf("missing terminal reason: %q", body)
	}
}

// TestServeAppLogs_NotFoundDelegatesToRenderAppLogsError mirrors the
// generic-error case for the parked-app path: state.ErrNotFound flows
// through the same renderAppLogsError mapping. (Today that mapping
// lifts *api.Problem, not raw NotFound — the test pins the current
// shape; if the mapping changes to lift raw NotFound too, this test
// will need to be updated alongside the handler.)
func TestServeAppLogs_NotFoundDelegatesToRenderAppLogsError(t *testing.T) {
	srv := newServer(state.NewMemStore(), slog.New(slog.NewTextHandler(io.Discard, nil)), "example.com", noopNotifier{})
	stream := newControllableScheddStream()

	srv.appLogsHeartbeat = 10 * time.Second
	srv.appLogsBackstop = 10 * time.Second
	srv.WithScheddClientForTest(&controllableScheddClient{stream: stream})
	rec := newFlusherRecorder()
	done := make(chan struct{})
	go func() {
		stream.finish(status.Error(codes.NotFound, "state: not found"))
		srv.serveAppLogs(context.Background(), rec, rec, "app-1", 0)
		close(done)
	}()
	<-done
	body := rec.body.String()
	if !strings.Contains(body, "event: degraded") {
		t.Errorf("missing degraded frame: %q", body)
	}
	if !strings.Contains(body, "event: end") {
		t.Errorf("missing terminal end: %q", body)
	}
}

// suppress unused-import warnings when io.EOF is no longer
// referenced directly inside this file's tests after refactors.
var _ = io.EOF
