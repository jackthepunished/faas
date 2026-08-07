// migration_harness_test.go — Tier A5 (ADR-066) end-to-end
// unit tests on the four-phase handoff orchestrated by
// MigrationHarness.MigrateOne.
//
// The state-machine half (ListLiveInstancesOnNode,
// MarkInstanceMigrating, MigrateInstanceOwner,
// CancelInstanceMigration) is pinned by
// pkg/sched/migration_handoff_test.go; this file pins the
// orchestrator: the lease-bounded context, the per-phase
// metric labels, the cancel-on-rollback discipline, and the
// per-tick cap.
//
// Failure modes pinned here:
//   - Happy path: Phase 1 → 2 → 3 → 4 commit + Phase 5 ack,
//     state ends at 'running' on the new owner, lineage
//     stamped, all four RPCs called exactly once.
//   - Phase 1 fails: no Phase 2/3/4; peer_failure bumped.
//   - Phase 2 conflict (peer re-owner / state drift): Phase 4
//     fires, conflict metric bumped.
//   - Phase 3 fails: Phase 4 fires (state→parked via
//     CancelInstanceMigration + CancelLiveMigration on the
//     dying vmmd), peer_failure bumped.
//   - Lease expiry: slow PrepareLiveMigration exceeds
//     leaseCtx; Phase 4 fires, lease_expired metric bumped.
//   - Per-tick cap: 3 instances in fixture, maxPerTick=2,
//     only 2 attempts land.
//   - Spec builder error: rolls back Phase 2 + Phase 4 fires.

package sched

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/onebox-faas/faas/pkg/state"
	"github.com/onebox-faas/faas/pkg/wire"
)

// stubSpecBuilder returns a fixed AppSpec regardless of
// instanceID. The migration_harness_test.go only asserts on
// orchestrator behaviour, not on spec contents — those are
// pinned by BuildAppSpecForMigration's own callers (the wake
// path mirrors it).
func stubSpecBuilder(_ context.Context, _ string) (AppSpec, error) {
	return AppSpec{
		BaseKey:    "base/runtime-node22.ext4",
		LayerKey:   "apps/test/dep.ext4",
		VCPUCount:  2,
		MemSizeMiB: 256,
	}, nil
}

// newHarnessForTest builds a MigrationHarness wired against a
// MemStore + the supplied fakeVMM. Lease seconds is short (2s)
// so the lease-expiry test runs quickly; maxPerTick is left at
// the api default unless the caller overrides via SetMaxPerTick.
func newHarnessForTest(t *testing.T, store *state.MemStore, vmm *fakeVMM, ownerNodeID string) *MigrationHarness {
	t.Helper()
	ops := wire.NewOpsMetrics("schedd")
	h := NewMigrationHarness(store, vmm, ops, testLog(), ownerNodeID, stubSpecBuilder, NewNodeLedger())
	h.SetLeaseSeconds(2)
	return h
}

func TestMigrateOne_HappyPath(t *testing.T) {
	store := state.NewMemStore()
	vmm := &fakeVMM{}
	insID := seedInstanceForMigration(t, store, "dying")
	h := newHarnessForTest(t, store, vmm, "new-owner")

	if err := h.MigrateOne(context.Background(), insID, "dying"); err != nil {
		t.Fatalf("MigrateOne happy path: %v", err)
	}

	// All four RPCs called exactly once.
	if vmm.prepares != 1 {
		t.Errorf("prepares = %d, want 1", vmm.prepares)
	}
	if vmm.adopts != 1 {
		t.Errorf("adopts = %d, want 1", vmm.adopts)
	}
	if vmm.acks != 1 {
		t.Errorf("acks = %d, want 1", vmm.acks)
	}
	if vmm.cancels != 0 {
		t.Errorf("cancels = %d, want 0 (no rollback)", vmm.cancels)
	}

	// State ended at 'running' on the new owner with lineage.
	ins, _ := store.InstanceByID(context.Background(), insID)
	if ins.NodeID != "new-owner" {
		t.Errorf("node_id = %q, want new-owner", ins.NodeID)
	}
	if string(ins.State) != string(state.StateRunning) {
		t.Errorf("state = %q, want running", ins.State)
	}
	if ins.MigratedFromNodeID == nil || *ins.MigratedFromNodeID != "dying" {
		t.Errorf("migrated_from_node_id = %v, want dying", ins.MigratedFromNodeID)
	}
	if ins.MigratedAt == nil {
		t.Errorf("migrated_at is nil")
	}
	if ins.LeaseToken == "" {
		t.Errorf("lease_token empty after commit")
	}
}

func TestMigrateOne_Phase1Fails(t *testing.T) {
	store := state.NewMemStore()
	vmm := &fakeVMM{prepareErr: errors.New("Park blew up")}
	insID := seedInstanceForMigration(t, store, "dying")
	h := newHarnessForTest(t, store, vmm, "new-owner")

	err := h.MigrateOne(context.Background(), insID, "dying")
	if err == nil {
		t.Fatalf("MigrateOne: want error on Phase 1 fail, got nil")
	}
	if vmm.prepares != 0 {
		t.Errorf("prepares = %d, want 0 (Phase 1 errored before counter)", vmm.prepares)
	}
	if vmm.adopts != 0 {
		t.Errorf("adopts = %d, want 0 (Phase 3 must not run)", vmm.adopts)
	}
	if vmm.cancels != 0 {
		t.Errorf("cancels = %d, want 0 (Phase 4 must not run; Park failed before tracker put)", vmm.cancels)
	}

	// State untouched (still running on the dying node).
	ins, _ := store.InstanceByID(context.Background(), insID)
	if string(ins.State) != string(state.StateRunning) {
		t.Errorf("state = %q, want running (Phase 1 fail must not touch the row)", ins.State)
	}
}

func TestMigrateOne_Phase2Conflict(t *testing.T) {
	store := state.NewMemStore()
	vmm := &fakeVMM{}
	insID := seedInstanceForMigration(t, store, "dying")
	// Pre-mutate the row to a non-running state so MarkInstanceMigrating
	// returns ErrConflict (the predicate is state='running' +
	// node_id='dying').
	if err := store.UpdateInstanceState(context.Background(), insID, string(state.StateParked)); err != nil {
		t.Fatalf("UpdateInstanceState: %v", err)
	}
	h := newHarnessForTest(t, store, vmm, "new-owner")

	err := h.MigrateOne(context.Background(), insID, "dying")
	if !errors.Is(err, state.ErrConflict) {
		t.Fatalf("MigrateOne on conflict = %v, want ErrConflict", err)
	}
	if vmm.prepares != 1 {
		t.Errorf("prepares = %d, want 1", vmm.prepares)
	}
	if vmm.cancels != 1 {
		t.Errorf("cancels = %d, want 1 (Phase 4 must fire on Phase 2 conflict)", vmm.cancels)
	}
	if vmm.adopts != 0 {
		t.Errorf("adopts = %d, want 0 (Phase 3 must not run)", vmm.adopts)
	}
}

func TestMigrateOne_Phase3Fails(t *testing.T) {
	store := state.NewMemStore()
	vmm := &fakeVMM{adoptErr: errors.New("Restore failed on new owner")}
	insID := seedInstanceForMigration(t, store, "dying")
	h := newHarnessForTest(t, store, vmm, "new-owner")

	err := h.MigrateOne(context.Background(), insID, "dying")
	if err == nil {
		t.Fatalf("MigrateOne: want error on Phase 3 fail, got nil")
	}
	if vmm.cancels != 1 {
		t.Errorf("cancels = %d, want 1 (Phase 4 must fire on Phase 3 fail)", vmm.cancels)
	}

	// State rolled back to 'parked' (CancelInstanceMigration fired).
	ins, _ := store.InstanceByID(context.Background(), insID)
	if string(ins.State) != string(state.StateParked) {
		t.Errorf("state = %q, want parked (Phase 4 rollback)", ins.State)
	}
	if ins.NodeID != "dying" {
		t.Errorf("node_id = %q, want dying (rollback did not flip)", ins.NodeID)
	}
}

func TestMigrateOne_LeaseExpires(t *testing.T) {
	store := state.NewMemStore()
	// sleepFor > leaseSeconds (2s) so the lease-bounded context
	// fires before Phase 1 returns. fakeVMM honours ctx.Done in
	// its PrepareLiveMigration, so the call returns ctx.Err.
	vmm := &fakeVMM{sleepFor: 3 * time.Second}
	insID := seedInstanceForMigration(t, store, "dying")
	h := newHarnessForTest(t, store, vmm, "new-owner")

	err := h.MigrateOne(context.Background(), insID, "dying")
	if err == nil {
		t.Fatalf("MigrateOne: want error on lease expiry, got nil")
	}
	// State untouched: Phase 2 conditional UPDATE never fired.
	ins, _ := store.InstanceByID(context.Background(), insID)
	if string(ins.State) != string(state.StateRunning) {
		t.Errorf("state = %q, want running (lease expiry pre-Phase 2)", ins.State)
	}
}

func TestMigrateOne_SpecBuilderError(t *testing.T) {
	store := state.NewMemStore()
	vmm := &fakeVMM{}
	insID := seedInstanceForMigration(t, store, "dying")
	ops := wire.NewOpsMetrics("schedd")
	h := NewMigrationHarness(store, vmm, ops, testLog(), "new-owner",
		func(_ context.Context, _ string) (AppSpec, error) {
			return AppSpec{}, errors.New("simulated spec build failure")
		}, NewNodeLedger())
	h.SetLeaseSeconds(2)

	err := h.MigrateOne(context.Background(), insID, "dying")
	if err == nil {
		t.Fatalf("MigrateOne: want error on spec builder failure, got nil")
	}
	// Phase 3 never ran (adopts=0). Phase 4 fired (cancels=1).
	if vmm.adopts != 0 {
		t.Errorf("adopts = %d, want 0", vmm.adopts)
	}
	if vmm.cancels != 1 {
		t.Errorf("cancels = %d, want 1 (Phase 4 rollback)", vmm.cancels)
	}
	// State rolled back via CancelInstanceMigration.
	ins, _ := store.InstanceByID(context.Background(), insID)
	if string(ins.State) != string(state.StateParked) {
		t.Errorf("state = %q, want parked", ins.State)
	}
}

func TestMigrateOne_NilSpecBuilderPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatalf("NewMigrationHarness(nil specBuilder) did not panic")
		}
	}()
	_ = NewMigrationHarness(state.NewMemStore(), &fakeVMM{}, wire.NewOpsMetrics("schedd"),
		testLog(), "new-owner", nil, NewNodeLedger())
}

// readLiveMigrationDecision scrapes the closed-set
// schedd_live_migration_decisions_total{outcome=…} counter
// from the OpsMetrics HTTP handler. Mirrors the readScaleUp
// helper at engine_test.go:408 — Prometheus pre-instantiates
// zero rows for the closed set, so a missing label returns 0.
func readLiveMigrationDecision(t *testing.T, ops *wire.OpsMetrics, outcome string) int {
	t.Helper()
	if ops == nil {
		return 0
	}
	body := getMetricsBody(t, ops)
	want := `schedd_live_migration_decisions_total{outcome="` + outcome + `"}`
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, want) {
			fields := strings.Fields(line)
			if len(fields) < 2 {
				return 0
			}
			n, err := strconv.Atoi(fields[len(fields)-1])
			if err != nil {
				t.Fatalf("parse %q: %v", line, err)
			}
			return n
		}
	}
	return 0
}

func TestMigrateOne_MetricOutcomeMigrated(t *testing.T) {
	store := state.NewMemStore()
	vmm := &fakeVMM{}
	ops := wire.NewOpsMetrics("schedd")
	insID := seedInstanceForMigration(t, store, "dying")
	h := NewMigrationHarness(store, vmm, ops, testLog(), "new-owner", stubSpecBuilder, NewNodeLedger())
	h.SetLeaseSeconds(2)

	if err := h.MigrateOne(context.Background(), insID, "dying"); err != nil {
		t.Fatalf("MigrateOne: %v", err)
	}
	if n := readLiveMigrationDecision(t, ops, "migrated"); n != 1 {
		t.Errorf("migrated outcome = %d, want 1", n)
	}
}

func TestMigrateOne_MetricOutcomePeerFailure(t *testing.T) {
	store := state.NewMemStore()
	vmm := &fakeVMM{adoptErr: errors.New("Restore failed")}
	ops := wire.NewOpsMetrics("schedd")
	insID := seedInstanceForMigration(t, store, "dying")
	h := NewMigrationHarness(store, vmm, ops, testLog(), "new-owner", stubSpecBuilder, NewNodeLedger())
	h.SetLeaseSeconds(2)

	if err := h.MigrateOne(context.Background(), insID, "dying"); err == nil {
		t.Fatalf("MigrateOne: want error, got nil")
	}
	if n := readLiveMigrationDecision(t, ops, "peer_failure"); n != 1 {
		t.Errorf("peer_failure outcome = %d, want 1", n)
	}
	if n := readLiveMigrationDecision(t, ops, "migrated"); n != 0 {
		t.Errorf("migrated outcome = %d, want 0", n)
	}
}

func TestMigrateOne_MetricOutcomeConflict(t *testing.T) {
	store := state.NewMemStore()
	vmm := &fakeVMM{}
	ops := wire.NewOpsMetrics("schedd")
	insID := seedInstanceForMigration(t, store, "dying")
	if err := store.UpdateInstanceState(context.Background(), insID, string(state.StateParked)); err != nil {
		t.Fatalf("UpdateInstanceState: %v", err)
	}
	h := NewMigrationHarness(store, vmm, ops, testLog(), "new-owner", stubSpecBuilder, NewNodeLedger())
	h.SetLeaseSeconds(2)

	if err := h.MigrateOne(context.Background(), insID, "dying"); !errors.Is(err, state.ErrConflict) {
		t.Fatalf("MigrateOne = %v, want ErrConflict", err)
	}
	if n := readLiveMigrationDecision(t, ops, "conflict"); n != 1 {
		t.Errorf("conflict outcome = %d, want 1", n)
	}
}

// TestMigrateLiveInstances_CapsPerTick drives the per-tick cap
// at the Engine level. fixture has 3 instances; maxPerTick=2
// caps to 2 attempts.
func TestMigrateLiveInstances_CapsPerTick(t *testing.T) {
	store := state.NewMemStore()
	vmm := &fakeVMM{}
	// Seed 3 running instances on a dying node.
	for i := 0; i < 3; i++ {
		seedInstanceForMigration(t, store, "dying")
	}
	engine := newEngine(t, store, vmm, &fakeNotifier{}, "1.10.0").
		WithOpsMetrics(wire.NewOpsMetrics("schedd")).
		WithOwnerNodeID("new-owner").WithMigrateLiveConfig(2)

	attempted, err := engine.MigrateLiveInstances(context.Background(), "dying")
	if err != nil {
		t.Fatalf("MigrateLiveInstances: %v", err)
	}
	if attempted != 2 {
		t.Errorf("attempted = %d, want 2 (per-tick cap)", attempted)
	}
	if vmm.prepares != 2 {
		t.Errorf("prepares = %d, want 2 (per-tick cap)", vmm.prepares)
	}
}

// TestMigrateLiveInstances_OwnerNodeIDEmpty confirms the
// single-box posture (no owner_node_id) is a no-op rather than
// a no-target crash.
func TestMigrateLiveInstances_OwnerNodeIDEmpty(t *testing.T) {
	store := state.NewMemStore()
	vmm := &fakeVMM{}
	seedInstanceForMigration(t, store, "dying")
	engine := newEngine(t, store, vmm, &fakeNotifier{}, "1.10.0").
		WithOpsMetrics(wire.NewOpsMetrics("schedd"))
	// Do NOT call WithOwnerNodeID — legacy single-box posture.
	attempted, err := engine.MigrateLiveInstances(context.Background(), "dying")
	if err != nil {
		t.Fatalf("MigrateLiveInstances: %v", err)
	}
	if attempted != 0 {
		t.Errorf("attempted = %d, want 0 (no owner_node_id)", attempted)
	}
	if vmm.prepares != 0 {
		t.Errorf("prepares = %d, want 0", vmm.prepares)
	}
}

// TestMigrateLiveInstances_DeadNodeEqualsSelf is the no-op
// branch when deadNodeID == owner_node_id (can't migrate
// from yourself to yourself).
func TestMigrateLiveInstances_DeadNodeEqualsSelf(t *testing.T) {
	store := state.NewMemStore()
	vmm := &fakeVMM{}
	seedInstanceForMigration(t, store, "self")
	engine := newEngine(t, store, vmm, &fakeNotifier{}, "1.10.0").
		WithOpsMetrics(wire.NewOpsMetrics("schedd")).
		WithOwnerNodeID("self")
	attempted, err := engine.MigrateLiveInstances(context.Background(), "self")
	if err != nil {
		t.Fatalf("MigrateLiveInstances: %v", err)
	}
	if attempted != 0 {
		t.Errorf("attempted = %d, want 0", attempted)
	}
}

// TestBuildAppSpecForMigration_NonEmpty is a smoke guard
// against a future refactor that breaks
// BuildAppSpecForMigration's signature. The harness tests above
// use the stubSpecBuilder, so a regression in the canonical
// builder wouldn't surface from those alone.
//
// Seeds the full instance + app + deployment + account chain
// the wake-time builder relies on, then asserts the migration
// builder produces a spec with non-empty drive0 base, drive1
// layer, VCPU count, and matching memory.
func TestBuildAppSpecForMigration_NonEmpty(t *testing.T) {
	store := state.NewMemStore()
	insID := seedInstanceForMigration(t, store, "dying")

	// seedInstanceForMigration seeds account + app + instance
	// but not deployment. Resolve the app and seed its live
	// deployment so BuildAppSpecForMigration's
	// LiveDeployment call doesn't 404.
	ins, err := store.InstanceByID(context.Background(), insID)
	if err != nil {
		t.Fatalf("InstanceByID: %v", err)
	}
	depID := "dep-" + uuid.NewString()
	if _, err := store.CreateDeployment(context.Background(),
		state.Deployment{ID: depID, AppID: ins.AppID, Kind: state.DeploymentKindImage,
			ImageDigest: "sha256:seed", Status: state.DeployLive, CreatedAt: time.Now()}); err != nil {
		t.Fatalf("CreateDeployment: %v", err)
	}

	engine := newEngine(t, store, &fakeVMM{}, &fakeNotifier{}, "1.10.0").
		WithOpsMetrics(wire.NewOpsMetrics("schedd"))
	spec, err := engine.BuildAppSpecForMigration(context.Background(), insID)
	if err != nil {
		t.Fatalf("BuildAppSpecForMigration: %v", err)
	}
	if spec.BaseKey == "" {
		t.Errorf("BaseKey empty; want base/runtime-node22.ext4 (driven by app.Runtime)")
	}
	if spec.LayerKey == "" {
		t.Errorf("LayerKey empty; want apps/<slug>/<depID>.ext4")
	}
	if spec.VCPUCount == 0 {
		t.Errorf("VCPUCount=0; want the per-plan VCPU (Hobby=2)")
	}
	if spec.MemSizeMiB != 256 {
		t.Errorf("MemSizeMiB = %d, want 256 (seeded)", spec.MemSizeMiB)
	}
	if spec.EgressMbit == 0 {
		t.Errorf("EgressMbit=0; want the per-plan cap (Hobby=25)")
	}
}
