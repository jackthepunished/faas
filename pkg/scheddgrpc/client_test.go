package scheddgrpc_test

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	vmmdpb "github.com/onebox-faas/faas/api/proto/onebox/faas/vmmd/v1"
	"github.com/onebox-faas/faas/pkg/api"
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

// newClient stands up an in-process schedd server backed by eng and returns a
// scheddgrpc.Client dialed to it (the same wrapper gatewayd uses).
func newClient(t *testing.T, eng scheddgrpc.SchedAPI) *scheddgrpc.Client {
	t.Helper()
	srv := grpc.NewServer()
	scheddgrpc.New(eng, wire.NewOpsMetrics("schedd_client_test"), nil).Register(srv)

	lis := bufconn.Listen(1024 * 1024)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(func() { srv.Stop(); _ = lis.Close() })

	conn, err := grpc.NewClient("passthrough://bufnet",
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return lis.Dial() }),
	)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	c := scheddgrpc.NewClient(conn)
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func TestClientWake_ReturnsNodeID(t *testing.T) {
	c := newClient(t, &fakeEngine{
		wakeFn: func(_ context.Context, appID string) (sched.WakeResult, error) {
			if appID != "app-1" {
				t.Errorf("appID = %q", appID)
			}
			return sched.WakeResult{InstanceID: "i-1", NodeID: "node-test-1", Method: vmmdpb.WakeMethod_WAKE_RESTORE}, nil
		},
	})
	instanceID, nodeID, wakeID, err := c.Wake(context.Background(), "app-1")
	if err != nil {
		t.Fatalf("Wake: %v", err)
	}
	if nodeID != "node-test-1" {
		t.Errorf("nodeID = %q", nodeID)
	}
	if instanceID != "i-1" {
		t.Errorf("instanceID = %q, want i-1", instanceID)
	}
	if wakeID == "" {
		// Phase 1 fast path returns empty wake_id (the existing
		// RUNNING instance was minted by an earlier wake). The fake
		// engine returns WAKE_RESTORE which is a fast-path return,
		// so wake_id stays unset here. The dedicated wake_id
		// propagation is covered by TestClientWake_PropagatesWakeID.
		t.Logf("wakeID empty on fast-path return (expected); no assertion")
	}
}

func TestClientWake_CapacityLiftsToProblem(t *testing.T) {
	c := newClient(t, &fakeEngine{
		wakeFn: func(context.Context, string) (sched.WakeResult, error) {
			return sched.WakeResult{}, api.ErrCapacity("no RAM headroom")
		},
	})
	_, _, _, err := c.Wake(context.Background(), "app-1")
	if err == nil {
		t.Fatal("expected capacity denial")
	}
	// The wire status must lift back to *api.Problem so the gateway maps it to
	// the right RFC 7807 response (503) without re-classifying strings.
	prob := api.AsProblem(err)
	if prob == nil {
		t.Fatalf("error did not lift to *api.Problem: %v", err)
	}
	if prob.Status != 503 {
		t.Errorf("problem status = %d, want 503", prob.Status)
	}
}

func TestClientReportActivity(t *testing.T) {
	var got []state.InstanceTouch
	c := newClient(t, &fakeEngine{
		reportFn: func(_ context.Context, touches []state.InstanceTouch) (int, error) {
			got = touches
			return len(touches), nil
		},
	})
	now := time.UnixMilli(1_700_000_000_000)
	applied, err := c.ReportActivity(context.Background(), []state.InstanceTouch{
		{InstanceID: "i-1", LastRequest: now},
		{InstanceID: "i-2", LastRequest: now},
	})
	if err != nil {
		t.Fatalf("ReportActivity: %v", err)
	}
	if applied != 2 {
		t.Errorf("applied = %d, want 2", applied)
	}
	if len(got) != 2 || got[0].InstanceID != "i-1" || !got[0].LastRequest.Equal(now) {
		t.Errorf("touches round-trip = %+v", got)
	}
}

func TestDial_EmptyPath(t *testing.T) {
	if _, err := scheddgrpc.Dial(""); err == nil {
		t.Fatal("expected error on empty socket path")
	}
}

func TestClient_CloseNilConn(t *testing.T) {
	var c scheddgrpc.Client
	if err := c.Close(); err != nil {
		t.Errorf("Close on zero client = %v, want nil", err)
	}
}

// TestClientAdmitInstance_AdmitsNewInstance (issue #168) — the
// happy path on the high-level Client wrapper: the engine returns
// a WakeResult with identity fields populated and AtCapacity=false,
// and the wrapper surfaces all four return values to the caller.
// The bufconn proto-level coverage in bufconn_test.go proves the
// wire shape; this test proves the wrapper's translation.
func TestClientAdmitInstance_AdmitsNewInstance(t *testing.T) {
	const wantWakeID = "0193f7c0-bbbb-7abc-9def-0123456789ab"
	c := newClient(t, &fakeEngine{
		admitInstanceFn: func(context.Context, string) (sched.WakeResult, error) {
			return sched.WakeResult{
				InstanceID: "i-1",
				NodeID:     "n-1",
				Method:     vmmdpb.WakeMethod_WAKE_COLD_BOOT,
				WakeID:     wantWakeID,
			}, nil
		},
	})
	instanceID, nodeID, wakeID, atCapacity, err := c.AdmitInstance(context.Background(), "app-1")
	if err != nil {
		t.Fatalf("AdmitInstance: %v", err)
	}
	if instanceID != "i-1" {
		t.Errorf("instanceID = %q, want i-1", instanceID)
	}
	if nodeID != "n-1" {
		t.Errorf("nodeID = %q, want n-1", nodeID)
	}
	if wakeID != wantWakeID {
		t.Errorf("wakeID = %q, want %q", wakeID, wantWakeID)
	}
	if atCapacity {
		t.Errorf("atCapacity = true on admit path; want false")
	}
}

// TestClientAdmitInstance_AtCapacityIsTypedResult (issue #168) —
// the benign "already at max_concurrency" outcome must surface as
// atCapacity=true with empty identity fields and no error. The
// gateway treats this as a no-op when it already has ≥1 cached
// target; an error here would be a 503 instead of a 200 with
// at-capacity metadata.
func TestClientAdmitInstance_AtCapacityIsTypedResult(t *testing.T) {
	c := newClient(t, &fakeEngine{
		admitInstanceFn: func(context.Context, string) (sched.WakeResult, error) {
			return sched.WakeResult{AtCapacity: true}, nil
		},
	})
	instanceID, nodeID, wakeID, atCapacity, err := c.AdmitInstance(context.Background(), "app-1")
	if err != nil {
		t.Fatalf("AdmitInstance: at_capacity must NOT be lifted to an error; got %v", err)
	}
	if !atCapacity {
		t.Errorf("atCapacity = false; want true")
	}
	if instanceID != "" || nodeID != "" || wakeID != "" {
		t.Errorf("identity fields populated on at_capacity path: i=%q n=%q w=%q",
			instanceID, nodeID, wakeID)
	}
}

// TestClientAdmitInstance_LiftsError covers the liftErr path on
// AdmitInstance: a real admission failure (RAM headroom, etc.)
// must surface as an *api.Problem the gateway can route to 503.
// The bufconn test already covers the wire translation; this
// test covers the client-side unwrap.
func TestClientAdmitInstance_LiftsError(t *testing.T) {
	c := newClient(t, &fakeEngine{
		admitInstanceFn: func(context.Context, string) (sched.WakeResult, error) {
			return sched.WakeResult{}, api.ErrCapacity("no RAM headroom")
		},
	})
	_, _, _, _, err := c.AdmitInstance(context.Background(), "app-1")
	if err == nil {
		t.Fatal("expected capacity denial on AdmitInstance")
	}
	prob := api.AsProblem(err)
	if prob == nil {
		t.Fatalf("AdmitInstance error did not lift to *api.Problem: %v", err)
	}
	if prob.Status != 503 {
		t.Errorf("problem status = %d, want 503", prob.Status)
	}
}

// TestClientParkInstance_Ok covers the happy path: the engine
// parks the instance, the wrapper returns nil. Most of meterd's
// quota loop depends on this returning nil so a successful park
// doesn't log a spurious error.
func TestClientParkInstance_Ok(t *testing.T) {
	c := newClient(t, &fakeEngine{
		parkFn: func(_ context.Context, instanceID, reason string) error {
			if instanceID != "i-1" || reason != "idle" {
				t.Errorf("park args = (%q, %q)", instanceID, reason)
			}
			return nil
		},
	})
	if err := c.ParkInstance(context.Background(), "i-1", "idle"); err != nil {
		t.Errorf("ParkInstance: %v", err)
	}
}

// TestClientParkInstance_NotFound documents the boundary: when
// the engine returns state.ErrNotFound (the instance was already
// gone before we got there), the wrapper must surface it as the
// typed sentinel so meterd's errors.Is check works without string
// matching. Anything else (a generic error, a gRPC status) and the
// quota loop logs noise on every idle eviction.
func TestClientParkInstance_NotFound(t *testing.T) {
	c := newClient(t, &fakeEngine{
		parkFn: func(context.Context, string, string) error {
			return state.ErrNotFound
		},
	})
	err := c.ParkInstance(context.Background(), "i-1", "idle")
	if !errors.Is(err, state.ErrNotFound) {
		t.Errorf("ParkInstance = %v, want errors.Is(state.ErrNotFound)", err)
	}
}

// TestClientParkInstance_PlainErrorPassesThrough documents the
// other side of the same boundary: a non-NotFound error from the
// engine surfaces to the Client as a gRPC status with code
// Internal (the wire shape the server's `status.Error(codes.Internal, err.Error())`
// call produces). The meterd quota loop distinguishes this from
// the NotFound case by checking the gRPC code, not by string
// matching the error.
//
// The Client itself does NOT lift the error to *api.Problem (only
// the specific NotFound case is unwrapped). liftErr on a status
// whose `details` carry no api.Problem returns the status
// unchanged — which is what meterd wants here, so a non-NotFound
// failure is a 500-shaped error the caller treats as a hard
// failure rather than a benign no-op.
func TestClientParkInstance_PlainErrorPassesThrough(t *testing.T) {
	c := newClient(t, &fakeEngine{
		parkFn: func(context.Context, string, string) error {
			return errors.New("db boom")
		},
	})
	got := c.ParkInstance(context.Background(), "i-1", "idle")
	if got == nil {
		t.Fatal("expected error from ParkInstance")
	}
	if errors.Is(got, state.ErrNotFound) {
		t.Errorf("plain error lifted to state.ErrNotFound; that path is NotFound-only")
	}
	// Wire shape: gRPC status with code Internal. The message text
	// includes the engine's error string (server.go:157 wraps with
	// status.Error(codes.Internal, err.Error())).
	if code := status.Code(got); code != codes.Internal {
		t.Errorf("code = %v, want Internal", code)
	}
}
