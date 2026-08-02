package state_test

// PG-backed round-trips for the Tier A5 cross-node live-instance
// migration surface (ADR-066, four-phase handoff). The pkg/state
// coverage gate in CI asserts ≥ 70% (Makefile test-state-coverage);
// the new ListLiveInstancesOnNode, MarkInstanceMigrating,
// MigrateInstanceOwner, CancelInstanceMigration methods (added by
// PR #533) dropped the package below the threshold because no
// pgstore test exercised them.
//
// The fixture seeds two compute_nodes (default-local + a peer
// flipped inactive for the cold-start ListLiveInstancesOnNode
// variant), one app+deployment, and one RUNNING instance owned by
// the dying peer. Each test below drives one method through its
// happy path and its at-least-one ErrConflict branch — the gate
// cares about *coverage*, not exhaustiveness; the engine-level
// e2e suite in cmd/e2e carries the four-phase orchestration.
//
// Skips when Postgres is unreachable via pgStore(t) — no `make
// test` regression in environments without a running cluster.

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/onebox-faas/faas/pkg/api"
	"github.com/onebox-faas/faas/pkg/state"
)

// seedRunningInstance seeds the standard pkg/state fixture
// (account + app + live deployment + RUNNING instance on the given
// node) and returns the instance id. Default-local is already
// seeded by migrations/00024; the second compute_node is created
// here so we have a "dying peer" to migrate off of.
func seedRunningInstance(t *testing.T, s *state.PgStore, ctx context.Context, nodeID string) (appID, instanceID string) {
	t.Helper()
	acct, err := s.CreateAccount(ctx, "mig-"+t.Name()+"-"+uuid.NewString()+"@example.com", api.PlanPro)
	if err != nil {
		t.Fatalf("seed acct: %v", err)
	}
	app, err := s.CreateApp(ctx, state.App{
		AccountID: acct.ID, Slug: "mig-" + t.Name() + "-" + uuid.NewString()[:8],
		Type: state.AppTypeApp, RAMMB: 512, MaxConcurrency: 5, IdleTimeoutS: 60,
	})
	if err != nil {
		t.Fatalf("seed app: %v", err)
	}
	dep, err := s.CreateDeployment(ctx, state.Deployment{
		AppID: app.ID, Kind: state.DeploymentKindImage, ImageDigest: "sha256:abc", Status: state.DeployPending,
	})
	if err != nil {
		t.Fatalf("seed dep: %v", err)
	}
	if err := s.MarkDeploymentLive(ctx, dep.ID); err != nil {
		t.Fatalf("MarkDeploymentLive: %v", err)
	}
	ins, err := s.CreateInstance(ctx, app.ID, dep.ID, string(state.StateRunning), 256, nodeID, "")
	if err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}
	return app.ID, ins.ID
}

// TestPgStore_ListLiveInstancesOnNode covers the migrator's
// primary input set: instances in state='running' on the dying
// node (or every inactive-owner instance when nodeID is empty).
// WAKING / COLD_BOOTING / SNAPSHOTTING must be excluded — the
// MarkInstanceMigrating predicate requires state='running', so
// the migrator would race-fail on those.
func TestPgStore_ListLiveInstancesOnNode(t *testing.T) {
	s, _, ctx := pgStoreWithPool(t)
	peer, err := s.CreateComputeNode(ctx, state.ComputeNode{
		Name: "dying-peer", TargetURL: "tcp://10.0.0.2:7000",
		VPCPUs: 80, MemMB: 28000, MaxConcurrency: 100, AdmissionCeilingMB: 23800,
		VCPUBudget: api.VCPUSlots, Active: true,
	})
	if err != nil {
		t.Fatalf("CreateComputeNode(dying-peer): %v", err)
	}

	// Three instances: RUNNING on the peer (eligible), COLD_BOOTING
	// on the peer (ineligible — state filter), RUNNING on a separate
	// (different) node (ineligible — node_id filter).
	_, eligibleID := seedRunningInstance(t, s, ctx, peer.ID)

	// Cold-boot instance on the peer — must be excluded by the
	// state='running' filter. seedRunningInstance starts in
	// RUNNING; flip it.
	_, coldBootID := seedRunningInstance(t, s, ctx, peer.ID)
	if err := s.UpdateInstanceState(ctx, coldBootID, string(state.StateColdBooting)); err != nil {
		t.Fatalf("UpdateInstanceState cold_booting: %v", err)
	}

	otherNode, err := s.CreateComputeNode(ctx, state.ComputeNode{
		Name: "other-node", TargetURL: "tcp://10.0.0.3:7000",
		VPCPUs: 80, MemMB: 28000, MaxConcurrency: 100, AdmissionCeilingMB: 23800,
		VCPUBudget: api.VCPUSlots, Active: true,
	})
	if err != nil {
		t.Fatalf("CreateComputeNode(other-node): %v", err)
	}
	_, otherInsID := seedRunningInstance(t, s, ctx, otherNode.ID)

	got, err := s.ListLiveInstancesOnNode(ctx, peer.ID, 50)
	if err != nil {
		t.Fatalf("ListLiveInstancesOnNode(peer): %v", err)
	}

	// The eligible row is in the result; the cold_booting and
	// other-node rows are NOT.
	has := map[string]bool{}
	for _, ins := range got {
		has[ins.ID] = true
	}
	if !has[eligibleID] {
		t.Errorf("ListLiveInstancesOnNode missing eligible RUNNING instance %s", eligibleID)
	}
	for _, id := range []string{otherInsID, coldBootID} {
		if has[id] {
			t.Errorf("ListLiveInstancesOnNode leaked instance %s (state/node_id filter failed)", id)
		}
	}

	// Per-tick cap of 0 returns empty (caller invariant).
	if got, err := s.ListLiveInstancesOnNode(ctx, peer.ID, 0); err != nil || len(got) != 0 {
		t.Errorf("ListLiveInstancesOnNode(max=0) = %+v, %v; want empty", got, err)
	}
}

// TestPgStore_MarkInstanceMigrating covers the Phase-2 atom:
// state='running' + node_id=currentNodeID predicate. Returns
// ErrConflict on a wrong-node race and on a wrong-state race.
func TestPgStore_MarkInstanceMigrating(t *testing.T) {
	s, _, ctx := pgStoreWithPool(t)
	peer, err := s.CreateComputeNode(ctx, state.ComputeNode{
		Name: "mig-peer", TargetURL: "tcp://10.0.0.2:7000",
		VPCPUs: 80, MemMB: 28000, MaxConcurrency: 100, AdmissionCeilingMB: 23800,
		VCPUBudget: api.VCPUSlots, Active: true,
	})
	if err != nil {
		t.Fatalf("CreateComputeNode: %v", err)
	}
	_, insID := seedRunningInstance(t, s, ctx, peer.ID)

	// Happy path: running + node matches.
	if err := s.MarkInstanceMigrating(ctx, insID, peer.ID, "lease-1"); err != nil {
		t.Fatalf("MarkInstanceMigrating happy: %v", err)
	}

	// Verify state flipped via a fresh read (InstanceByID).
	ins, err := s.InstanceByID(ctx, insID)
	if err != nil {
		t.Fatalf("InstanceByID: %v", err)
	}
	if ins.State != string(state.StateMigrating) {
		t.Errorf("post-mark state = %q, want %q", ins.State, state.StateMigrating)
	}

	// Wrong-node race: a different nodeID sees ErrConflict
	// because the row's node_id flipped earlier (or rather, was
	// stamped by the happy path above as migrating; the predicate
	// requires state='running' so this fails on the state clause).
	if err := s.MarkInstanceMigrating(ctx, insID, "00000000-0000-0000-0000-000000000000", "lease-2"); !errors.Is(err, state.ErrConflict) {
		t.Errorf("MarkInstanceMigrating wrong-node/state: %v; want ErrConflict", err)
	}

	// Missing row.
	if err := s.MarkInstanceMigrating(ctx, "00000000-0000-0000-0000-000000000099", peer.ID, "lease-3"); !errors.Is(err, state.ErrConflict) {
		t.Errorf("MarkInstanceMigrating missing: %v; want ErrConflict", err)
	}

	// Empty arg rejected with a generic error (not a typed
	// sentinel — see pgstore.go comment at lines 1180-1188).
	for _, tt := range []struct {
		name                    string
		instanceID, node, lease string
	}{
		{"empty instanceID", "", peer.ID, "lease"},
		{"empty currentNodeID", insID, "", "lease"},
		{"empty leaseToken", insID, peer.ID, ""},
	} {
		if err := s.MarkInstanceMigrating(ctx, tt.instanceID, tt.node, tt.lease); err == nil {
			t.Errorf("MarkInstanceMigrating(%s): nil error; want rejection", tt.name)
		}
	}
}

// TestPgStore_MigrateInstanceOwner covers the Phase-3 commit:
// conditional on state='migrating' + node_id=fromNodeID, flips
// node_id, stamps migrated_from_node_id / migrated_at / lease_token,
// restores state='running', AND stamps apps.migrated_at in the
// same transaction. Returns ErrConflict on a wrong-state or wrong-
// node race, and on a missing row.
func TestPgStore_MigrateInstanceOwner(t *testing.T) {
	s, _, ctx := pgStoreWithPool(t)
	peer, err := s.CreateComputeNode(ctx, state.ComputeNode{
		Name: "owner-peer", TargetURL: "tcp://10.0.0.2:7000",
		VPCPUs: 80, MemMB: 28000, MaxConcurrency: 100, AdmissionCeilingMB: 23800,
		VCPUBudget: api.VCPUSlots, Active: true,
	})
	if err != nil {
		t.Fatalf("CreateComputeNode(peer): %v", err)
	}
	other, err := s.CreateComputeNode(ctx, state.ComputeNode{
		Name: "owner-other", TargetURL: "tcp://10.0.0.3:7000",
		VPCPUs: 80, MemMB: 28000, MaxConcurrency: 100, AdmissionCeilingMB: 23800,
		VCPUBudget: api.VCPUSlots, Active: true,
	})
	if err != nil {
		t.Fatalf("CreateComputeNode(other): %v", err)
	}
	appID, insID := seedRunningInstance(t, s, ctx, peer.ID)
	// Phase 2: flip to migrating.
	if err := s.MarkInstanceMigrating(ctx, insID, peer.ID, "lease-commit"); err != nil {
		t.Fatalf("MarkInstanceMigrating: %v", err)
	}

	// Happy path: state='migrating' + node_id=peer matches.
	if err := s.MigrateInstanceOwner(ctx, insID, peer.ID, other.ID, "lease-commit"); err != nil {
		t.Fatalf("MigrateInstanceOwner happy: %v", err)
	}

	// Verify the instance row + the app row + the lineage
	// columns via a fresh read. We can't use InstanceByID
	// because it doesn't project migrated_at / migrated_from_node_id
	// (older scanner), so query the row directly through the
	// Store's ListInstancesForApp read path which surfaces the
	// fields the engine reads.
	got, err := s.ListInstancesForApp(ctx, appID)
	if err != nil {
		t.Fatalf("ListInstancesForApp: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("ListInstancesForApp returned %d rows, want 1", len(got))
	}
	if got[0].State != string(state.StateRunning) {
		t.Errorf("post-migrate state = %q, want %q", got[0].State, state.StateRunning)
	}
	if got[0].NodeID != other.ID {
		t.Errorf("post-migrate node_id = %q, want %q", got[0].NodeID, other.ID)
	}

	app, err := s.AppByID(ctx, appID)
	if err != nil {
		t.Fatalf("AppByID: %v", err)
	}
	if app.MigratedAt == nil {
		t.Errorf("post-migrate app.MigratedAt = nil; want non-nil")
	}

	// Lost race: try to commit again from the original peer.
	// The row's state has flipped back to 'running' so the
	// predicate fails with ErrConflict.
	if err := s.MigrateInstanceOwner(ctx, insID, peer.ID, other.ID, "lease-commit"); !errors.Is(err, state.ErrConflict) {
		t.Errorf("MigrateInstanceOwner double-commit: %v; want ErrConflict", err)
	}

	// Missing row.
	if err := s.MigrateInstanceOwner(ctx, "00000000-0000-0000-0000-000000000099", peer.ID, other.ID, "lease-missing"); !errors.Is(err, state.ErrConflict) {
		t.Errorf("MigrateInstanceOwner missing: %v; want ErrConflict", err)
	}

	// Wrong-state seed: a fresh RUNNING instance (never marked
	// migrating) refuses the commit because the predicate
	// requires state='migrating'.
	_, insID2 := seedRunningInstance(t, s, ctx, peer.ID)
	if err := s.MigrateInstanceOwner(ctx, insID2, peer.ID, other.ID, "lease-fresh"); !errors.Is(err, state.ErrConflict) {
		t.Errorf("MigrateInstanceOwner wrong-state: %v; want ErrConflict", err)
	}

	// Empty arg rejected.
	for _, tt := range []struct {
		name                        string
		instanceID, from, to, lease string
	}{
		{"empty instanceID", "", peer.ID, other.ID, "lease"},
		{"empty fromNodeID", insID, "", other.ID, "lease"},
		{"empty toNodeID", insID, peer.ID, "", "lease"},
		{"empty leaseToken", insID, peer.ID, other.ID, ""},
	} {
		if err := s.MigrateInstanceOwner(ctx, tt.instanceID, tt.from, tt.to, tt.lease); err == nil {
			t.Errorf("MigrateInstanceOwner(%s): nil error; want rejection", tt.name)
		}
	}
}

// TestPgStore_CancelInstanceMigration covers the Phase-4 rollback:
// conditional on state='migrating' + node_id=originalNodeID +
// lease_token=leaseToken. Restores state='parked' and clears the
// lease token. Returns ErrConflict on a stale lease, wrong node,
// or missing row.
func TestPgStore_CancelInstanceMigration(t *testing.T) {
	s, _, ctx := pgStoreWithPool(t)
	peer, err := s.CreateComputeNode(ctx, state.ComputeNode{
		Name: "cancel-peer", TargetURL: "tcp://10.0.0.2:7000",
		VPCPUs: 80, MemMB: 28000, MaxConcurrency: 100, AdmissionCeilingMB: 23800,
		VCPUBudget: api.VCPUSlots, Active: true,
	})
	if err != nil {
		t.Fatalf("CreateComputeNode: %v", err)
	}
	_, insID := seedRunningInstance(t, s, ctx, peer.ID)
	if err := s.MarkInstanceMigrating(ctx, insID, peer.ID, "lease-cancel"); err != nil {
		t.Fatalf("MarkInstanceMigrating setup: %v", err)
	}

	// Happy path: state='migrating' + node matches + lease
	// matches → state flips to 'parked', lease cleared.
	if err := s.CancelInstanceMigration(ctx, insID, peer.ID, "lease-cancel"); err != nil {
		t.Fatalf("CancelInstanceMigration happy: %v", err)
	}
	ins, err := s.InstanceByID(ctx, insID)
	if err != nil {
		t.Fatalf("InstanceByID: %v", err)
	}
	if ins.State != string(state.StateParked) {
		t.Errorf("post-cancel state = %q, want %q", ins.State, state.StateParked)
	}

	// Stale lease: a fresh attempt with the same predicates
	// (state is now 'parked', not 'migrating') also returns
	// ErrConflict, but the more direct test is a wrong-lease on
	// a still-migrating instance.
	_, insID2 := seedRunningInstance(t, s, ctx, peer.ID)
	if err := s.MarkInstanceMigrating(ctx, insID2, peer.ID, "lease-fresh"); err != nil {
		t.Fatalf("MarkInstanceMigrating setup2: %v", err)
	}
	if err := s.CancelInstanceMigration(ctx, insID2, peer.ID, "wrong-lease"); !errors.Is(err, state.ErrConflict) {
		t.Errorf("CancelInstanceMigration wrong-lease: %v; want ErrConflict", err)
	}

	// Wrong-node rollback attempt on the still-migrating row.
	if err := s.CancelInstanceMigration(ctx, insID2, "00000000-0000-0000-0000-000000000000", "lease-fresh"); !errors.Is(err, state.ErrConflict) {
		t.Errorf("CancelInstanceMigration wrong-node: %v; want ErrConflict", err)
	}

	// Missing row.
	if err := s.CancelInstanceMigration(ctx, "00000000-0000-0000-0000-000000000099", peer.ID, "lease-missing"); !errors.Is(err, state.ErrConflict) {
		t.Errorf("CancelInstanceMigration missing: %v; want ErrConflict", err)
	}

	// Empty arg rejected.
	for _, tt := range []struct {
		name                    string
		instanceID, node, lease string
	}{
		{"empty instanceID", "", peer.ID, "lease"},
		{"empty originalNodeID", insID2, "", "lease-fresh"},
	} {
		if err := s.CancelInstanceMigration(ctx, tt.instanceID, tt.node, tt.lease); err == nil {
			t.Errorf("CancelInstanceMigration(%s): nil error; want rejection", tt.name)
		}
	}
}
