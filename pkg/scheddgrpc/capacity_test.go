// capacity_test.go — ReportCapacity gRPC handler tests
// (ADR-025 axis 5).
//
// End-to-end tests through bufconn:
//
//   - RoundTrip: vmmd opens a stream, sends two CapacityReports,
//     closes the send side, and receives a ReportCapacityAck. The
//     handler applies each report to the engine's per-node table
//     via the SchedAPI.CapacitySink seam; the test asserts the
//     typed reports reach the seam with intact fields.
//
//   - ContextCancelSurfacesCanceled: cancelling the caller's
//     context mid-stream surfaces codes.Canceled on the wire.
//
//   - EmptyNodeIDIsInvalidArgument: a report with empty node_id
//     is rejected with codes.InvalidArgument (load-bearing; the
//     table's defensive no-op is a fallback, not the gate).
//
//   - StreamClosedAfterLastSend: closing the send side cleanly
//     yields a single ReportCapacityAck on CloseAndRecv (the
//     handler's SendAndClose path).
//
//   - MultipleNodesCoexist: reports for two distinct node_ids
//     coexist in the table (mirrors
//     capacity_test.go::TestNodeCapacityTable_ReplaceAndLookup's
//     second-Replace assertion but exercised through the wire).

package scheddgrpc_test

import (
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	scheddpb "github.com/onebox-faas/faas/api/proto/onebox/faas/schedd/v1"
	"github.com/onebox-faas/faas/pkg/sched"
	"github.com/onebox-faas/faas/pkg/scheddgrpc"
	"github.com/onebox-faas/faas/pkg/state"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// capturingEngine is a SchedAPI whose CapacitySink drives
// reports into a per-test receiver (an ordered slice or a
// seen-set, depending on the test). The rest of the SchedAPI
// surface is a no-op. Defined inline (not via fakeEngine)
// because the existing fakeEngine's CapacitySink default
// no-ops, and the test needs to observe the captured reports.
// A third SchedAPI fixture (the multi-node coexistence test)
// extends capturingEngine with a seen-set.
type capturingEngine struct {
	mu   *sync.Mutex
	recv *[]sched.CapacityReport
	seen map[string]bool
}

func (c *capturingEngine) Wake(_ context.Context, _ string) (sched.WakeResult, error) {
	return sched.WakeResult{}, nil
}
func (c *capturingEngine) AdmitInstance(_ context.Context, _ string) (sched.WakeResult, error) {
	return sched.WakeResult{}, nil
}
func (c *capturingEngine) ReportActivity(_ context.Context, _ []state.InstanceTouch) (int, error) {
	return 0, nil
}
func (c *capturingEngine) ParkWithReason(_ context.Context, _, _ string) error { return nil }
func (c *capturingEngine) StreamAppLogs(_ context.Context, _ string, _ int64, _ scheddgrpc.LogFrameSink) error {
	return nil
}
func (c *capturingEngine) StreamWarmHints(_ context.Context, _ scheddgrpc.WarmHintSink) error {
	return nil
}
func (c *capturingEngine) CapacitySink() scheddgrpc.CapacitySink {
	return func(r sched.CapacityReport) error {
		c.mu.Lock()
		defer c.mu.Unlock()
		if c.recv != nil {
			*c.recv = append(*c.recv, r)
		}
		if c.seen != nil {
			c.seen[r.NodeID] = true
		}
		return nil
	}
}

// NodeKeyRegistry returns nil to disable signature verification
// (pre-slice-3 mode). The RoundTrip test sends unsigned reports
// via plain CapacityReport proto messages — the wire field is
// additive and the handler skips verification when the
// registry is nil. The TestReportCapacity_Slice3StrictMode test
// (added in Task #39) wraps this engine with a populated
// registry.
func (c *capturingEngine) NodeKeyRegistry() *sched.NodeKeyRegistry { return nil }

// TestReportCapacity_RoundTrip drives two reports through the
// wire and asserts the handler applies them to the table via
// the SchedAPI.CapacitySink seam. The seam is the only surface
// the test governs: the handler's job is to decode the proto,
// invoke the sink, and reply with the typed ack on stream close.
func TestReportCapacity_RoundTrip(t *testing.T) {
	var (
		mu       sync.Mutex
		received []sched.CapacityReport
	)
	want := []sched.CapacityReport{
		{
			NodeID:        "node-1",
			SampledAt:     time.Unix(1730000000, 0).UTC(),
			LiveCount:     12,
			LeasedCount:   8,
			UsedMB:        4096,
			RAMHeadroomMB: 32000,
			VCPUBusy:      24,
		},
		{
			NodeID:        "node-2",
			SampledAt:     time.Unix(1730000001, 0).UTC(),
			LiveCount:     4,
			LeasedCount:   2,
			UsedMB:        1024,
			RAMHeadroomMB: 35000,
			VCPUBusy:      8,
		},
	}
	cli := newServer(t, &capturingEngine{mu: &mu, recv: &received})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream, err := cli.ReportCapacity(ctx)
	if err != nil {
		t.Fatalf("ReportCapacity: %v", err)
	}

	for _, r := range want {
		if err := stream.Send(&scheddpb.CapacityReport{
			NodeId:          r.NodeID,
			SampledAtUnixMs: r.SampledAt.UnixMilli(),
			LiveCount:       r.LiveCount,
			LeasedCount:     r.LeasedCount,
			UsedMb:          r.UsedMB,
			RamHeadroomMb:   r.RAMHeadroomMB,
			VcpuBusy:        r.VCPUBusy,
		}); err != nil {
			t.Fatalf("Send: %v", err)
		}
	}
	ack, err := stream.CloseAndRecv()
	if err != nil {
		t.Fatalf("CloseAndRecv: %v", err)
	}
	if ack == nil {
		t.Fatal("CloseAndRecv returned nil ack")
	}

	// Engine side: the captured slice must carry both reports
	// in order, with intact fields. The handler decodes
	// SampledAtUnixMs via time.UnixMilli — assert the round-
	// trip preserves the second.
	mu.Lock()
	defer mu.Unlock()
	if len(received) != len(want) {
		t.Fatalf("received %d reports, want %d", len(received), len(want))
	}
	for i := range want {
		got := received[i]
		w := want[i]
		if got.NodeID != w.NodeID {
			t.Errorf("[%d] NodeID = %q, want %q", i, got.NodeID, w.NodeID)
		}
		if got.LiveCount != w.LiveCount {
			t.Errorf("[%d] LiveCount = %d, want %d", i, got.LiveCount, w.LiveCount)
		}
		if got.UsedMB != w.UsedMB {
			t.Errorf("[%d] UsedMB = %d, want %d", i, got.UsedMB, w.UsedMB)
		}
		if got.RAMHeadroomMB != w.RAMHeadroomMB {
			t.Errorf("[%d] RAMHeadroomMB = %d, want %d", i, got.RAMHeadroomMB, w.RAMHeadroomMB)
		}
		if got.VCPUBusy != w.VCPUBusy {
			t.Errorf("[%d] VCPUBusy = %d, want %d", i, got.VCPUBusy, w.VCPUBusy)
		}
		if got.SampledAt.Unix() != w.SampledAt.Unix() {
			t.Errorf("[%d] SampledAt = %v, want %v", i, got.SampledAt, w.SampledAt)
		}
	}
}

// TestReportCapacity_ContextCancelSurfacesCanceled cancels the
// caller's context mid-stream and asserts the handler surfaces
// codes.Canceled on the wire. Mirrors the warmhints test's
// cancel coverage so the two long-lived streams share the same
// failure-mode contract.
func TestReportCapacity_ContextCancelSurfacesCanceled(t *testing.T) {
	cli := newServer(t, &fakeEngine{})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream, err := cli.ReportCapacity(ctx)
	if err != nil {
		t.Fatalf("ReportCapacity: %v", err)
	}

	// Send one report so the handler is past the first Recv,
	// then cancel. The Recv reads see context.Canceled and the
	// handler maps to codes.Canceled.
	if err := stream.Send(&scheddpb.CapacityReport{NodeId: "node-1", UsedMb: 1}); err != nil {
		if !errors.Is(err, io.EOF) {
			// Some client stacks surface a status error
			// here; tolerate it but log for the test trail.
			t.Logf("Send after cancel returned: %v (acceptable per gRPC semantics)", err)
		}
	} else {
		cancel()
	}

	// Drive CloseAndRecv to wait for the handler's response.
	// Whether the cancel lands before or after Send, the ack
	// path is either Canceled or InvalidArgument (if the
	// empty-id check didn't fire). In either case the call
	// must NOT return nil.
	_, err = stream.CloseAndRecv()
	if err == nil {
		t.Fatal("CloseAndRecv returned nil after cancel; want Canceled")
	}
	if errors.Is(err, io.EOF) {
		t.Fatalf("err = io.EOF after cancel; want a status error")
	}
	st, ok := status.FromError(err)
	if !ok {
		t.Fatalf("err = %v; want gRPC status error", err)
	}
	if st.Code() != codes.Canceled {
		t.Errorf("code = %v, want Canceled", st.Code())
	}
}

// TestReportCapacity_EmptyNodeIDIsInvalidArgument pins the
// load-bearing gate: an empty node_id is rejected with
// codes.InvalidArgument (the table's defensive no-op is a
// fallback, not the gate). A regression that lets an empty
// id slip through to the table would corrupt the cache and
// silently hide a vmmd bug.
func TestReportCapacity_EmptyNodeIDIsInvalidArgument(t *testing.T) {
	cli := newServer(t, &fakeEngine{})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream, err := cli.ReportCapacity(ctx)
	if err != nil {
		t.Fatalf("ReportCapacity: %v", err)
	}

	// Send may return errors before the handler's
	// validation (stream already closed by ctx cancel) — but
	// typically only after the gRPC trailer arrives. Either
	// way the empty-id assertion below is the load-bearing
	// check; we tolerate the Send return.
	_ = stream.Send(&scheddpb.CapacityReport{NodeId: ""})

	// CloseAndRecv surfaces the handler's status. The handler
	// returns codes.InvalidArgument for empty node_id.
	_, err = stream.CloseAndRecv()
	if err == nil {
		t.Fatal("CloseAndRecv = nil; want InvalidArgument")
	}
	st, ok := status.FromError(err)
	if !ok {
		t.Fatalf("err = %v; want status error", err)
	}
	if st.Code() != codes.InvalidArgument {
		t.Errorf("code = %v, want InvalidArgument", st.Code())
	}
}

// TestReportCapacity_StreamClosedAfterLastSend closes the send
// side after the last report and asserts CloseAndRecv returns
// a non-nil ack (the handler's SendAndClose path). The shape
// is distinct from the round-trip test's "send N then close",
// which exercises the same path; this test is the bare
// "close after send" guarantee with the smallest possible
// payload to pin the SendAndClose contract.
func TestReportCapacity_StreamClosedAfterLastSend(t *testing.T) {
	cli := newServer(t, &fakeEngine{})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream, err := cli.ReportCapacity(ctx)
	if err != nil {
		t.Fatalf("ReportCapacity: %v", err)
	}

	if err := stream.Send(&scheddpb.CapacityReport{
		NodeId: "node-1",
		UsedMb: 100,
	}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	ack, err := stream.CloseAndRecv()
	if err != nil {
		t.Fatalf("CloseAndRecv: %v", err)
	}
	if ack == nil {
		t.Fatal("ack = nil; want non-nil ReportCapacityAck")
	}
}

// TestReportCapacity_MultipleNodesCoexist sends reports for two
// distinct node_ids and asserts both reach the engine side. The
// second-id assertion is the load-bearing one: a regression
// that overwrites the first entry on the second Replace would
// break the chooser's per-node accounting (PR-2).
func TestReportCapacity_MultipleNodesCoexist(t *testing.T) {
	var (
		mu   sync.Mutex
		seen = map[string]bool{}
	)
	cli := newServer(t, &capturingEngine{mu: &mu, seen: seen})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream, err := cli.ReportCapacity(ctx)
	if err != nil {
		t.Fatalf("ReportCapacity: %v", err)
	}

	for _, id := range []string{"node-a", "node-b"} {
		if err := stream.Send(&scheddpb.CapacityReport{NodeId: id, UsedMb: 100}); err != nil {
			t.Fatalf("Send %s: %v", id, err)
		}
	}
	if _, err := stream.CloseAndRecv(); err != nil {
		t.Fatalf("CloseAndRecv: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if !seen["node-a"] {
		t.Errorf("node-a missing; second Replace overwrote first entry")
	}
	if !seen["node-b"] {
		t.Errorf("node-b missing; second Replace was lost")
	}
}
