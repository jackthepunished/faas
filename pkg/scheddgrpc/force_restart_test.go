// Tests for the ForceRestartInstance RPC (P2d follow-on to PR
// #1099). The RPC is the operator-initiated kill-instance +
// cold-boot-on-next-wake primitive. The handler is a thin
// wrapper around Engine.ForceRestart; this file pins the four
// behavioural edges the apid handler + gregalectl CLI depend
// on:
//
//  1. happy path      — engine returns snap IDs + nil error,
//                       server forwards ok=true + snap IDs.
//  2. race-loser      — engine returns state.ErrInstanceNotRunning,
//                       server surfaces ok=false + error_msg (NOT
//                       a gRPC status error so the CLI can
//                       render the cause verbatim).
//  3. not-found       — engine returns state.ErrNotFound, server
//                       surfaces codes.NotFound so the apid
//                       handler can translate to 404 with code
//                       "instance_not_found".
//  4. partial-success — engine returns snap IDs + non-nil error
//                       (destroy failed after snap-stale work).
//                       Server surfaces ok=true + snap IDs +
//                       error_msg so the operator learns both
//                       facts.
//
// The client-side NotFound → state.ErrNotFound mapping is a
// trivial pass-through (liftErr + codes.NotFound check) that
// mirrors Client.ForceColdBootNextWake at client.go:248; not
// worth a dedicated test.

package scheddgrpc_test

import (
	"context"
	"errors"
	"testing"

	scheddpb "github.com/onebox-faas/faas/api/proto/onebox/faas/schedd/v1"
	"github.com/onebox-faas/faas/pkg/state"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestServerForceRestartInstance_HappyPath verifies the server
// forwards the snap IDs through the proto envelope unchanged.
// The fakeEngine's forceRestartFn returns a fixed slice; the
// response.SnapIdsMarkedStale field must read back the same
// slice, response.Ok must be true, response.ErrorMsg must be
// empty.
//
// Mirrors TestServerForceColdBootNextWake_HappyPath shape.
func TestServerForceRestartInstance_HappyPath(t *testing.T) {
	cli := newServer(t, &fakeEngine{
		forceRestartFn: func(_ context.Context, _, _ string) ([]string, error) {
			return []string{"snap-warm-abc", "snap-init-def"}, nil
		},
	})
	resp, err := cli.ForceRestartInstance(context.Background(), &scheddpb.ForceRestartInstanceRequest{
		InstanceId: "inst-1",
		Reason:     "operator_smoke",
	})
	if err != nil {
		t.Fatalf("ForceRestartInstance happy path: unexpected err = %v", err)
	}
	if !resp.GetOk() {
		t.Errorf("response.Ok = false, want true on success path")
	}
	if got := resp.GetErrorMsg(); got != "" {
		t.Errorf("response.ErrorMsg = %q, want empty on success path", got)
	}
	got := resp.GetSnapIdsMarkedStale()
	want := []string{"snap-warm-abc", "snap-init-def"}
	if len(got) != len(want) {
		t.Fatalf("snap_ids = %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("snap_ids[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// TestServerForceRestartInstance_RaceLoser pins the
// state.ErrInstanceNotRunning → ok=false + error_msg mapping at
// the handler. Distinct from not-found (which maps to
// codes.NotFound); the race-loser is GRACEFUL — the engine
// observed a non-RUNNING state on the locked re-read and the
// desired end-state was achieved by the racing customer-driven
// action.
//
// The handler MUST NOT surface a gRPC status error here; the
// CLI/UI renders the cause verbatim via the error_msg field.
// If a future edit returns codes.FailedPrecondition the
// gregalectl `instances force-restart` flow regresses (it
// wouldn't get to render the cause).
func TestServerForceRestartInstance_RaceLoser(t *testing.T) {
	cli := newServer(t, &fakeEngine{
		forceRestartFn: func(_ context.Context, _, _ string) ([]string, error) {
			return nil, state.ErrInstanceNotRunning
		},
	})
	resp, err := cli.ForceRestartInstance(context.Background(), &scheddpb.ForceRestartInstanceRequest{
		InstanceId: "inst-already-parked",
	})
	// Deliberately NO err != nil check — the race-loser is
	// graceful. A non-nil err here means the handler regressed
	// to codes.FailedPrecondition.
	if err != nil {
		t.Fatalf("ForceRestartInstance race-loser: want nil err (graceful surface), got %v", err)
	}
	if resp.GetOk() {
		t.Errorf("response.Ok = true, want false on race-loser path")
	}
	if got := resp.GetErrorMsg(); got == "" {
		t.Errorf("response.ErrorMsg = empty, want state.ErrInstanceNotRunning text (CLI renders the cause verbatim)")
	}
	if got := len(resp.GetSnapIdsMarkedStale()); got != 0 {
		t.Errorf("response.SnapIdsMarkedStale = %v, want empty on race-loser (the locked re-read fired before snap-stale work)", resp.GetSnapIdsMarkedStale())
	}
}

// TestServerForceRestartInstance_NotFound pins the
// state.ErrNotFound → codes.NotFound mapping at the handler.
// This is the load-bearing mapping: apid's HTTP layer turns
// codes.NotFound into 404 with code "instance_not_found" so
// the operator UI can render "double-check the instance id".
func TestServerForceRestartInstance_NotFound(t *testing.T) {
	cli := newServer(t, &fakeEngine{
		forceRestartFn: func(_ context.Context, _, _ string) ([]string, error) {
			return nil, state.ErrNotFound
		},
	})
	_, err := cli.ForceRestartInstance(context.Background(), &scheddpb.ForceRestartInstanceRequest{
		InstanceId: "inst-missing",
	})
	if err == nil {
		t.Fatal("expected NotFound status, got nil")
	}
	if code := status.Code(err); code != codes.NotFound {
		t.Errorf("code = %v, want NotFound", code)
	}
}

// TestServerForceRestartInstance_PartialSuccess pins the
// partial-success shape: engine returns snap IDs (snap-stale
// work is durable) + non-nil error (destroy failed). Handler
// surfaces ok=true + snap_ids_marked_stale populated +
// error_msg populated so the operator learns both facts:
// "snapshots were flipped (next wake WILL cold-boot)" AND
// "destroy did not complete (check vmmd logs)".
//
// This is the load-bearing contract for the apid handler's
// 502-with-snap-IDs translation. If a future edit returns
// ok=false on snap-ID-populated partial-success, the operator
// sees a useless "failed" + the next wake silently cold-boots
// (because the stale flag flipped regardless), so the operator
// can't reconcile the "I killed it" claim against the "the
// next request cold-booted from a fresh snap" reality.
func TestServerForceRestartInstance_PartialSuccess(t *testing.T) {
	destroyErr := errors.New("fake: firecracker wedged")
	cli := newServer(t, &fakeEngine{
		forceRestartFn: func(_ context.Context, _, _ string) ([]string, error) {
			return []string{"snap-warm-only"}, destroyErr
		},
	})
	resp, err := cli.ForceRestartInstance(context.Background(), &scheddpb.ForceRestartInstanceRequest{
		InstanceId: "inst-wedged",
	})
	// The partial-success is graceful (no gRPC status error).
	if err != nil {
		t.Fatalf("ForceRestartInstance partial-success: want nil err (graceful surface), got %v", err)
	}
	if !resp.GetOk() {
		t.Errorf("response.Ok = false, want true on partial-success (snap-stale work IS the durable operator signal)")
	}
	got := resp.GetSnapIdsMarkedStale()
	if len(got) != 1 || got[0] != "snap-warm-only" {
		t.Errorf("response.SnapIdsMarkedStale = %v, want [snap-warm-only] (snap-stale work is durable through destroy failure)", got)
	}
	if msg := resp.GetErrorMsg(); msg == "" {
		t.Errorf("response.ErrorMsg = empty, want destroy-cause (the operator learns both facts)")
	}
}
