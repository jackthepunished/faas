// Tests for the ForceColdBootNextWake RPC (P2b of the
// operator-side observability mega-PR). The RPC is the
// operator-side recovery primitive for the case where the live
// instance is fine but the snapshot backing the warm tier is
// suspected to be the carrier of a customer-reported wedge. The
// handler is a thin wrapper around Engine.ForceColdBootNextWake;
// this file pins the four behavioural edges that the apid
// handler depends on:
//
//  1. happy path — engine returns a list of snap IDs, server
//     forwards them through the proto envelope.
//  2. not-found  — engine returns state.ErrNotFound, server
//     surfaces gRPC codes.NotFound so the apid handler can
//     translate to 404 with code "deployment_not_found".
//  3. empty      — engine returns ([]string{}, nil) for a
//     deployment with no snapshots; the server forwards the
//     empty list (durable no-op for the audit row).
//
// The client-side NotFound → state.ErrNotFound mapping is a
// trivial pass-through (liftErr + codes.NotFound check) that
// mirrors Client.ParkInstance at client.go:222; not worth a
// dedicated test.

package scheddgrpc_test

import (
	"context"
	"testing"

	scheddpb "github.com/onebox-faas/faas/api/proto/onebox/faas/schedd/v1"
	"github.com/onebox-faas/faas/pkg/state"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestServerForceColdBootNextWake_HappyPath verifies the server
// forwards the snap IDs through the proto envelope unchanged.
// The fakeEngine's forceColdBootFn returns a fixed slice; the
// response.SnapIdsMarkedStale field must read back the same slice.
func TestServerForceColdBootNextWake_HappyPath(t *testing.T) {
	cli := newServer(t, &fakeEngine{
		forceColdBootFn: func(_ context.Context, _ string) ([]string, error) {
			return []string{"snap-warm-abc", "snap-init-def"}, nil
		},
	})
	resp, err := cli.ForceColdBootNextWake(context.Background(), &scheddpb.ForceColdBootNextWakeRequest{
		DeploymentId: "dep-1",
	})
	if err != nil {
		t.Fatalf("ForceColdBootNextWake happy path: unexpected err = %v", err)
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

// TestServerForceColdBootNextWake_NotFound pins the error mapping
// at server.go: ForceColdBootNextWake maps state.ErrNotFound to
// gRPC codes.NotFound so the apid handler can render a 404 with
// code "deployment_not_found" on the wire.
func TestServerForceColdBootNextWake_NotFound(t *testing.T) {
	cli := newServer(t, &fakeEngine{
		forceColdBootFn: func(_ context.Context, _ string) ([]string, error) {
			return nil, state.ErrNotFound
		},
	})
	_, err := cli.ForceColdBootNextWake(context.Background(), &scheddpb.ForceColdBootNextWakeRequest{
		DeploymentId: "dep-missing",
	})
	if err == nil {
		t.Fatal("expected NotFound status, got nil")
	}
	if code := status.Code(err); code != codes.NotFound {
		t.Errorf("code = %v, want NotFound", code)
	}
}

// TestServerForceColdBootNextWake_EmptyDeployment pins the
// "deployment has no snapshots" path: engine returns an empty
// slice + nil error; server forwards an empty list on the wire.
// The apid handler relies on this to emit the audit row + return
// 200 OK with `snap_ids_marked_stale: []` — the durable record
// of the operator check, even when no snap was flipped.
func TestServerForceColdBootNextWake_EmptyDeployment(t *testing.T) {
	cli := newServer(t, &fakeEngine{
		forceColdBootFn: func(_ context.Context, _ string) ([]string, error) {
			return []string{}, nil
		},
	})
	resp, err := cli.ForceColdBootNextWake(context.Background(), &scheddpb.ForceColdBootNextWakeRequest{
		DeploymentId: "dep-empty",
	})
	if err != nil {
		t.Fatalf("ForceColdBootNextWake empty deployment: unexpected err = %v", err)
	}
	if got := resp.GetSnapIdsMarkedStale(); len(got) != 0 {
		t.Errorf("snap_ids = %v, want empty slice", got)
	}
}
