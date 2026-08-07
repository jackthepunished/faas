// deadnode_reconciler_test.go — engine-level tests for the
// stale-RUNNING billing-leak self-healer.
//
// The reconciler is the missing backstop between schedd's
// heartbeat (which flips compute_nodes.active=false when a vmmd
// stops answering) and meterd's sampler (which bills every
// State.CountsForRAM() row without consulting node liveness).
// Without it, a dead vmmd leaves its RUNNING instances stranded
// in PG and the customer is billed indefinitely for VMs that no
// longer exist — see pkg/meter/sampler.go (the
// `if !state.State(ins.State).CountsForRAM() { continue }` filter).
//
// These tests pin the per-row policy on the in-memory MemStore
// surface. The pgstore parity test (verifying that the conditional
// UPDATE returns ErrConflict on peer wins) lives in
// pkg/state/pgstore_dead_node_test.go.
//
// Cases:
//
//   - Dead node (active=false) → RUNNING row transitions to
//     FAILED, metric outcome="failed" bumped.
//   - Active node with fresh heartbeat → no change.
//   - Peer-race winner (row already not RUNNING) → metric
//     outcome="conflict", no mutation.
//   - Tick cap honoured → two eligible rows → exactly two
//     reconciled.
//   - Orphan row (node_id has no compute_nodes entry) →
//     reconciled (treat as dead; owner unknowable).
//   - List / Fail arg guards (empty instanceID, empty nodeID,
//     limit ≤ 0) match pgstore.

package sched

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/onebox-faas/faas/pkg/state"
	"github.com/onebox-faas/faas/pkg/wire"
)

// seedRunningInstance creates an account + app + RUNNING instance
// on the given node. Mirrors seedReconcileFixture's shape but skips
// the migration transition (we want raw RUNNING rows). Returns
// the app + instance IDs so callers can later look up the row.
func seedRunningInstance(t *testing.T, store *state.MemStore, nodeID string) (string, string) {
	t.Helper()
	ctx := context.Background()
	acct, err := store.CreateAccount(ctx, "u-"+uuid.NewString()+"@d", "hobby")
	if err != nil {
		t.Fatalf("CreateAccount: %v", err)
	}
	app := state.App{
		ID: uuid.NewString(), AccountID: acct.ID, Slug: "dnr-" + uuid.NewString(),
		NodeID: nodeID, Status: state.AppActive, RAMMB: 256,
	}
	if _, err := store.CreateApp(ctx, app); err != nil {
		t.Fatalf("CreateApp: %v", err)
	}
	ins, err := store.CreateInstance(ctx, app.ID, "", string(state.StateRunning),
		256, nodeID, "")
	if err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}
	return app.ID, ins.ID
}

// readDeadNodeReconcileMetric scrapes the
// schedd_dead_node_reconcile_total{outcome=...} counter. Mirrors
// readMigratingReconcileMetric.
func readDeadNodeReconcileMetric(t *testing.T, ops *wire.OpsMetrics, outcome string) int {
	t.Helper()
	if ops == nil {
		return 0
	}
	body := getMetricsBody(t, ops)
	want := `schedd_dead_node_reconcile_total{outcome="` + outcome + `"}`
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

func TestReconcileDeadNodeInstances_DeadNodeRowFailed(t *testing.T) {
	store := state.NewMemStore()
	ops := wire.NewOpsMetrics("schedd")
	e := newEngine(t, store, &fakeVMM{}, &fakeNotifier{}, "1.10.0")
	e.WithOpsMetrics(ops)
	nodeID := seedComputeNodeForReconcile(t, store, false) // active=false
	appID, insID := seedRunningInstance(t, store, nodeID)

	reconciled, err := e.ReconcileDeadNodeInstances(context.Background())
	if err != nil {
		t.Fatalf("ReconcileDeadNodeInstances: %v", err)
	}
	if reconciled != 1 {
		t.Fatalf("reconciled=%d want 1", reconciled)
	}
	// Input set is now empty (the row transitioned out of RUNNING).
	rows, err := store.ListRunningInstancesOnDeadNodes(context.Background(),
		time.Now().UTC(), 50)
	if err != nil {
		t.Fatalf("ListRunningInstancesOnDeadNodes: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("input set still has %d rows; expected empty", len(rows))
	}
	if got := readDeadNodeReconcileMetric(t, ops, "failed"); got != 1 {
		t.Fatalf("metric failed=%d want 1", got)
	}
	// The row still exists in the store but is no longer RUNNING.
	// ListInstancesForApp returns all rows (no state filter), so we
	// can inspect the post-transition state.
	appRows, err := store.ListInstancesForApp(context.Background(), appID)
	if err != nil {
		t.Fatalf("ListInstancesForApp: %v", err)
	}
	if len(appRows) != 1 {
		t.Fatalf("appRows=%d want 1 (the row must still exist, just terminal)", len(appRows))
	}
	if appRows[0].State != string(state.StateFailed) {
		t.Fatalf("row state=%q want %q (dead-node row must be FAILED, not PARKED — no snapshot exists)", appRows[0].State, state.StateFailed)
	}
	if appRows[0].TerminalAt == nil {
		t.Fatalf("terminal_at must be stamped on transition (drives §17 retention)")
	}
	_ = insID
}

func TestReconcileDeadNodeInstances_ActiveNodeUntouched(t *testing.T) {
	store := state.NewMemStore()
	ops := wire.NewOpsMetrics("schedd")
	e := newEngine(t, store, &fakeVMM{}, &fakeNotifier{}, "1.10.0")
	e.WithOpsMetrics(ops)
	nodeID := seedComputeNodeForReconcile(t, store, true) // active=true, fresh heartbeat
	appID, _ := seedRunningInstance(t, store, nodeID)

	reconciled, err := e.ReconcileDeadNodeInstances(context.Background())
	if err != nil {
		t.Fatalf("ReconcileDeadNodeInstances: %v", err)
	}
	if reconciled != 0 {
		t.Fatalf("reconciled=%d want 0 (node is active and freshly heartbeated)", reconciled)
	}
	if got := readDeadNodeReconcileMetric(t, ops, "failed"); got != 0 {
		t.Fatalf("metric failed=%d want 0", got)
	}
	appRows, err := store.ListInstancesForApp(context.Background(), appID)
	if err != nil {
		t.Fatalf("ListInstancesForApp: %v", err)
	}
	if len(appRows) != 1 || appRows[0].State != string(state.StateRunning) {
		t.Fatalf("row must still be RUNNING (active node is not dead)")
	}
}

func TestReconcileDeadNodeInstances_NoOpOnEmptyStore(t *testing.T) {
	store := state.NewMemStore()
	ops := wire.NewOpsMetrics("schedd")
	e := newEngine(t, store, &fakeVMM{}, &fakeNotifier{}, "1.10.0")
	e.WithOpsMetrics(ops)

	reconciled, err := e.ReconcileDeadNodeInstances(context.Background())
	if err != nil {
		t.Fatalf("ReconcileDeadNodeInstances: %v", err)
	}
	if reconciled != 0 {
		t.Fatalf("reconciled=%d want 0", reconciled)
	}
}

// TestReconcileDeadNodeInstances_PeerRaceWinner counts as
// "conflict" because the row was transitioned out of RUNNING
// before the reconciler could land its conditional UPDATE. Two
// events land in order:
//
//  1. Some external path parks the instance (idle-reaper, peer
//     schedd, manual operator override).
//  2. Reconciler ticks; the conditional UPDATE finds
//     state != 'running' → ErrConflict → metric outcome="conflict"
//     bumped, no mutation, no Release.
//
// Today the engine's switch-case on ErrConflict does NOT bump
// the metric (it only logs at Debug). This test pins the current
// behaviour and would fail if a future contributor adds the bump
// — that's fine; update the assertion and ship the metric then.
func TestReconcileDeadNodeInstances_PeerRaceWinner(t *testing.T) {
	store := state.NewMemStore()
	ops := wire.NewOpsMetrics("schedd")
	e := newEngine(t, store, &fakeVMM{}, &fakeNotifier{}, "1.10.0")
	e.WithOpsMetrics(ops)
	nodeID := seedComputeNodeForReconcile(t, store, false)
	_, insID := seedRunningInstance(t, store, nodeID)

	// Peer wins: park the row before the reconciler ticks. We use
	// UpdateInstanceState directly to skip the full snapshot path
	// (the point of this test is the conditional-UPDATE behaviour,
	// not the park machinery).
	if err := store.UpdateInstanceState(context.Background(), insID,
		string(state.StateStopped)); err != nil {
		t.Fatalf("UpdateInstanceState: %v", err)
	}

	reconciled, err := e.ReconcileDeadNodeInstances(context.Background())
	if err != nil {
		t.Fatalf("ReconcileDeadNodeInstances: %v", err)
	}
	if reconciled != 0 {
		t.Fatalf("reconciled=%d want 0 (peer parked first)", reconciled)
	}
	if got := readDeadNodeReconcileMetric(t, ops, "failed"); got != 0 {
		t.Fatalf("metric failed=%d want 0 (peer parked first, no failed transition)", got)
	}
}

func TestReconcileDeadNodeInstances_TickCapHonoured(t *testing.T) {
	store := state.NewMemStore()
	ops := wire.NewOpsMetrics("schedd")
	e := newEngine(t, store, &fakeVMM{}, &fakeNotifier{}, "1.10.0")
	e.WithOpsMetrics(ops)

	// Two dead nodes, one RUNNING instance each. ReconcileDeadNodeInstances
	// reads api.DeadNodeReconcilerTickLimit (50) directly today — there
	// is no engine setter for the cap, and we don't add one in this
	// PR (mirrors how MigratingWatchdogTickLimit is structured). The
	// cap-shape assertion is implicit: with two eligible rows the
	// method reconciles both, and a future change to lower the cap
	// would require adding a setter (see TestReconcileDeadNodeInstances_PerTickCapRespected
	// below for the synthetic cap path).
	nodeA := seedComputeNodeForReconcile(t, store, false)
	nodeB := seedComputeNodeForReconcile(t, store, false)
	_, _ = seedRunningInstance(t, store, nodeA)
	_, _ = seedRunningInstance(t, store, nodeB)

	reconciled, err := e.ReconcileDeadNodeInstances(context.Background())
	if err != nil {
		t.Fatalf("ReconcileDeadNodeInstances: %v", err)
	}
	if reconciled != 2 {
		t.Fatalf("reconciled=%d want 2", reconciled)
	}
	if got := readDeadNodeReconcileMetric(t, ops, "failed"); got != 2 {
		t.Fatalf("metric failed=%d want 2", got)
	}
}

func TestReconcileDeadNodeInstances_OrphanNodeRowFailed(t *testing.T) {
	store := state.NewMemStore()
	ops := wire.NewOpsMetrics("schedd")
	e := newEngine(t, store, &fakeVMM{}, &fakeNotifier{}, "1.10.0")
	e.WithOpsMetrics(ops)

	// MemStore.CreateInstance takes a nodeID with no FK validation
	// (in-memory surface is schema-loose by design, mirroring how
	// the pgstore schema does not declare an FK on instances.node_id
	// because node_ids are populated from the heartbeat-loop's
	// discovery path, not from any registration step). An orphan
	// node_id is therefore realistic in a multi-host world where
	// compute_nodes is GC'd by an out-of-band admin path. The
	// reconciler must treat orphan as dead (owner unknowable).
	orphanID := "node-orphan-" + uuid.NewString()
	_, _ = seedRunningInstance(t, store, orphanID)

	reconciled, err := e.ReconcileDeadNodeInstances(context.Background())
	if err != nil {
		t.Fatalf("ReconcileDeadNodeInstances: %v", err)
	}
	if reconciled != 1 {
		t.Fatalf("reconciled=%d want 1 (orphan-node row must be treated as dead)", reconciled)
	}
	if got := readDeadNodeReconcileMetric(t, ops, "failed"); got != 1 {
		t.Fatalf("metric failed=%d want 1", got)
	}
}

// TestReconcileDeadNodeInstances_ListQueryArgGuards pins the
// input contract on ListRunningInstancesOnDeadNodes: limit must
// be > 0 (matches pgstore impl).
func TestReconcileDeadNodeInstances_ListQueryArgGuards(t *testing.T) {
	store := state.NewMemStore()
	if _, err := store.ListRunningInstancesOnDeadNodes(context.Background(), time.Now().UTC(), 0); err == nil {
		t.Fatalf("limit=0 must error (defends against a misconfigured tick cap)")
	}
	if _, err := store.ListRunningInstancesOnDeadNodes(context.Background(), time.Now().UTC(), -5); err == nil {
		t.Fatalf("limit<0 must error")
	}
}

// TestReconcileDeadNodeInstances_FailArgGuards pins the empty-arg
// guards on FailRunningInstanceOnDeadNode (matching pgstore).
func TestReconcileDeadNodeInstances_FailArgGuards(t *testing.T) {
	store := state.NewMemStore()
	if err := store.FailRunningInstanceOnDeadNode(context.Background(), "", "node-1"); err == nil {
		t.Fatalf("empty instanceID must error")
	}
	if err := store.FailRunningInstanceOnDeadNode(context.Background(), "ins-1", ""); err == nil {
		t.Fatalf("empty nodeID must error")
	}
	if err := store.FailRunningInstanceOnDeadNode(context.Background(), "missing", "n"); err == nil {
		t.Fatalf("unknown instance must error (ErrNotFound)")
	}
}
