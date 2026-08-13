// Whitebox regression tests for the wsOutcome labelling in
// rawStreamOnceWithEvents (issue #676 / ADR-080 PR-B code-review
// findings #1 + #2 + #3). Internal-package test (same pattern
// as pkg/gateway/handler_test.go) because the wsOutcome /
// withWSContext helpers are unexported. The external test
// corpus (pkg/gateway/forwardproxy_test.go) covers the wire
// path; this file pins the observability contract.

package gateway

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	vmmdpb "github.com/onebox-faas/faas/api/proto/onebox/faas/vmmd/v1"
	"github.com/onebox-faas/faas/pkg/api"
	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// initErrorFakeStream is the minimum BidiStreamingClient
// shape rawStreamOnceWithEvents requires. The stream emits a
// single Init frame with status=502 + Error="dial refused"
// (the bridge's "guest refused connection" failure path),
// then returns io.EOF — the same shape the external
// TestRawStreamReverseProxy_InitError_Populated fixture uses.
//
// Why we don't reuse fakeRawBidiStream from
// forwardproxy_test.go: that file is package gateway_test,
// so its unexported types are not reachable from package
// gateway. The minimal replica below is the whitebox
// equivalent.
type initErrorFakeStream struct {
	grpc.ClientStream
	mu       sync.Mutex
	emitted  bool
	closed   bool
	sentInit bool
}

func (s *initErrorFakeStream) Send(req *vmmdpb.ForwardRawRequest) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if req.GetInit() != nil {
		s.sentInit = true
	}
	return nil
}

func (s *initErrorFakeStream) Recv() (*vmmdpb.ForwardRawResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.emitted {
		s.emitted = true
		return &vmmdpb.ForwardRawResponse{
			Frame: &vmmdpb.ForwardRawResponse_Init{
				Init: &vmmdpb.ForwardRawResponseInit{
					Status: 502,
					Error:  "dial refused",
				},
			},
		}, nil
	}
	return nil, io.EOF
}

func (s *initErrorFakeStream) CloseSend() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	return nil
}

func (s *initErrorFakeStream) Header() (metadata.MD, error) { return nil, nil }
func (s *initErrorFakeStream) Trailer() metadata.MD         { return nil }
func (s *initErrorFakeStream) SendMsg(any) error            { return nil }
func (s *initErrorFakeStream) RecvMsg(any) error            { return nil }

// initErrorFakeVmmdClient returns the canned init-error
// stream regardless of the call. Embeds vmmdpb.VmmdClient
// so the scaffold surface (AcknowledgeMigration, etc.) is
// automatically satisfied — the forwarder only ever calls
// ForwardRawStream, but the interface contract requires
// every method to be present.
type initErrorFakeVmmdClient struct {
	vmmdpb.VmmdClient
	stream *initErrorFakeStream
}

func (c *initErrorFakeVmmdClient) ForwardRawStream(_ context.Context, _ ...grpc.CallOption) (grpc.BidiStreamingClient[vmmdpb.ForwardRawRequest, vmmdpb.ForwardRawResponse], error) {
	return c.stream, nil
}

// TestRawStreamOnceWithEvents_InitErrorLabelsUpstreamUnavailable
// pins PR-B code review finding #1: the init.Error path
// must label wsOutcome as WSOutcomeUpstreamUnavailable, not
// the defer's default of WSOutcomeClientDisconnect. A bridge
// dial failure is an upstream-availability issue (the bridge
// wrote the synthetic 502 from inside the per-instance netns
// because the guest refused the connect). Mislabeling
// pollutes the
// rate(gateway_ws_session_duration_seconds{outcome="client_disconnect"})
// panel with bridge-side failures.
func TestRawStreamOnceWithEvents_InitErrorLabelsUpstreamUnavailable(t *testing.T) {
	m := NewMetrics()
	stream := &initErrorFakeStream{}
	cli := &initErrorFakeVmmdClient{stream: stream}

	wsCtx := withWSContext(context.Background(), api.PlanHobby, m)
	req := httptest.NewRequestWithContext(wsCtx, http.MethodGet, "/socket", strings.NewReader(""))
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	rec := httptest.NewRecorder()

	rawStreamOnceWithEvents(rec, req, cli, nil, Target{NodeID: "n-1", InstanceID: "i-x", Port: 8080}, nil, nil)

	// Assert: the histogram count for {plan=hobby,
	// outcome=upstream_unavailable} is 1, and the count for
	// {plan=hobby, outcome=client_disconnect} is 0.
	// testutil.ToFloat64 works on Counter/Gauge but
	// histogram.WithLabelValues returns prometheus.Observer
	// (read-side only). Reuse the existing
	// histogramObservationCount helper from handler_test.go
	// (whitebox helper, same package) — it reads the
	// _count sample via the dto format.
	gotUp := histogramObservationCount(t, m.wsSessionDuration.WithLabelValues("hobby", string(WSOutcomeUpstreamUnavailable)).(prometheus.Histogram))
	if gotUp != 1 {
		t.Errorf("wsSessionDuration{hobby,upstream_unavailable} count = %d, want 1", gotUp)
	}
	gotCD := histogramObservationCount(t, m.wsSessionDuration.WithLabelValues("hobby", string(WSOutcomeClientDisconnect)).(prometheus.Histogram))
	if gotCD != 0 {
		t.Errorf("wsSessionDuration{hobby,client_disconnect} count = %d, want 0 — the init.Error path mislabeled the defer default", gotCD)
	}
}

// TestWithWSContext_StampsWhenMetricsNil pins PR-B code
// review finding #3: the previous short-circuit
// (`if plan == "" && m == nil { return ctx }`) was asymmetric.
// The helper now always stamps; the asymmetric path (plan != ""
// with m == nil) is now identical to the symmetric one.
func TestWithWSContext_StampsWhenMetricsNil(t *testing.T) {
	ctx := context.Background()
	stamped := withWSContext(ctx, api.PlanPro, nil)
	if stamped == ctx {
		t.Fatalf("withWSContext(plan=Pro, m=nil) returned the original ctx; want a stamped copy")
	}
	plan, m := wsContextFrom(stamped)
	if plan != api.PlanPro {
		t.Errorf("plan = %q, want pro", plan)
	}
	if m != nil {
		t.Errorf("metrics = %v, want nil (the caller passed nil)", m)
	}
}

// TestWithWSContext_StampsWhenPlanEmpty is the symmetric case
// for finding #3: the empty-plan + nil-metrics path
// (e.g. a constructor call before the handler has
// resolved the app row) now also stamps, so the receiver
// gets a uniform context shape regardless of which fields
// are populated.
func TestWithWSContext_StampsWhenPlanEmpty(t *testing.T) {
	ctx := context.Background()
	stamped := withWSContext(ctx, "", mForTest())
	if stamped == ctx {
		t.Fatalf("withWSContext(plan=\"\", m=non-nil) returned the original ctx; want a stamped copy")
	}
	plan, m := wsContextFrom(stamped)
	if plan != "" {
		t.Errorf("plan = %q, want \"\"", plan)
	}
	if m == nil {
		t.Errorf("metrics = nil, want non-nil")
	}
}

// mForTest is a small helper to instantiate a Metrics for
// the withWSContext test — the constructor is unexported
// but NewMetrics is the public seam.
func mForTest() *Metrics { return NewMetrics() }

// Compile-time guard: the unused codes import ensures the
// helper path stays consistent with the external fixture
// (forwardproxy_test.go imports google.golang.org/grpc/codes
// for similar shape guards).
var _ = codes.Unavailable

// Compile-time guard: status is used by the wired
// initErrorFakeStream.Recv path in production but not from
// the test directly; the import keeps the fake symmetric
// with the production receiver loop's error conversion.
var _ = status.Error
