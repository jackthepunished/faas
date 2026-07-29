// AdvisoryClient unit tests — Wave 0 PR-C / ADR-047.
//
// Strategy: stand up a real gRPC server backed by a stub AdvisoryServer
// implementation, listen on a per-test unix socket in t.TempDir(), and
// point AdvisoryClient at it. wire.DialContext("unix:///tmp/...sock")
// resolves to the standard grpc.NewClient(unix-scheme) path which the
// bufconn harness can't intercept (no WithContextDialer seam in
// AdvisoryClient), so a real socket is the cheapest honest end-to-end.

package vmmdgrpc_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"sync"
	"testing"
	"time"

	apidpb "github.com/onebox-faas/faas/api/proto/onebox/faas/apid/v1"
	"github.com/onebox-faas/faas/pkg/fcvm"
	"github.com/onebox-faas/faas/pkg/vmmdgrpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// stubAdvisoryServer is the test-side apid AdvisoryServer. It records
// every call and can be wired to return codes.Unavailable N times to
// drive the retry path.
type stubAdvisoryServer struct {
	apidpb.UnimplementedAdvisoryServer
	mu         sync.Mutex
	calls      []stubAdvisoryCall
	failNTimes int32 // first N calls return Unavailable, then succeed
	failAll    bool  // all calls return Unavailable
}

type stubAdvisoryCall struct {
	Instance string
	AppID    string
	Events   []*apidpb.AdvisoryEvent
}

func (s *stubAdvisoryServer) ForwardStatelessAdvisory(_ context.Context, req *apidpb.ForwardStatelessAdvisoryRequest) (*apidpb.ForwardStatelessAdvisoryResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, stubAdvisoryCall{
		Instance: req.GetInstance(),
		AppID:    req.GetAppId(),
		Events:   req.GetEvents(),
	})
	if s.failAll {
		return nil, status.Error(codes.Unavailable, "stub: apid down")
	}
	if int32(len(s.calls)) <= s.failNTimes {
		return nil, status.Error(codes.Unavailable, "stub: transient down")
	}
	return &apidpb.ForwardStatelessAdvisoryResponse{}, nil
}

func (s *stubAdvisoryServer) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.calls)
}

// alwaysRejectAdvisory is the focused server for the InvalidArgument path.
type alwaysRejectAdvisory struct {
	apidpb.UnimplementedAdvisoryServer
	code codes.Code
}

func (s *alwaysRejectAdvisory) ForwardStatelessAdvisory(_ context.Context, _ *apidpb.ForwardStatelessAdvisoryRequest) (*apidpb.ForwardStatelessAdvisoryResponse, error) {
	return nil, status.Error(s.code, "stub: always reject")
}

// startStubAdvisoryServer binds a per-test unix socket and registers the
// stub on a fresh grpc.Server. Returns the socket path and a cleanup
// registered with t.Cleanup.
//
// Uses a hand-rolled short path under /tmp (not t.TempDir()) because
// the default Go test tmpdir on macOS is so deep (~80 chars) that
// the resulting socket path exceeds the kernel's 104-byte AF_UNIX
// sun_path limit and the listen() call fails with EINVAL.
func startStubAdvisoryServer(t *testing.T, stub apidpb.AdvisoryServer) string {
	t.Helper()
	sock := fmt.Sprintf("/tmp/faas-test-advisory-%d-%s.sock", os.Getpid(), t.Name())
	lis, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen %s: %v", sock, err)
	}
	srv := grpc.NewServer()
	apidpb.RegisterAdvisoryServer(srv, stub)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(func() {
		srv.Stop()
		_ = lis.Close()
		_ = os.Remove(sock)
	})
	return sock
}

// silentLog discards every log line so the test output stays clean.
// The AdvisoryClient path emits a Warn log on every retry, which is
// correct production behavior; we don't need it in unit tests.
func silentLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// TestAdvisoryForward_DialsApidSocket exercises the happy path: the
// client dials a real unix-socket apid, sends one batch, the stub
// receives it verbatim.
func TestAdvisoryForward_DialsApidSocket(t *testing.T) {
	stub := &stubAdvisoryServer{}
	sock := startStubAdvisoryServer(t, stub)

	cli := vmmdgrpc.NewAdvisoryClient("unix://"+sock, silentLog())
	defer cli.Close()

	batch := []fcvm.AdvisoryEvent{
		{Path: "/data/foo", Masks: []string{"create"}, PID: 42, TsUnix: 1700000000000},
		{Path: "/data/bar", Masks: []string{"modify", "move"}, PID: 42, TsUnix: 1700000001000},
	}
	if err := cli.Forward(context.Background(), "i-1", "a-1", batch); err != nil {
		t.Fatalf("Forward: %v", err)
	}
	if got := stub.callCount(); got != 1 {
		t.Fatalf("stub call count = %d, want 1", got)
	}
	last := stub.calls[0]
	if last.Instance != "i-1" || last.AppID != "a-1" {
		t.Errorf("instance/app = %q/%q, want i-1/a-1", last.Instance, last.AppID)
	}
	if got := len(last.Events); got != 2 {
		t.Fatalf("events = %d, want 2", got)
	}
	if last.Events[0].GetPath() != "/data/foo" {
		t.Errorf("events[0].path = %q, want /data/foo", last.Events[0].GetPath())
	}
	if last.Events[1].GetTsUnixMs() != 1700000001000 {
		t.Errorf("events[1].ts_unix_ms = %d, want 1700000001000", last.Events[1].GetTsUnixMs())
	}
}

// TestAdvisoryForward_RetriesOnApidUnavailable pins the ADR-035 retry
// contract: codes.Unavailable triggers exactly one retry; a second
// Unavailable gives up (Warn log + drop, no error returned).
func TestAdvisoryForward_RetriesOnApidUnavailable(t *testing.T) {
	stub := &stubAdvisoryServer{failNTimes: 5} // every call fails Unavailable
	sock := startStubAdvisoryServer(t, stub)

	cli := vmmdgrpc.NewAdvisoryClient("unix://"+sock, silentLog())
	defer cli.Close()

	batch := []fcvm.AdvisoryEvent{{Path: "/data/x", Masks: []string{"create"}}}
	start := time.Now()
	if err := cli.Forward(context.Background(), "i", "a", batch); err != nil {
		t.Fatalf("Forward: %v (ADR-035: drop on Unavailable, never bubble)", err)
	}
	elapsed := time.Since(start)
	// Two attempts × 200ms dial + 200ms retry delay ≈ 800ms ceiling.
	// Generous bound: 1.5s. Tighter bound catches a regression that
	// loops the retry path.
	if elapsed > 1500*time.Millisecond {
		t.Errorf("retry elapsed = %v, want <1.5s (two attempts + retry delay)", elapsed)
	}
	// Exactly two calls: one initial, one retry. A regression that
	// drops the retry would record 1; one that loops would record 3+.
	if got := stub.callCount(); got != 2 {
		t.Errorf("stub call count = %d, want 2 (initial + one retry)", got)
	}
}

// TestAdvisoryForward_NonUnavailableErrorSwallowed pins the
// "apid rejects but is up" path: codes.InvalidArgument is NOT
// retried (only Unavailable triggers the retry), the call still
// returns nil, and the stub sees exactly one attempt.
//
// We use elapsed-time as the indirect signal: two attempts × 200ms
// would exceed 300ms; one attempt finishes in <100ms.
func TestAdvisoryForward_NonUnavailableErrorSwallowed(t *testing.T) {
	stub := &alwaysRejectAdvisory{code: codes.InvalidArgument}
	sock := startStubAdvisoryServer(t, stub)

	cli := vmmdgrpc.NewAdvisoryClient("unix://"+sock, silentLog())
	defer cli.Close()

	batch := []fcvm.AdvisoryEvent{{Path: "/data/x", Masks: []string{"create"}}}
	start := time.Now()
	if err := cli.Forward(context.Background(), "i", "a", batch); err != nil {
		t.Fatalf("Forward: %v (ADR-035: drop non-Unavailable rejections, never bubble)", err)
	}
	elapsed := time.Since(start)
	if elapsed > 300*time.Millisecond {
		t.Errorf("elapsed = %v, want <300ms (InvalidArgument must NOT retry)", elapsed)
	}
}

// TestAdvisoryForward_EmptyBatchIsNoop asserts the manager-side guard
// mirrored at the client: empty events slice does not dial, does not
// log, returns nil. The stub on the socket is never reached, so its
// callCount must be 0.
func TestAdvisoryForward_EmptyBatchIsNoop(t *testing.T) {
	stub := &stubAdvisoryServer{}
	sock := startStubAdvisoryServer(t, stub)

	cli := vmmdgrpc.NewAdvisoryClient("unix://"+sock, silentLog())
	defer cli.Close()

	if err := cli.Forward(context.Background(), "i", "a", nil); err != nil {
		t.Fatalf("Forward nil batch: %v", err)
	}
	if err := cli.Forward(context.Background(), "i", "a", []fcvm.AdvisoryEvent{}); err != nil {
		t.Fatalf("Forward empty batch: %v", err)
	}
	if got := stub.callCount(); got != 0 {
		t.Errorf("stub call count = %d, want 0 (empty batches must not dial)", got)
	}
}

// TestAdvisoryForward_EmptyInputsAreNoop covers the input-validation
// guards at the top of Forward: missing instance or appID must short-
// circuit before the dial. ADR-035 covers nil; this test pins the
// empty-string leg.
func TestAdvisoryForward_EmptyInputsAreNoop(t *testing.T) {
	stub := &stubAdvisoryServer{}
	sock := startStubAdvisoryServer(t, stub)

	cli := vmmdgrpc.NewAdvisoryClient("unix://"+sock, silentLog())
	defer cli.Close()

	if err := cli.Forward(context.Background(), "", "a", []fcvm.AdvisoryEvent{{Path: "/x"}}); err != nil {
		t.Fatalf("Forward empty instance: %v", err)
	}
	if err := cli.Forward(context.Background(), "i", "", []fcvm.AdvisoryEvent{{Path: "/x"}}); err != nil {
		t.Fatalf("Forward empty appID: %v", err)
	}
	if got := stub.callCount(); got != 0 {
		t.Errorf("stub call count = %d, want 0 (empty inputs must not dial)", got)
	}
}

// TestAdvisoryForward_NilClientNoop mirrors the default-local posture:
// when vmmd never wired an AdvisoryClient, the Manager's forward call
// must succeed without crashing. We pass a nil *AdvisoryClient directly
// to Forward and assert nil.
func TestAdvisoryForward_NilClientNoop(t *testing.T) {
	var cli *vmmdgrpc.AdvisoryClient // nil receiver
	if err := cli.Forward(context.Background(), "i", "a", []fcvm.AdvisoryEvent{{Path: "/x"}}); err != nil {
		t.Errorf("nil Forward: %v (default-local must no-op)", err)
	}
	if err := cli.Close(); err != nil {
		t.Errorf("nil Close: %v", err)
	}
}

// TestNewAdvisoryClient_NilLogDefaults pins the NewAdvisoryClient
// convenience: a nil log must default to slog.Default(), not panic.
// We can't easily assert which logger is held, so we exercise the
// no-op path (empty batch) — a nil deref inside Forward would panic.
func TestNewAdvisoryClient_NilLogDefaults(t *testing.T) {
	cli := vmmdgrpc.NewAdvisoryClient("unix:///nonexistent/apid.sock", nil)
	defer cli.Close()
	// Empty batch never reaches dial — safe even if no socket exists.
	if err := cli.Forward(context.Background(), "i", "a", nil); err != nil {
		t.Errorf("Forward with nil log: %v", err)
	}
}
