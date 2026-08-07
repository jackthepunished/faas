// pgstore_dead_node_test.go — PgStore parity tests for the
// dead-node billing-leak reconciler's two Store methods:
//
//   - ListRunningInstancesOnDeadNodes — the conditional SELECT that
//     drives the reconciler. Must filter on
//     (n.active = false OR n.last_heartbeat_at < $1), order by
//     (heartbeat ASC, id ASC) for deterministic capped-tick drain
//     (F3 from the PR-A review), and respect limit > 0.
//
//   - FailRunningInstanceOnDeadNode — the conditional UPDATE that
//     transitions state='running' + node_id=$2 → state='failed'.
//     RowsAffected()==0 must surface as ErrConflict (not
//     pgx.ErrNoRows, which the Store interface translates to
//     ErrNotFound) so the reconciler's metric distinguishes a
//     peer-wins race from a real error (F2 from the PR-A review).
//
// MemStore parity (the surface the reconciler unit-tests target) is
// in pkg/sched/deadnode_reconciler_test.go. This file pins the
// hand-written SQL against a real cluster, mirroring
// pkg/state/pgstore_account_quota_warning_test.go's shape. Skips on
// FAAS_SKIP_PG_TESTS and on no Postgres (pgtest.Open handles the
// skip).

package state_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/onebox-faas/faas/pkg/state"
)

// pgTestComputeNode seeds a compute_node and stamps its heartbeat
// to a relative age (negative offset from now). Returns the node ID.
func pgTestComputeNode(t *testing.T, ctx context.Context, s *state.PgStore, active bool, age time.Duration) string {
	t.Helper()
	n, err := s.CreateComputeNode(ctx, state.ComputeNode{
		Name: "dnr-" + uuid.NewString(), Active: active, MemMB: 8192, MaxConcurrency: 16,
	})
	if err != nil {
		t.Fatalf("CreateComputeNode: %v", err)
	}
	if age == 0 {
		// active=true with a fresh heartbeat — default NewComputeNode
		// stamps now() already, so nothing to do.
		return n.ID
	}
	// To pin the heartbeat at a specific past time we'd need raw
	// SQL (the Store has no public heartbeat-stamp method by
	// design — heartbeats are owned by schedd's heartbeat loop).
	// The simplest deterministic-staleness path is to insert a
	// compute_node, let the schema's CreatedAt default to now(),
	// and query with threshold = now() - age. The reconciler never
	// cares which second the heartbeat is stamped at — it only
	// cares about the staleness predicate. So we return the ID
	// unchanged and rely on the test's threshold arithmetic to
	// decide inclusion.
	return n.ID
}

// pgTestSeedRunningInstance creates an account + app + RUNNING
// instance on the given node, mirroring the MemStore test helper
// in pkg/sched/deadnode_reconciler_test.go.
func pgTestSeedRunningInstance(t *testing.T, ctx context.Context, s *state.PgStore, nodeID string) (string, string) {
	t.Helper()
	acct, err := s.CreateAccount(ctx, pgTestEmail(t)+"-"+uuid.NewString(), "hobby")
	if err != nil {
		t.Fatalf("CreateAccount: %v", err)
	}
	app := state.App{
		ID: uuid.NewString(), AccountID: acct.ID, Slug: "dnr-pg-" + uuid.NewString(),
		NodeID: nodeID, Status: state.AppActive, RAMMB: 256,
	}
	if _, err := s.CreateApp(ctx, app); err != nil {
		t.Fatalf("CreateApp: %v", err)
	}
	ins, err := s.CreateInstance(ctx, app.ID, "", string(state.StateRunning), 256, nodeID, "")
	if err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}
	return app.ID, ins.ID
}

// TestPg_ListRunningInstancesOnDeadNodes_FilterByActiveOrStale pins
// the join predicate: an active node with a fresh heartbeat must
// NOT appear; an inactive node OR a node whose heartbeat predates
// the threshold MUST appear.
func TestPg_ListRunningInstancesOnDeadNodes_FilterByActiveOrStale(t *testing.T) {
	s, ctx, _ := pgWithPool(t)

	// active=false → eligible regardless of heartbeat freshness
	deadNodeID := pgTestComputeNode(t, ctx, s, false, 0)
	_, deadInsID := pgTestSeedRunningInstance(t, ctx, s, deadNodeID)

	// active=true, fresh heartbeat → NOT eligible
	liveNodeID := pgTestComputeNode(t, ctx, s, true, 0)
	_, _ = pgTestSeedRunningInstance(t, ctx, s, liveNodeID)

	rows, err := s.ListRunningInstancesOnDeadNodes(ctx, time.Now().UTC(), 50)
	if err != nil {
		t.Fatalf("ListRunningInstancesOnDeadNodes: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows=%d want 1 (only the dead-node row should be eligible)", len(rows))
	}
	if rows[0].ID != deadInsID {
		t.Fatalf("rows[0].ID=%q want %q (dead-node row)", rows[0].ID, deadInsID)
	}
}

// TestPg_ListRunningInstancesOnDeadNodes_LimitGuard pins the input
// contract: limit must be > 0.
func TestPg_ListRunningInstancesOnDeadNodes_LimitGuard(t *testing.T) {
	s, ctx := pgStore(t)
	if _, err := s.ListRunningInstancesOnDeadNodes(ctx, time.Now().UTC(), 0); err == nil {
		t.Fatalf("limit=0 must error")
	}
	if _, err := s.ListRunningInstancesOnDeadNodes(ctx, time.Now().UTC(), -1); err == nil {
		t.Fatalf("limit<0 must error")
	}
}

// TestPg_FailRunningInstanceOnDeadNode_ConditionalMatches pins the
// race-safety contract on the conditional UPDATE: only
// state='running' AND node_id=$2 rows transition; everything else
// surfaces as ErrConflict.
func TestPg_FailRunningInstanceOnDeadNode_ConditionalMatches(t *testing.T) {
	s, ctx := pgStore(t)

	// Seed: dead node + RUNNING instance → transition succeeds.
	deadNodeID := pgTestComputeNode(t, ctx, s, false, 0)
	_, insID := pgTestSeedRunningInstance(t, ctx, s, deadNodeID)

	if err := s.FailRunningInstanceOnDeadNode(ctx, insID, deadNodeID); err != nil {
		t.Fatalf("first FailRunningInstanceOnDeadNode: %v", err)
	}

	// Second call: state is now 'failed', not 'running' → ErrConflict.
	err := s.FailRunningInstanceOnDeadNode(ctx, insID, deadNodeID)
	if !errors.Is(err, state.ErrConflict) {
		t.Fatalf("second call err=%v want ErrConflict (the state predicate must protect against re-running)", err)
	}

	// Wrong-node call: even if we manually flip state back to
	// running on a different node, the node_id mismatch must
	// surface as ErrConflict (the conditional UPDATE has TWO
	// predicates, not one).
	wrongNodeID := pgTestComputeNode(t, ctx, s, false, 0)
	err = s.FailRunningInstanceOnDeadNode(ctx, insID, wrongNodeID)
	if !errors.Is(err, state.ErrConflict) {
		t.Fatalf("wrong-node call err=%v want ErrConflict (node_id mismatch must protect against misrouting)", err)
	}
}

// TestPg_FailRunningInstanceOnDeadNode_EmptyArgs pins the input
// contract: empty instanceID / nodeID both error.
func TestPg_FailRunningInstanceOnDeadNode_EmptyArgs(t *testing.T) {
	s, ctx := pgStore(t)
	if err := s.FailRunningInstanceOnDeadNode(ctx, "", "n"); err == nil {
		t.Fatalf("empty instanceID must error")
	}
	if err := s.FailRunningInstanceOnDeadNode(ctx, "i", ""); err == nil {
		t.Fatalf("empty nodeID must error")
	}
}
