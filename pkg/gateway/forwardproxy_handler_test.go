// Tests for the gatewayd Handler × ForwardingReverseProxy integration
// (issue #98 / ADR-028 / ADR-047). The unit tests in forwardproxy_test.go
// pin the forwarder in isolation; this file pins the seam — when the
// Handler has proxyByNode installed, every request dispatches
// through it.
//
// PR-D / ADR-047: the legacy unary ForwardHTTP RPC was removed. The
// stubVmmdClient now drives the bidi ForwardHTTPStream RPC via the
// in-package proxy_stub.go fake. The streaming-only shape keeps
// the integration test on one consistent bridge.
//
// What this test exercises:
//   1. proxyByNode != nil + Backend.Target returns a node id → the
//      forwarder is called with that id and the response body is
//      written back to the inbound ResponseWriter.
//   2. proxyByNode nil (legacy path) → proxyFor is called with
//      whatever Target returned (verifies the e2e harness keeps
//      working without the overlay path wired).
//   3. WithForwarding is a fluent setter that doesn't panic on
//      successive calls (idempotent re-install during reload).
//
// Lives in package gateway (not gateway_test) because the seam
// touches unexported fields on Handler (proxyByNode, proxyFor).
// The forwardproxy_test.go fakes are kept in package gateway_test
// on purpose; we re-declare a minimal in-package stub here so the
// integration test compiles standalone.

package gateway

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	vmmdpb "github.com/onebox-faas/faas/api/proto/onebox/faas/vmmd/v1"
	"github.com/onebox-faas/faas/pkg/api"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// stubVmmdClient is the in-package fake for the handler-side seam
// test. forwardproxy_test.go's fakeVmmdClient lives in
// package gateway_test and isn't reachable from here. This stub
// satisfies the full vmmdpb.VmmdClient interface (ForwardHTTPStream
// + the other RPCs the cache might exercise on shutdown) so the
// NodeClientLookup can hand it back without an interface-conversion
// error.
//
// PR-D / ADR-047: the streaming RPC is the only bridge today. The
// stub dispatches ForwardHTTPStream through a bufconn-based fake
// server (proxy_stub.go) that records the request init and writes
// a fixed response body — the integration test asserts the body
// reaches the inbound ResponseWriter and the init was sent.
type stubVmmdClient struct {
	mu    sync.Mutex
	calls []*vmmdpb.ForwardHTTPRequestInit
	resp  *vmmdpb.ForwardHTTPResponseInit
	body  []byte
}

func (s *stubVmmdClient) ForwardHTTPStream(ctx context.Context, _ ...grpc.CallOption) (grpc.BidiStreamingClient[vmmdpb.ForwardHTTPStreamRequest, vmmdpb.ForwardHTTPStreamResponse], error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	stream := newProxyStubStream(ctx, &s.calls, s.resp, s.body)
	return stream, nil
}

func (s *stubVmmdClient) CreateFromSnapshot(context.Context, *vmmdpb.CreateFromSnapshotRequest, ...grpc.CallOption) (*vmmdpb.WakeResponse, error) {
	panic("CreateFromSnapshot: not stubbed in handler integration test")
}
func (s *stubVmmdClient) CreateColdBoot(context.Context, *vmmdpb.CreateColdBootRequest, ...grpc.CallOption) (*vmmdpb.WakeResponse, error) {
	panic("CreateColdBoot: not stubbed in handler integration test")
}
func (s *stubVmmdClient) PauseAndSnapshot(context.Context, *vmmdpb.PauseAndSnapshotRequest, ...grpc.CallOption) (*vmmdpb.SnapshotResponse, error) {
	panic("PauseAndSnapshot: not stubbed in handler integration test")
}
func (s *stubVmmdClient) Destroy(context.Context, *vmmdpb.DestroyRequest, ...grpc.CallOption) (*vmmdpb.DestroyResponse, error) {
	return &vmmdpb.DestroyResponse{}, nil
}
func (s *stubVmmdClient) Stats(context.Context, *vmmdpb.StatsRequest, ...grpc.CallOption) (*vmmdpb.StatsResponse, error) {
	return &vmmdpb.StatsResponse{}, nil
}
func (s *stubVmmdClient) Heartbeat(context.Context, *vmmdpb.HeartbeatRequest, ...grpc.CallOption) (*vmmdpb.HeartbeatResponse, error) {
	return &vmmdpb.HeartbeatResponse{}, nil
}
func (s *stubVmmdClient) Ping(context.Context, *vmmdpb.PingRequest, ...grpc.CallOption) (*vmmdpb.PingResponse, error) {
	return &vmmdpb.PingResponse{}, nil
}

// UpdateEgressAllowlist (tier-2 PR-B) — the gateway hot path
// doesn't drive the in-place patch; schedd's egress_drift
// subscriber does. Returns success so the gRPC VmmdClient
// interface stays satisfied.
func (s *stubVmmdClient) UpdateEgressAllowlist(context.Context, *vmmdpb.UpdateEgressAllowlistRequest, ...grpc.CallOption) (*vmmdpb.UpdateEgressAllowlistAck, error) {
	return &vmmdpb.UpdateEgressAllowlistAck{}, nil
}

// Logs (issue #254 / Move 4) — the gateway hot path never dials
// the per-instance log stream directly; apid dials schedd for
// that. The stub returns Unimplemented so any accidental test
// that touches the codepath fails fast with a stable gRPC code.
func (s *stubVmmdClient) Logs(context.Context, *vmmdpb.LogsRequest, ...grpc.CallOption) (grpc.ServerStreamingClient[vmmdpb.LogsResponse], error) {
	return nil, status.Error(codes.Unimplemented, "gateway stub does not stream logs")
}

// SeccompStatus (M8 §11) — the gateway hot path doesn't poll
// seccomp state; cmd/e2e/sec11_seccomp_e2e_test.go drives the
// dial directly. Returns a "not implemented" envelope so the
// gRPC VmmdClient interface stays satisfied; this path is never
// expected to fire in handler tests.
func (s *stubVmmdClient) SeccompStatus(context.Context, *vmmdpb.SeccompStatusRequest, ...grpc.CallOption) (*vmmdpb.SeccompStatusResponse, error) {
	return &vmmdpb.SeccompStatusResponse{}, nil
}

// MountParentExt4ReadOnly (ADR-053) — gateway never drives the
// parent-mount staging path; imaged owns that RPC. Returns
// empty + nil so the vmmdpb.VmmdClient interface is satisfied.
// Any accidental caller would surface as imaged's
// "empty mountpoint" check rather than a NotFound from vmmd.
func (s *stubVmmdClient) MountParentExt4ReadOnly(context.Context, *vmmdpb.MountParentExt4ReadOnlyRequest, ...grpc.CallOption) (*vmmdpb.MountParentExt4ReadOnlyResponse, error) {
	return &vmmdpb.MountParentExt4ReadOnlyResponse{}, nil
}

// UmountParentExt4 (ADR-053) — gateway never drives the parent
// umount path. Returns nil.
func (s *stubVmmdClient) UmountParentExt4(context.Context, *vmmdpb.UmountParentExt4Request, ...grpc.CallOption) (*vmmdpb.UmountParentExt4Response, error) {
	return &vmmdpb.UmountParentExt4Response{}, nil
}

// Tier A5 (ADR-066) — the four migration RPCs are not driven
// from the gateway; they are schedd→vmmd only. Stubs return
// empty responses so the gRPC VmmdClient interface stays
// satisfied.
func (s *stubVmmdClient) PrepareLiveMigration(context.Context, *vmmdpb.PrepareLiveMigrationRequest, ...grpc.CallOption) (*vmmdpb.PrepareLiveMigrationResponse, error) {
	return &vmmdpb.PrepareLiveMigrationResponse{}, nil
}
func (s *stubVmmdClient) AdoptMigratedInstance(context.Context, *vmmdpb.AdoptMigratedInstanceRequest, ...grpc.CallOption) (*vmmdpb.AdoptMigratedInstanceResponse, error) {
	return &vmmdpb.AdoptMigratedInstanceResponse{}, nil
}
func (s *stubVmmdClient) AcknowledgeMigration(context.Context, *vmmdpb.AcknowledgeMigrationRequest, ...grpc.CallOption) (*vmmdpb.AcknowledgeMigrationResponse, error) {
	return &vmmdpb.AcknowledgeMigrationResponse{}, nil
}
func (s *stubVmmdClient) CancelLiveMigration(context.Context, *vmmdpb.CancelLiveMigrationRequest, ...grpc.CallOption) (*vmmdpb.CancelLiveMigrationResponse, error) {
	return &vmmdpb.CancelLiveMigrationResponse{}, nil
}

// FrameworkReady (issue #470 / PR #470-FU-B) is the vmmd-side
// receipt of the guest-init "framework ready" DGRAM. The
// forwardproxy handler never invokes this RPC (it's a vmmd→vmmd
// receipt path routed via the schedd), but the vmmd gRPC
// client interface demands the method so the stub satisfies
// the full surface. Returns an empty success; tests that
// exercise the framework-ready data path live in
// pkg/vmmdgrpc/bufconn_test.go.
func (s *stubVmmdClient) FrameworkReady(context.Context, *vmmdpb.FrameworkReadyRequest, ...grpc.CallOption) (*vmmdpb.FrameworkReadyResponse, error) {
	return &vmmdpb.FrameworkReadyResponse{}, nil
}

// stubLookup matches the NodeClientLookup interface; returns the
// same client for any non-empty id. ok=false on empty (matches the
// defensive 503 contract).
type stubLookup struct {
	cli *stubVmmdClient
}

func (s *stubLookup) ClientFor(_ context.Context, nodeID string) (vmmdpb.VmmdClient, io.Closer, bool) {
	if nodeID == "" {
		return nil, nil, false
	}
	return s.cli, nopCloserFn{}, true
}

type nopCloserFn struct{}

func (nopCloserFn) Close() error { return nil }

// newProxyTestBackend is a small Backend returning a known host +
// a fixed node id, so the handler dispatches straight through the
// forwarder without exercising the wake path. Reuses fakeBackend
// from handler_test.go when possible — but the in-package fake
// already exposes a host set, so the integration tests below wire
// it directly.

func TestHandler_DispatchesThroughProxyByNode(t *testing.T) {
	cli := &stubVmmdClient{
		resp: &vmmdpb.ForwardHTTPResponseInit{
			Status: 200,
		},
		body: []byte("forwarded:ok"),
	}
	lookup := &stubLookup{cli: cli}
	b := &fakeBackend{
		app:      App{ID: "app-1", Plan: api.PlanScale},
		host:     "app.example.com",
		upstream: "node-uuid-1",
		running:  true,
	}
	h := NewHandlerWith(b, NewMetrics(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	h.WithForwarding(ForwardingReverseProxy(lookup, slog.New(slog.NewTextHandler(io.Discard, nil))))

	req := httptest.NewRequest("GET", "/v1/probe?z=1", nil)
	req.Host = "app.example.com"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "forwarded:ok") {
		t.Errorf("body=%q, want it to contain the forwarder's response", rec.Body.String())
	}
	if len(cli.calls) != 1 {
		t.Fatalf("forwarder called %d times, want 1", len(cli.calls))
	}
}

func TestHandler_WithoutForwardingFallsBackToProxyFor(t *testing.T) {
	called := false
	b := &fakeBackend{
		app:      App{ID: "app-1", Plan: api.PlanScale},
		host:     "app.example.com",
		upstream: "addr-1",
		running:  true,
	}
	h := NewHandlerWith(b, NewMetrics(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	h.proxyFor = func(addr string) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			called = true
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, "addr="+addr)
		})
	}

	req := httptest.NewRequest("GET", "/v1/probe", nil)
	req.Host = "app.example.com"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if !called {
		t.Fatal("proxyFor not invoked on the legacy path")
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "addr=addr-1") {
		t.Errorf("body=%q, want it to mention the legacy addr", rec.Body.String())
	}
}

func TestHandler_LookupMissStill404sBeforeProxy(t *testing.T) {
	cli := &stubVmmdClient{
		resp: &vmmdpb.ForwardHTTPResponseInit{Status: 200},
	}
	lookup := &stubLookup{cli: cli}
	b := &fakeBackend{app: App{ID: "app-1", Plan: api.PlanScale}, host: "app.example.com", running: true}
	h := NewHandlerWith(b, NewMetrics(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	h.WithForwarding(ForwardingReverseProxy(lookup, slog.New(slog.NewTextHandler(io.Discard, nil))))
	req := httptest.NewRequest("GET", "/", nil)
	req.Host = "unknown.example.com"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status=%d, want 404", rec.Code)
	}
	if len(cli.calls) != 0 {
		t.Errorf("forwarder called on Lookup miss: %d calls", len(cli.calls))
	}
}

func TestHandler_WithForwardingIdempotent(t *testing.T) {
	first := func(Target) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTeapot) })
	}
	second := func(Target) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	}
	b := &fakeBackend{app: App{ID: "app-1", Plan: api.PlanScale}, host: "app.example.com", running: true}
	h := NewHandlerWith(b, NewMetrics(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	h.WithForwarding(first)
	h.WithForwarding(second)

	if got := h.proxyByNode(Target{NodeID: "anything"}); got == nil {
		t.Fatal("proxyByNode nil after install")
	}
}
