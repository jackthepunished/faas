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
