// Engine-level tests for the workload-OOM-triggered destroy path
// (Cluster C / ADR-121). These tests pin the AC surface that
// mirrors the liveness tests (engine_liveness_test.go) but for
// the runtime-OOM producer chain:
//
//   - AC #1 — workload OOM stamps CodeAppRuntimeOOM on the
//     deployment with the whycopy Observed payload (peakMB /
//     planMB templated into Hint/Why/Fix).
//
//   - AC #2 — snapshots are NEVER restored after a workload OOM
//     (mirrors ADR-005 invariant): pinned via
//     TestWorkloadOOM_StaleSnapOnDestroy.
//
//   - AC #3 — `vmm_workload_oom_kills_total{app, deployment}`
//     metric emitted: pinned via
//     TestWorkloadOOM_RestartCounterIncrement.
//
//   - AC #4 — non-RUNNING instances are skipped (idempotency
//     against duplicate relay races): pinned via
//     TestWorkloadOOM_SkipsNonRunning.
//
// The Engine has many moving parts (Wake/Park/Transition/
// ledger/etc); we keep these tests tightly scoped to the
// workload-OOM path and use a deliberately small MemStore +
// fakeVMM, mirroring engine_liveness_test.go.
package sched

import (
	"context"
	"strings"
	"testing"

	"github.com/onebox-faas/faas/pkg/api"
	"github.com/onebox-faas/faas/pkg/state"
	"github.com/onebox-faas/faas/pkg/wire"
)

// TestWorkloadOOM_StampsAppRuntimeOOM (Cluster C / ADR-121, AC #1)
// asserts the engine destroy path stamps the deployment row with
// CodeAppRuntimeOOM and the whycopy Observed payload templated
// from (peakMB, planMB). The customer-facing surface is the
// dashboard's `.error-explanation` section + `gregale inspect
// <slug> --errors`.
//
// We assert the prose strings here because they're the customer
// contract — the whycopy catalog row's Observed closure is the
// source of truth, and the engine just wires it through.
func TestWorkloadOOM_StampsAppRuntimeOOM(t *testing.T) {
	store := state.NewMemStore()
	_, app, dep := seedApp(t, store, api.PlanPro, 512, 5)
	vmm := &fakeVMM{}
	ops := wire.NewOpsMetrics("schedd")
	engine := newEngine(t, store, vmm, &fakeNotifier{}, "1.10.0").WithOpsMetrics(ops)
	inst := runningInstance(t, store, app, dep, vmm, engine)

	if err := engine.DestroyForWorkloadOOMFailure(context.Background(), inst.ID, 384, 256); err != nil {
		t.Fatalf("DestroyForWorkloadOOMFailure: %v", err)
	}

	got, err := store.DeploymentByID(context.Background(), dep.ID)
	if err != nil {
		t.Fatalf("DeploymentByID: %v", err)
	}
	if got.ErrorCode != api.CodeAppRuntimeOOM {
		t.Errorf("deployment.ErrorCode = %q, want %q", got.ErrorCode, api.CodeAppRuntimeOOM)
	}
	// The whycopy Observed closure templates peakMB=384 into the
	// Why string and "plan 256 MB → at least 392 MB" into the Fix
	// string.
	if !strings.Contains(got.ErrorWhy, "384 MB") {
		t.Errorf("deployment.ErrorWhy missing peak MB; got %q", got.ErrorWhy)
	}
	if !strings.Contains(got.ErrorWhy, "256 MB + 8 MB overhead") {
		t.Errorf("deployment.ErrorWhy missing plan cap + 8 MB overhead; got %q", got.ErrorWhy)
	}
	if !strings.Contains(got.ErrorFix, "256 MB plan") {
		t.Errorf("deployment.ErrorFix missing source plan; got %q", got.ErrorFix)
	}
	if !strings.Contains(got.ErrorFix, "at least 392 MB") {
		t.Errorf("deployment.ErrorFix missing recommended plan (peak + 8 MB); got %q", got.ErrorFix)
	}
	if got.ErrorHint == "" {
		t.Errorf("deployment.ErrorHint is empty; customer sees no next-action line")
	}
}

// TestWorkloadOOM_StaleSnapOnDestroy (Cluster C / ADR-121, AC #2)
// asserts the workload OOM path eagerly marks the deployment's
// latest snapshot stale (mirrors ADR-005 invariant). The next
// Wake cold-boots; without this the workload-OOM blast radius
// would persist on the next restore.
func TestWorkloadOOM_StaleSnapOnDestroy(t *testing.T) {
	store := state.NewMemStore()
	_, app, dep := seedApp(t, store, api.PlanPro, 512, 5)
	vmm := &fakeVMM{}
	ops := wire.NewOpsMetrics("schedd")
	engine := newEngine(t, store, vmm, &fakeNotifier{}, "1.10.0").WithOpsMetrics(ops)
	// Seed a snapshot on the deployment — DestroyForWorkloadOOMFailure
	// must flip stale=true.
	_, err := store.CreateSnapshot(context.Background(), state.Snapshot{
		DeploymentID: dep.ID, FCVersion: "1.10.0",
		MemBytes: 1 << 20, DiskBytes: 1 << 20, StorageKey: "/tmp/snap",
		Tier: state.SnapshotTierInit,
	})
	if err != nil {
		t.Fatalf("CreateSnapshot: %v", err)
	}
	pre, err := store.LatestSnapshotForTier(context.Background(), dep.ID, state.SnapshotTierInit)
	if err != nil {
		t.Fatalf("LatestSnapshotForTier (pre): %v", err)
	}
	if pre.Stale {
		t.Errorf("snapshot.Stale = true pre-destroy, want false")
	}
	inst := runningInstance(t, store, app, dep, vmm, engine)

	if err := engine.DestroyForWorkloadOOMFailure(context.Background(), inst.ID, 384, 256); err != nil {
		t.Fatalf("DestroyForWorkloadOOMFailure: %v", err)
	}
	// Post-destroy: LatestSnapshotForTier returns ErrNotFound
	// because the only matching row is now stale.
	_, err = store.LatestSnapshotForTier(context.Background(), dep.ID, state.SnapshotTierInit)
	if err == nil {
		t.Errorf("LatestSnapshotForTier (post) = nil, want ErrNotFound (AC #2: stale flag flipped)")
	}
	// Instance row must be STOPPED — the destroy succeeded.
	final, err := store.InstanceByID(context.Background(), inst.ID)
	if err != nil {
		t.Fatalf("InstanceByID: %v", err)
	}
	if state.State(final.State) != state.StateStopped {
		t.Errorf("instance.State = %q, want %q", final.State, state.StateStopped)
	}
}

// TestWorkloadOOM_RestartCounterIncrement (Cluster C / ADR-121,
// AC #3) — the vmm_workload_oom_kills_total{app, deployment}
// counter increments exactly once per successful
// DestroyForWorkloadOOMFailure. Mirrors the liveness counter test.
func TestWorkloadOOM_RestartCounterIncrement(t *testing.T) {
	store := state.NewMemStore()
	_, app, dep := seedApp(t, store, api.PlanPro, 512, 5)
	vmm := &fakeVMM{}
	ops := wire.NewOpsMetrics("schedd")
	engine := newEngine(t, store, vmm, &fakeNotifier{}, "1.10.0").WithOpsMetrics(ops)
	inst := runningInstance(t, store, app, dep, vmm, engine)
	counter := ops.WorkloadOOMKills(app.ID, dep.ID)
	before := readCounterValue(t, counter)
	if err := engine.DestroyForWorkloadOOMFailure(context.Background(), inst.ID, 384, 256); err != nil {
		t.Fatalf("DestroyForWorkloadOOMFailure: %v", err)
	}
	after := readCounterValue(t, counter)
	if after != before+1 {
		t.Errorf("counter delta = %v, want 1 (AC #3: increment on every destroy)", after-before)
	}
}

// TestWorkloadOOM_SkipsNonRunning (Cluster C / ADR-121, AC #4)
// asserts the engine does not stamp the deployment row when the
// instance is not currently RUNNING. This is the idempotency
// protection against a duplicate relay race (e.g. vmmd emits
// twice for the same OOM; the second call sees STOPPED).
//
// We deliberately test the "no stamp" path because the
// `Deployment` row's existing ErrorCode is the customer-visible
// signal — a duplicate stamp would overwrite the first with the
// same code, which is benign, but skips the stale-snap + counter
// path so the test has to seed the SID the right way.
func TestWorkloadOOM_SkipsNonRunning(t *testing.T) {
	store := state.NewMemStore()
	_, app, dep := seedApp(t, store, api.PlanPro, 512, 5)
	vmm := &fakeVMM{}
	ops := wire.NewOpsMetrics("schedd")
	engine := newEngine(t, store, vmm, &fakeNotifier{}, "1.10.0").WithOpsMetrics(ops)
	// Create an instance in WARMING (not RUNNING) — the destroy
	// path must skip the stamp + counter.
	inst, err := store.CreateInstance(context.Background(), app.ID, dep.ID, string(state.StateWaking), 512, state.DefaultLocalNodeName, "")
	if err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}
	counter := ops.WorkloadOOMKills(app.ID, dep.ID)
	before := readCounterValue(t, counter)

	if err := engine.DestroyForWorkloadOOMFailure(context.Background(), inst.ID, 384, 256); err != nil {
		t.Fatalf("DestroyForWorkloadOOMFailure: %v", err)
	}

	// Counter must NOT have incremented.
	after := readCounterValue(t, counter)
	if after != before {
		t.Errorf("counter delta = %v, want 0 (AC #4: non-RUNNING is skipped)", after-before)
	}
	// Deployment ErrorCode must remain empty.
	got, err := store.DeploymentByID(context.Background(), dep.ID)
	if err != nil {
		t.Fatalf("DeploymentByID: %v", err)
	}
	if got.ErrorCode != "" {
		t.Errorf("deployment.ErrorCode = %q, want empty (AC #4: no stamp on non-RUNNING)", got.ErrorCode)
	}
}
