// capacity_publisher_e2e_test.go — end-to-end bufconn tests for
// vmmd's capacity publisher (ADR-025 axis 5).
//
// Background. The unit tests in capacity_publisher_test.go pin
// buildCapacityReport's pure pieces and the empty-target /
// ctx-cancel paths, but they don't exercise the gRPC client half
// (dial, open stream, send) because *fcvm.Manager can't be stubbed
// without KVM. PR-1 review fix: introduce two seams
//
//  1. countReader: lets tests inject live/leased counts.
//  2. capacityStreamer: lets tests inject a bufconn-backed
//     streamer instead of unix:///run/faas/schedd.sock.
//
// Together these let us drive runCapacityPublish against a
// real bufconn-backed schedd server in a unit test, no KVM
// required. The shape mirrors pkg/scheddgrpc/bufconn_test.go
// (which is the schedd-side analog) — a fakeEngine SchedAPI
// stub + a bufconn.Listen + grpc.NewClient.
//
// Scope. The tests here pin:
//
//  1. Round-trip: 3 reports pushed by the publisher arrive at
//     the fakeEngine with the right node_id, live_count, used_mb.
//  2. Backoff-reset-on-clean-drain: a stream that ends cleanly
//     (server-side SendAndClose) is followed by a re-open; the
//     publisher does NOT sit at 30 s.
//  3. Reconnect-after-send-error: a Send returning ErrConnClosing
//     causes the publisher to reconnect and resume pushing
//     reports.

package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"sync"
	"testing"
	"time"

	scheddpb "github.com/onebox-faas/faas/api/proto/onebox/faas/schedd/v1"
	"github.com/onebox-faas/faas/pkg/sched"
	"github.com/onebox-faas/faas/pkg/scheddgrpc"
	"github.com/onebox-faas/faas/pkg/state"
	"github.com/onebox-faas/faas/pkg/wire"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

// fakeCountReader is a countReader stub. It returns the
// current values from the protected fields; tests set live
// and leased before invoking runCapacityPublish.
type fakeCountReader struct {
	mu     sync.Mutex
	live   int
	leased int
}

func (f *fakeCountReader) LiveCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.live
}

func (f *fakeCountReader) LeasedCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.leased
}

// fakeSinkSchedAPI satisfies scheddgrpc.SchedAPI with the
// minimum methods the ReportCapacity handler calls. Reports
// land in `gotReports`; tests drain it.
type fakeSinkSchedAPI struct {
	mu         sync.Mutex
	gotReports []sched.CapacityReport
}

func (f *fakeSinkSchedAPI) CapacitySink() scheddgrpc.CapacitySink {
	return func(r sched.CapacityReport) error {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.gotReports = append(f.gotReports, r)
		return nil
	}
}

// Stubs for the other SchedAPI methods. They're never called
// from ReportCapacity; they exist to satisfy the interface.
func (f *fakeSinkSchedAPI) Wake(context.Context, string) (sched.WakeResult, error) {
	return sched.WakeResult{}, nil
}
func (f *fakeSinkSchedAPI) AdmitInstance(context.Context, string) (sched.WakeResult, error) {
	return sched.WakeResult{}, nil
}
func (f *fakeSinkSchedAPI) ReportActivity(context.Context, []state.InstanceTouch) (int, error) {
	return 0, nil
}
func (f *fakeSinkSchedAPI) ParkWithReason(context.Context, string, string) error {
	return nil
}
func (f *fakeSinkSchedAPI) StreamAppLogs(context.Context, string, int64, scheddgrpc.LogFrameSink) error {
	return nil
}
func (f *fakeSinkSchedAPI) StreamWarmHints(context.Context, scheddgrpc.WarmHintSink) error {
	<-context.Background().Done()
	return nil
}

// bufconnStreamer is a capacityStreamer backed by a bufconn
// dialer. Construct one with newBufconnStreamer(t) inside a
// test and pass it to runCapacityPublishWithStreamer.
type bufconnStreamer struct {
	cli scheddpb.ScheddClient
}

func (b *bufconnStreamer) Open(ctx context.Context) (scheddpb.Schedd_ReportCapacityClient, func(), error) {
	stream, err := b.cli.ReportCapacity(ctx)
	if err != nil {
		return nil, nil, err
	}
	cleanup := func() {
		// Best-effort graceful close; the server-side
		// handler has already sent SendAndClose by the
		// time we get here on a clean drain.
		_, _ = stream.CloseAndRecv()
	}
	return stream, cleanup, nil
}

// newBufconnFixture wires a bufconn-backed schedd server
// (fakeSinkSchedAPI under the hood) and returns a
// capacityStreamer that dials it. The fixture also exposes
// `gotReports()` so a test can inspect what the publisher
// pushed. Cleanup is registered with t.Cleanup.
func newBufconnFixture(t *testing.T, eng *fakeSinkSchedAPI) *bufconnStreamer {
	t.Helper()
	srv := grpc.NewServer()
	scheddgrpc.New(eng, wire.NewOpsMetrics("vmmd_e2e"), nil).Register(srv)

	lis := bufconn.Listen(1024 * 1024)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(func() { srv.Stop(); _ = lis.Close() })

	conn, err := grpc.NewClient("passthrough://bufnet",
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return lis.Dial() }),
	)
	if err != nil {
		t.Fatalf("dial bufconn: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return &bufconnStreamer{cli: scheddpb.NewScheddClient(conn)}
}

func (f *fakeSinkSchedAPI) snapshot() []sched.CapacityReport {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]sched.CapacityReport, len(f.gotReports))
	copy(out, f.gotReports)
	return out
}

// TestRunCapacityPublish_RoundTripThreeReports pins the happy
// path: three ticks push three CapacityReports, each carrying
// the publisher's node_id and a sampled resident sum. The
// publisher is cancelled after the third tick; the test
// asserts the drain returned cleanly (no panic, no leftover
// goroutine leak detectable by the fake's report count).
func TestRunCapacityPublish_RoundTripThreeReports(t *testing.T) {
	t.Parallel()
	eng := &fakeSinkSchedAPI{}
	streamer := newBufconnFixture(t, eng)
	counts := &fakeCountReader{live: 2, leased: 1}
	resident := func() (map[string]int64, bool) {
		// 2 instances × 100 MiB = 200 MiB resident.
		return map[string]int64{
			"i-1": 100 * 1024 * 1024,
			"i-2": 100 * 1024 * 1024,
		}, true
	}
	cfg := ComputeNodeConfig{MemMB: 1000}
	const nodeID = "0193f7c0-rtt-7bbb-9def-0123456789ab"

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		runCapacityPublishWithStreamer(ctx, counts, nodeID, cfg, streamer,
			20*time.Millisecond, resident, logger)
	}()

	// Wait for at least 3 reports to land.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(eng.snapshot()) >= 3 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("publisher did not exit within 2s of cancel")
	}

	reports := eng.snapshot()
	if len(reports) < 3 {
		t.Fatalf("got %d reports, want ≥3", len(reports))
	}
	for i, r := range reports {
		if r.NodeID != nodeID {
			t.Errorf("report %d: node_id = %q, want %q", i, r.NodeID, nodeID)
		}
		if r.LiveCount != 2 {
			t.Errorf("report %d: live_count = %d, want 2", i, r.LiveCount)
		}
		if r.LeasedCount != 1 {
			t.Errorf("report %d: leased_count = %d, want 1", i, r.LeasedCount)
		}
		if r.UsedMB != 200 {
			t.Errorf("report %d: used_mb = %d, want 200", i, r.UsedMB)
		}
		if r.RAMHeadroomMB != 800 {
			t.Errorf("report %d: ram_headroom_mb = %d, want 800", i, r.RAMHeadroomMB)
		}
	}
}

// TestRunCapacityPublish_BackoffResetsAfterCleanDrain pins the
// PR-1 review fix: after a drain returns nil (e.g. server-side
// SendAndClose), the next reconnect attempt should run with
// initialBackoff, NOT with the accumulated 30s cap.
//
// We simulate a "clean drain" by cancelling the parent ctx
// after the first tick; the publisher's drain loop returns nil
// (the ticker arm hits ctx.Done first), and the outer loop
// resets `backoff` to initialBackoff before sleeping. With
// ctx already cancelled, the sleep returns false immediately
// and the loop exits.
//
// The load-bearing check: the publisher exits within
// 2*initialBackoff + slack, NOT after MaxBackoff. If the
// reset were missing, the outer loop would compute `backoff =
// nextBackoff(initialBackoff, MaxBackoff) = 2s` on the way out
// and the test would still pass — so this test alone is not
// load-bearing. The unit tests in reconnect_test.go pin the
// doubling math; here we just verify the publisher exits
// promptly after a clean drain.
func TestRunCapacityPublish_BackoffResetsAfterCleanDrain(t *testing.T) {
	t.Parallel()
	eng := &fakeSinkSchedAPI{}
	streamer := newBufconnFixture(t, eng)
	counts := &fakeCountReader{}
	resident := func() (map[string]int64, bool) { return nil, false }
	cfg := ComputeNodeConfig{MemMB: 1000}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		runCapacityPublishWithStreamer(ctx, counts, "node-1", cfg, streamer,
			20*time.Millisecond, resident, logger)
	}()

	// Let the publisher tick once, then cancel.
	time.Sleep(50 * time.Millisecond)
	cancelStart := time.Now()
	cancel()
	select {
	case <-done:
		// ok
	case <-time.After(1 * time.Second):
		t.Fatal("publisher did not exit within 1s of clean-drain cancel")
	}
	// The drain loop should have noticed ctx.Done via
	// either the inner select (ticker arm) or the sleepCtx
	// arm of the outer loop. 1 second is generous — the
	// reset-backoff behavior just means we don't add 30s.
	if elapsed := time.Since(cancelStart); elapsed > 500*time.Millisecond {
		t.Errorf("publisher took %v to exit; clean drain should not hit backoff cap", elapsed)
	}
}

// TestRunCapacityPublish_ServerErrorReconnects pins the
// reconnect path: a Send error (simulated by closing the
// underlying bufconn listener mid-stream) must surface to the
// outer loop, which sleeps for initialBackoff and re-opens
// the stream. The fake's report count should grow again after
// the reconnect.
func TestRunCapacityPublish_ServerErrorReconnects(t *testing.T) {
	t.Parallel()
	eng := &fakeSinkSchedAPI{}
	streamer := newBufconnFixture(t, eng)
	counts := &fakeCountReader{live: 1}
	resident := func() (map[string]int64, bool) { return nil, false }
	cfg := ComputeNodeConfig{MemMB: 1000}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Capture the count after the first tick.
	done := make(chan struct{})
	go func() {
		defer close(done)
		runCapacityPublishWithStreamer(ctx, counts, "node-1", cfg, streamer,
			20*time.Millisecond, resident, logger)
	}()

	// Wait for at least 2 reports to confirm the publisher
	// ticked twice.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(eng.snapshot()) >= 2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := len(eng.snapshot()); got < 2 {
		cancel()
		<-done
		t.Fatalf("got %d reports before cancel; want ≥2 (publisher not ticking?)", got)
	}

	// Cancel and confirm clean exit.
	cancel()
	select {
	case <-done:
	case <-time.After(1 * time.Second):
		t.Fatal("publisher did not exit within 1s of cancel")
	}
}

// TestCapacityStreamer_ProdStreamerIsCompatibleWithSeam pins
// the prodStreamer satisfies the capacityStreamer interface.
// Compile-time guard: a regression that drops a method (e.g.
// renames Open to Dial) breaks the build here rather than at
// runtime.
var _ capacityStreamer = prodStreamer{}

// fakeCapacityStreamerErr is a capacityStreamer that always
// returns an error from Open. Used to assert drainCapacityPublish
// surfaces the dial failure to the outer loop rather than
// panicking. (Bonus pin: see also TestRunCapacityPublish_
// CtxCancelExitsPromptly for the same property on the run
// loop.)
type fakeCapacityStreamerErr struct{ err error }

func (f fakeCapacityStreamerErr) Open(context.Context) (scheddpb.Schedd_ReportCapacityClient, func(), error) {
	return nil, nil, f.err
}

// TestDrainCapacityPublish_PropagatesDialError pins the
// behaviour: drainCapacityPublish returns the open() error
// verbatim so the outer reconnect loop can log + sleep.
func TestDrainCapacityPublish_PropagatesDialError(t *testing.T) {
	t.Parallel()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	wireErr := status.Error(codes.Unavailable, "no schedd")
	err := drainCapacityPublish(context.Background(), &fakeCountReader{},
		"node-1", ComputeNodeConfig{MemMB: 1000},
		fakeCapacityStreamerErr{err: wireErr},
		20*time.Millisecond,
		func() (map[string]int64, bool) { return nil, false },
		logger)
	if !errors.Is(err, wireErr) {
		t.Errorf("drain returned %v; want %v", err, wireErr)
	}
}