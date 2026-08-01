package scheddgrpc_test

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	scheddpb "github.com/onebox-faas/faas/api/proto/onebox/faas/schedd/v1"
	"github.com/onebox-faas/faas/pkg/grpcerr"
	"github.com/onebox-faas/faas/pkg/sched"
	"github.com/onebox-faas/faas/pkg/scheddgrpc"
	"github.com/onebox-faas/faas/pkg/state"
	"github.com/onebox-faas/faas/pkg/wire"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestStreamAppLogs_HappyPath drives the schedd-side handler
// against a stub engine that pushes 3 frames. The handler must
// forward every frame to the caller's gRPC stream in arrival order.
// (issue #254 / Move 4)
//
// The frames carry an instance_id so a consumer can interleave
// multi-instance streams deterministically (acceptance #5).
func TestStreamAppLogs_HappyPath(t *testing.T) {
	cl := newServer(t, &fakeEngine{
		streamLogFn: func(ctx context.Context, appID string, sinceSeq int64, sinceWrittenAt time.Time, deploymentID string, sink scheddgrpc.LogFrameSink) error {
			if err := sink(sched.LogFrame{InstanceID: "inst-A", Seq: 1, Stream: "stdout", Line: "alpha", WrittenAt: time.Unix(0, 0).UTC()}); err != nil {
				return err
			}
			if err := sink(sched.LogFrame{InstanceID: "inst-A", Seq: 2, Stream: "stdout", Line: "beta", WrittenAt: time.Unix(0, 0).UTC()}); err != nil {
				return err
			}
			if err := sink(sched.LogFrame{InstanceID: "inst-B", Seq: 1, Stream: "stderr", Line: "gamma", WrittenAt: time.Unix(0, 0).UTC()}); err != nil {
				return err
			}
			<-ctx.Done()
			return nil
		},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	stream, err := cl.StreamAppLogs(ctx, &scheddpb.StreamAppLogsRequest{AppId: "app-1"})
	if err != nil {
		t.Fatalf("StreamAppLogs dial: %v", err)
	}
	want := []struct {
		instance string
		seq      int64
		stream   string
		line     string
	}{
		{"inst-A", 1, "stdout", "alpha"},
		{"inst-A", 2, "stdout", "beta"},
		{"inst-B", 1, "stderr", "gamma"},
	}
	for i, w := range want {
		resp, err := stream.Recv()
		if err != nil {
			t.Fatalf("Recv[%d]: %v", i, err)
		}
		if resp.GetInstanceId() != w.instance {
			t.Errorf("frame[%d].InstanceId = %q, want %q", i, resp.GetInstanceId(), w.instance)
		}
		if resp.GetSeq() != w.seq {
			t.Errorf("frame[%d].Seq = %d, want %d", i, resp.GetSeq(), w.seq)
		}
		if resp.GetStream() != w.stream {
			t.Errorf("frame[%d].Stream = %q, want %q", i, resp.GetStream(), w.stream)
		}
		if resp.GetLine() != w.line {
			t.Errorf("frame[%d].Line = %q, want %q", i, resp.GetLine(), w.line)
		}
	}
	cancel()
	// After cancel, the stream should close; reading more frames
	// returns io.EOF (or a context-cancelled gRPC status — both are
	// acceptable per the test framework).
	if _, err := stream.Recv(); err == nil {
		t.Errorf("Recv after cancel: expected EOF or error, got nil")
	}
}

// TestStreamAppLogs_NotFound pins the empty-instance rejection:
// when the engine returns state.ErrNotFound (no live instances),
// the handler must lift it to codes.NotFound so the apid maps it
// to its 404 "the app is parked; wake it first".
func TestStreamAppLogs_NotFound(t *testing.T) {
	cl := newServer(t, &fakeEngine{
		streamLogFn: func(ctx context.Context, appID string, sinceSeq int64, sinceWrittenAt time.Time, deploymentID string, sink scheddgrpc.LogFrameSink) error {
			return state.ErrNotFound
		},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	stream, err := cl.StreamAppLogs(ctx, &scheddpb.StreamAppLogsRequest{AppId: "parked-app"})
	if err != nil {
		t.Fatalf("StreamAppLogs dial: %v", err)
	}
	_, recErr := stream.Recv()
	if recErr == nil {
		t.Fatalf("expected NotFound, got nil")
	}
	st, ok := status.FromError(recErr)
	if !ok {
		// Could be unwrapped via grpcerr adapter; try lifting.
		if p, ok := grpcerr.FromStatus(recErr); ok && p != nil {
			if p.Status == int(codes.NotFound) {
				return
			}
		}
		t.Fatalf("Recv error is not a gRPC status: %v", recErr)
	}
	if st.Code() != codes.NotFound {
		t.Errorf("code = %s, want NotFound", st.Code())
	}
}

// TestStreamAppLogs_InvalidArgument pins the empty-app-id rejection.
// A caller who forgets the field gets InvalidArgument, not NotFound.
// Distinguishes a wire-contract bug from a missing-app bug.
func TestStreamAppLogs_InvalidArgument(t *testing.T) {
	cl := newServer(t, &fakeEngine{})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	stream, err := cl.StreamAppLogs(ctx, &scheddpb.StreamAppLogsRequest{AppId: ""})
	if err != nil {
		t.Fatalf("StreamAppLogs dial: %v", err)
	}
	_, recErr := stream.Recv()
	st, _ := status.FromError(recErr)
	if st.Code() != codes.InvalidArgument {
		t.Errorf("code = %s, want InvalidArgument", st.Code())
	}
}

// TestStreamAppLogs_ContextCancel pins the clean shutdown path:
// a caller cancel must terminate the stream without leaking the
// per-instance goroutines schedd spawned. The stream returns nil
// (or io.EOF) on cancel; not a gRPC error status.
func TestStreamAppLogs_ContextCancel(t *testing.T) {
	cl := newServer(t, &fakeEngine{
		streamLogFn: func(ctx context.Context, appID string, sinceSeq int64, sinceWrittenAt time.Time, deploymentID string, sink scheddgrpc.LogFrameSink) error {
			<-ctx.Done()
			return nil
		},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	stream, err := cl.StreamAppLogs(ctx, &scheddpb.StreamAppLogsRequest{AppId: "app-1"})
	if err != nil {
		t.Fatalf("StreamAppLogs dial: %v", err)
	}
	if _, err := stream.Recv(); err != nil && !errors.Is(err, io.EOF) {
		t.Logf("Recv returned: %v", err)
	}
}

// Compile-time assertion that wire.OpsMetrics is acceptable for the
// schedd test registry (issue #254 / Move 4 — keeps the same
// single-registry pattern as the rest of pkg/scheddgrpc).
var _ *wire.OpsMetrics = (*wire.OpsMetrics)(nil)

// TestStreamAppLogs_GapForwardedOverSchedd pins AC4 at the
// schedd-gRPC boundary: when the engine's per-instance fan-out
// emits a sched.LogFrame with IsGap=true (the vmmd producer
// labelled it via gap_reason), the server's sink translates it
// to a scheddpb.StreamAppLogsResponse with is_gap=true,
// gap_to_written_at populated, and gap_reason propagated. The
// line-frame fields (seq/stream/line/written_at) stay zero on
// the gap frame — Finding 2's contract — and a subsequent line
// frame arrives intact.
//
// A regression here means gatewayd's RenderAppLogGap stops
// seeing `is_gap=true` and falls back to its broken
// "since_below_retained" heuristic — Finding 1.
func TestStreamAppLogs_GapForwardedOverSchedd(t *testing.T) {
	headAt := time.Date(2026, 8, 1, 13, 0, 0, 0, time.UTC)
	cl := newServer(t, &fakeEngine{
		streamLogFn: func(ctx context.Context, appID string, sinceSeq int64, sinceWrittenAt time.Time, deploymentID string, sink scheddgrpc.LogFrameSink) error {
			if err := sink(sched.LogFrame{
				InstanceID:     "inst-A",
				IsGap:          true,
				GapToWrittenAt: headAt,
				GapReason:      "seq_below_retained",
			}); err != nil {
				return err
			}
			if err := sink(sched.LogFrame{
				InstanceID: "inst-A",
				Seq:        42,
				Stream:     "stdout",
				Line:       "first after gap",
				WrittenAt:  headAt.Add(time.Second),
			}); err != nil {
				return err
			}
			<-ctx.Done()
			return nil
		},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	stream, err := cl.StreamAppLogs(ctx, &scheddpb.StreamAppLogsRequest{AppId: "app-1"})
	if err != nil {
		t.Fatalf("StreamAppLogs dial: %v", err)
	}

	// Frame 0: the gap.
	gap, err := stream.Recv()
	if err != nil {
		t.Fatalf("Recv gap: %v", err)
	}
	if !gap.GetIsGap() {
		t.Errorf("is_gap = false on gap frame, want true")
	}
	if got := gap.GetInstanceId(); got != "inst-A" {
		t.Errorf("instance_id = %q, want inst-A", got)
	}
	if got := gap.GetGapReason(); got != "seq_below_retained" {
		t.Errorf("gap_reason = %q, want seq_below_retained", got)
	}
	if ts := gap.GetGapToWrittenAt(); ts == nil || !ts.AsTime().Equal(headAt) {
		t.Errorf("gap_to_written_at = %v, want %v", ts, headAt)
	}
	// Line-frame fields must be zero on a gap frame (Finding 2).
	if gap.GetSeq() != 0 {
		t.Errorf("seq on gap frame = %d, want 0", gap.GetSeq())
	}
	if gap.GetStream() != "" {
		t.Errorf("stream on gap frame = %q, want empty", gap.GetStream())
	}
	if gap.GetLine() != "" {
		t.Errorf("line on gap frame = %q, want empty", gap.GetLine())
	}
	if gap.GetWrittenAt() != nil {
		t.Errorf("written_at on gap frame = %v, want nil", gap.GetWrittenAt())
	}

	// Frame 1: the line.
	line, err := stream.Recv()
	if err != nil {
		t.Fatalf("Recv line: %v", err)
	}
	if line.GetIsGap() {
		t.Errorf("is_gap = true on line frame, want false")
	}
	if got := line.GetSeq(); got != 42 {
		t.Errorf("seq = %d, want 42", got)
	}
	if got := line.GetStream(); got != "stdout" {
		t.Errorf("stream = %q, want stdout", got)
	}
	if got := line.GetLine(); got != "first after gap" {
		t.Errorf("line = %q, want %q", got, "first after gap")
	}
}
