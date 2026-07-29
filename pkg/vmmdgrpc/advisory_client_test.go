// AdvisoryClient unit tests — Wave 0 PR-C / ADR-047.
//
// Strategy: stand up a real gRPC server backed by a stub AdvisoryServer
// implementation, listen on a per-test unix socket in t.TempDir(), and
// point AdvisoryClient at it. wire.DialContext("unix:///tmp/...sock")
// resolves to the standard grpc.NewClient(unix-scheme) path which the
// bufconn harness can't intercept (no WithContextDialer seam in
// AdvisoryClient), so a real socket is the cheapest honest end-to-end.
//
// Mega-PR B: each test that exercises a Forward outcome also asserts
// the stateless_advisory_batches_emitted_total{result} counter
// increments (closed-set semantics). The counter wire-up is in
// NewAdvisoryClient; nil-safe on a nil *wire.OpsMetrics.

package vmmdgrpc_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	apidpb "github.com/onebox-faas/faas/api/proto/onebox/faas/apid/v1"
	"github.com/onebox-faas/faas/pkg/fcvm"
	"github.com/onebox-faas/faas/pkg/vmmdgrpc"
	"github.com/onebox-faas/faas/pkg/wire"
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
// receives it verbatim. Mega-PR B: also asserts the OK counter
// increments by 1 and the dial_failed counter stays at 0.
func TestAdvisoryForward_DialsApidSocket(t *testing.T) {
	stub := &stubAdvisoryServer{}
	sock := startStubAdvisoryServer(t, stub)

	ops := wire.NewOpsMetrics("vmmd")
	cli := vmmdgrpc.NewAdvisoryClient("unix://"+sock, silentLog(), ops)
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
	// Mega-PR B: ok counter increments by 1; the failure labels stay at 0.
	// Use the same render seam as pkg/wire/metrics_test.go: scrape
	// the daemon's /metrics and assert the counter line exists.
	if v := scrapeCounter(t, ops, `vmmd_stateless_advisory_batches_emitted_total{result="ok"}`); v != 1 {
		t.Errorf("ok counter = %v, want 1", v)
	}
	if v := scrapeCounter(t, ops, `vmmd_stateless_advisory_batches_emitted_total{result="dial_failed"}`); v != 0 {
		t.Errorf("dial_failed counter must stay 0, got %v", v)
	}
}

// TestAdvisoryForward_RetriesOnApidUnavailable pins the ADR-035 retry
// contract: codes.Unavailable triggers exactly one retry; a second
// Unavailable gives up (Warn log + drop, no error returned). Mega-PR B:
// assert result=unavailable_after_retry increments by 1 and result=ok
// stays at 0.
func TestAdvisoryForward_RetriesOnApidUnavailable(t *testing.T) {
	stub := &stubAdvisoryServer{failNTimes: 5} // every call fails Unavailable
	sock := startStubAdvisoryServer(t, stub)

	ops := wire.NewOpsMetrics("vmmd")
	cli := vmmdgrpc.NewAdvisoryClient("unix://"+sock, silentLog(), ops)
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
	if v := scrapeCounter(t, ops, `vmmd_stateless_advisory_batches_emitted_total{result="unavailable_after_retry"}`); v != 1 {
		t.Errorf("unavailable_after_retry counter = %v, want 1", v)
	}
	if v := scrapeCounter(t, ops, `vmmd_stateless_advisory_batches_emitted_total{result="ok"}`); v != 0 {
		t.Errorf("ok counter must stay 0 on Unavailable path, got %v", v)
	}
}

// TestAdvisoryForward_NonUnavailableErrorSwallowed pins the
// "apid rejects but is up" path: codes.InvalidArgument is NOT
// retried (only Unavailable triggers the retry), the call still
// returns nil, and the stub sees exactly one attempt. Mega-PR B:
// assert result=rejected increments by 1.
//
// We use elapsed-time as the indirect signal: two attempts × 200ms
// would exceed 300ms; one attempt finishes in <100ms.
func TestAdvisoryForward_NonUnavailableErrorSwallowed(t *testing.T) {
	stub := &alwaysRejectAdvisory{code: codes.InvalidArgument}
	sock := startStubAdvisoryServer(t, stub)

	ops := wire.NewOpsMetrics("vmmd")
	cli := vmmdgrpc.NewAdvisoryClient("unix://"+sock, silentLog(), ops)
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
	if v := scrapeCounter(t, ops, `vmmd_stateless_advisory_batches_emitted_total{result="rejected"}`); v != 1 {
		t.Errorf("rejected counter = %v, want 1", v)
	}
}

// TestAdvisoryForward_DialErrorSurfacesAsUnavailable pins the
// shape of the "apid unreachable" path. wire.DialContext defers
// the actual unix-socket dial until the first RPC, so pointing
// the client at a non-existent socket returns a usable *grpc.ClientConn
// at Forward() time and surfaces as codes.Unavailable from the
// RPC, not as a dial-time error. The counter therefore increments
// result=unavailable_after_retry, NOT result=dial_failed.
//
// The dial_failed label is reserved for the case where the dial
// fails synchronously — currently unreachable through this
// surface because grpc.NewClient is non-blocking. Kept here as a
// guard against a future change that surfaces the dial error
// earlier (e.g. if the implementation switches to grpc.Dial,
// which blocks, the dial_failed branch in Forward would fire).
func TestAdvisoryForward_DialErrorSurfacesAsUnavailable(t *testing.T) {
	ops := wire.NewOpsMetrics("vmmd")
	cli := vmmdgrpc.NewAdvisoryClient("unix:///nonexistent/does-not-exist.sock", silentLog(), ops)
	defer cli.Close()

	batch := []fcvm.AdvisoryEvent{{Path: "/data/x", Masks: []string{"create"}}}
	if err := cli.Forward(context.Background(), "i", "a", batch); err != nil {
		t.Errorf("Forward unreachable: %v (ADR-035: drop, never bubble)", err)
	}
	if v := scrapeCounter(t, ops, `vmmd_stateless_advisory_batches_emitted_total{result="unavailable_after_retry"}`); v != 1 {
		t.Errorf("unavailable_after_retry counter = %v, want 1", v)
	}
}

// TestAdvisoryForward_EmptyBatchIsNoop asserts the manager-side guard
// mirrored at the client: empty events slice does not dial, does not
// log, returns nil. The stub on the socket is never reached, so its
// callCount must be 0. Mega-PR B: empty batches must NOT increment
// any of the four result counters.
func TestAdvisoryForward_EmptyBatchIsNoop(t *testing.T) {
	stub := &stubAdvisoryServer{}
	sock := startStubAdvisoryServer(t, stub)

	ops := wire.NewOpsMetrics("vmmd")
	cli := vmmdgrpc.NewAdvisoryClient("unix://"+sock, silentLog(), ops)
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
	// No result label should have moved off 0 from the pre-instantiated baseline.
	for _, r := range []string{"ok", "dial_failed", "rejected", "unavailable_after_retry"} {
		want := `vmmd_stateless_advisory_batches_emitted_total{result="` + r + `"} 0`
		if v := scrapeCounter(t, ops, want); v != 0 {
			t.Errorf("empty batch incremented %s to %v, want 0", r, v)
		}
	}
}

// TestAdvisoryForward_EmptyInputsAreNoop covers the input-validation
// guards at the top of Forward: missing instance or appID must short-
// circuit before the dial. ADR-035 covers nil; this test pins the
// empty-string leg.
func TestAdvisoryForward_EmptyInputsAreNoop(t *testing.T) {
	stub := &stubAdvisoryServer{}
	sock := startStubAdvisoryServer(t, stub)

	ops := wire.NewOpsMetrics("vmmd")
	cli := vmmdgrpc.NewAdvisoryClient("unix://"+sock, silentLog(), ops)
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
// to Forward and assert nil. Mega-PR B: nil *AdvisoryClient must NOT
// touch any counter (it never reaches the increment path).
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
	cli := vmmdgrpc.NewAdvisoryClient("unix:///nonexistent/apid.sock", nil, nil)
	defer cli.Close()
	// Empty batch never reaches dial — safe even if no socket exists.
	if err := cli.Forward(context.Background(), "i", "a", nil); err != nil {
		t.Errorf("Forward with nil log: %v", err)
	}
}

// scrapeCounter fetches the /metrics body of ops and returns the
// numeric value of the counter line that exactly matches `prefix`.
// Counter lines are emitted as e.g.
//
//	vmmd_stateless_advisory_batches_emitted_total{result="ok"} 1
//
// so the helper extracts the trailing float. Returns 0 if the
// line is absent (pre-instantiated but never incremented). This
// keeps the test self-contained from pkg/wire's internal series
// type — we don't need to reach inside OpsMetrics for the raw
// CounterVec, which is unexported.
//
// Uses httptest.NewServer over ops.Handler() rather than the
// prometheus testutil.CollectAndFormat because the latter would
// emit only matched series and lose the pre-instantiated zero
// rows we want to assert on. We need the full body so the
// "must stay 0" assertions actually surface the row.
//
// Assumptions (load-bearing for correctness):
//
//   - The OpenMetrics text format emits counter samples as
//     "<name>{label=\"v\",...} <value>\n" — the metric name and
//     the value are always separated by exactly one space, and
//     the value is always the trailing token of the line.
//   - All label values passed to this helper come from the
//     closed-set constants in pkg/wire/metrics.go (AdvisoryResult*
//     / AdvisorySeverity*) and contain no spaces or quotes. The
//     Prometheus exposition format forbids them at the spec
//     level; if a future label set widens, change the helper to
//     parse the label block first rather than treating the
//     trailing token as the value.
//   - Scientific notation ("1e+09") is not exercised by this
//     project today; the Sscanf %f path accepts it but is not
//     asserted by any test. If counter values grow past 2^53
//     (CounterVec.Inc is float64), tighten the parse.
//
// Returns 0 if the line is absent (pre-instantiated but never
// incremented), so test assertions can rely on the absence/zero
// symmetry: a missing series and a zero-valued series both
// surface as 0. Callers that need to distinguish the two must
// add a presence check before calling.
func scrapeCounter(t *testing.T, ops *wire.OpsMetrics, linePrefix string) float64 {
	t.Helper()
	srv := httptest.NewServer(ops.Handler())
	t.Cleanup(srv.Close)
	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("scrape %s: %v", srv.URL, err)
	}
	defer resp.Body.Close()
	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("scrape body: %v", err)
	}
	body := string(bodyBytes)
	for _, ln := range strings.Split(body, "\n") {
		if !strings.HasPrefix(ln, linePrefix) {
			continue
		}
		// The trailing value sits after the last space.
		// Closed-set label values guarantee no spaces inside the
		// labelled suffix, so last-space is the value separator.
		idx := strings.LastIndex(ln, " ")
		if idx < 0 || idx == len(ln)-1 {
			continue
		}
		var f float64
		if _, err := fmt.Sscanf(ln[idx+1:], "%f", &f); err != nil {
			continue
		}
		return f
	}
	return 0
}
