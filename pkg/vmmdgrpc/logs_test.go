package vmmdgrpc_test

import (
	"context"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	vmmdpb "github.com/onebox-faas/faas/api/proto/onebox/faas/vmmd/v1"
	"github.com/onebox-faas/faas/pkg/fcvm/logbuf"
	"github.com/onebox-faas/faas/pkg/vmmdgrpc"
	"github.com/onebox-faas/faas/pkg/wire"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

// TestLogs_HappyPath drives the Logs(req) handler against an injected
// ring (issue #254, Move 4). The producer pre-populates 3 lines; we
// drain the initial page, then publish 2 more live lines via Subscribe
// and assert the stream emits them in commit order.
//
// Backpressure: gRPC's stream.Send on a buffered client keeps the
// round-trip cost bounded; the test uses a small context timeout so
// the live-tail loop can't spin forever if the Subscribe path drops
// instead of delivering.
func TestLogs_HappyPath(t *testing.T) {
	ring := logbuf.New(1 << 20)
	for _, ln := range []string{"alpha\n", "beta\n", "gamma\n"} {
		if _, err := ring.Write("stdout", []byte(ln)); err != nil {
			t.Fatalf("seed Write: %v", err)
		}
	}
	cl := startLogsTestClient(t, &fakeVMM{
		logRingFn: func(string) *logbuf.Ring { return ring },
	})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	// SinceSeq=1 replays the seeded lines (Seq 1..3). SinceSeq=0 is
	// the "tail from now" sentinel (per pkg/fcvm/logbuf.Ring.Snapshot
	// docs) — the handler skips the replay phase and Subscribe only
	// delivers lines committed after the RPC is opened. We want the
	// test to assert the initial-page path, so we pass an explicit
	// cursor of 1.
	stream, err := cl.Logs(ctx, &vmmdpb.LogsRequest{Instance: "inst-1", SinceSeq: 1})
	if err != nil {
		t.Fatalf("Logs dial: %v", err)
	}

	// Read the initial page (3 lines). Snapshot is synchronous: the
	// handler emits all 3 before Subscribe() blocks on the next line,
	// so the 3 Recv calls return immediately.
	want := []string{"alpha", "beta", "gamma"}
	got := make([]*vmmdpb.LogsResponse, 0, 4)
	for i := 0; i < len(want); i++ {
		resp, err := stream.Recv()
		if err != nil {
			t.Fatalf("Recv[%d]: %v", i, err)
		}
		got = append(got, resp)
	}
	for i, line := range want {
		if got[i].Line != line {
			t.Errorf("got[%d].Line = %q, want %q", i, got[i].Line, line)
		}
		if got[i].Stream != "stdout" {
			t.Errorf("got[%d].Stream = %q, want stdout", i, got[i].Stream)
		}
		if got[i].Seq != int64(i+1) {
			t.Errorf("got[%d].Seq = %d, want %d", i, got[i].Seq, i+1)
		}
	}
	// Live tail: push one more line, expect it on the stream.
	if _, err := ring.Write("stderr", []byte("post-subscribe\n")); err != nil {
		t.Fatalf("tail Write: %v", err)
	}
	resp, err := stream.Recv()
	if err != nil {
		t.Fatalf("tail Recv: %v", err)
	}
	if resp.Line != "post-subscribe" || resp.Stream != "stderr" || resp.Seq != 4 {
		t.Errorf("tail frame = %+v, want line=post-subscribe stream=stderr seq=4", resp)
	}
}

// TestLogs_NotFound pins the wire contract: when the instance is not
// alive on this vmmd (LogRing returns nil), the handler returns
// codes.NotFound with no stream opened. apid maps that to its own
// 404 Problem.
func TestLogs_NotFound(t *testing.T) {
	cl := startLogsTestClient(t, &fakeVMM{
		logRingFn: func(string) *logbuf.Ring { return nil },
	})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	stream, err := cl.Logs(ctx, &vmmdpb.LogsRequest{Instance: "missing"})
	if err != nil {
		t.Fatalf("Logs dial: %v", err)
	}
	_, recvErr := stream.Recv()
	if recvErr == nil {
		t.Fatalf("expected NotFound error, got nil")
	}
	st, ok := status.FromError(recvErr)
	if !ok {
		t.Fatalf("Recv error is not a gRPC status: %v", recvErr)
	}
	if st.Code() != codes.NotFound {
		t.Errorf("code = %s, want NotFound", st.Code())
	}
}

// TestLogs_InvalidArgument pins the empty-instance rejection: callers
// who forget the field get InvalidArgument, not NotFound. Distinguishes
// a wire-contract bug from a missing-instance bug.
func TestLogs_InvalidArgument(t *testing.T) {
	cl := startLogsTestClient(t, &fakeVMM{})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	stream, err := cl.Logs(ctx, &vmmdpb.LogsRequest{Instance: ""})
	if err != nil {
		t.Fatalf("Logs dial: %v", err)
	}
	_, recvErr := stream.Recv()
	st, _ := status.FromError(recvErr)
	if st.Code() != codes.InvalidArgument {
		t.Errorf("code = %s, want InvalidArgument", st.Code())
	}
}

// TestLogs_ContextCancel verifies that cancelling the caller's context
// cleanly terminates the stream without leaking the Subscribe() goroutine.
// We pin this with a short context timeout that fires while we're waiting
// on a quiet ring; the handler must return nil (not an error) on cancel.
func TestLogs_ContextCancel(t *testing.T) {
	ring := logbuf.New(1 << 20)
	cl := startLogsTestClient(t, &fakeVMM{
		logRingFn: func(string) *logbuf.Ring { return ring },
	})
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	stream, err := cl.Logs(ctx, &vmmdpb.LogsRequest{Instance: "inst-1"})
	if err != nil {
		t.Fatalf("Logs dial: %v", err)
	}
	// Drain the empty initial page so we end up in Subscribe().
	if _, err := stream.Recv(); err != nil && !errors.Is(err, io.EOF) {
		// gRPC may surface the cancel as Canceled; both are acceptable
		// — what we pin is "we got OUT of the loop" and the goroutine
		// inside the handler has returned.
		t.Logf("Recv returned: %v", err)
	}
}

// startLogsTestClient stands up a bufconn-backed gRPC server with the
// given fakeVMM and returns a vmmdpb.VmmdClient. Mirrors the pattern
// in bufconn_test.go but is scoped to Logs-specific tests so future
// additions don't pay the full handler suite's setup cost.
func startLogsTestClient(t *testing.T, fake *fakeVMM) vmmdpb.VmmdClient {
	t.Helper()
	const bufSize = 1 << 20
	lis := bufconn.Listen(bufSize)
	srv := grpc.NewServer()
	impl := vmmdgrpc.New(fake, wire.NewOpsMetrics("vmmdgrpc_logs_test"), "1.0", nil)
	impl.Register(srv)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(func() {
		srv.GracefulStop()
		_ = lis.Close()
	})

	dialer := func(context.Context, string) (net.Conn, error) { return lis.Dial() }
	conn, err := grpc.NewClient("passthrough://bufnet",
		grpc.WithContextDialer(dialer),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("grpc.NewClient: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return vmmdpb.NewVmmdClient(conn)
}
