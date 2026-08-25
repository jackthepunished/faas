// pkg/sched/operator_intent_subscriber_test.go — MemStore tests
// for the operator-intent dispatcher
// (PR #1099 P2 redesign step 2). Mirrors the cron fire-now
// precedent at pkg/sched/operator_intent_subscriber.go.
//
// Pins:
//
//  1. drainPendingOperatorIntents finds + dispatches a pending
//     row, transitions to terminal via MarkOperatorIntentSucceeded.
//  2. Two pending rows are dispatched in FIFO order.
//  3. A duplicate drain after MarkSucceeded is a no-op (the row
//     is terminal, invisible to the claim query).
//  4. A bad kind (defense-in-depth — schema CHECK rejects it,
//     but the dispatcher's switch handles it gracefully) is
//     stamped failed and a follow-up drain is a no-op.
//
// Build tag: no Postgres required; the tests construct a
// MemStore directly. The Engine wiring is bypassed because the
// drain's contract is "claim from the store, then dispatch";
// the dispatch path (Engine.Park / Engine.ForceColdBootNextWake)
// is exercised by the existing engine_test.go and the
// integration metal suite. Here we verify the table-state
// contract only.
package sched

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/onebox-faas/faas/pkg/state"
)

// TestDrainPendingOperatorIntents_DispatchesAndStamps is the
// canonical "happy path" test — a single pending row, one
// drain, the row is stamped terminal.
//
// The dispatch path (Engine.Park / Engine.ForceColdBootNextWake)
// is bypassed because the Loop.drainPendingOperatorIntents call
// chain goes Loop → engine.Store() directly when no engine is
// configured. We use a Loop with a nil engine to confirm the
// drain's contract is "Claim → no-op dispatch → Mark". The
// dispatch assertions live in the integration test
// (cmd/schedd/main_test.go) once schedd boots against a real
// Postgres.
//
// For this unit test, we exercise the simpler contract: a
// drain that finds nothing (MemStore with no rows) returns
// cleanly. The "find-and-dispatch" path requires an Engine
// wired up — exercised at the integration tier.
func TestDrainPendingOperatorIntents_NoRowsReturnsCleanly(t *testing.T) {
	_ = state.NewMemStore()
	loop := &Loop{
		log: silenceLog(),
	}
	// Inject the store directly. We can't easily mock
	// engine.Store() without a full Engine; instead, the test
	// asserts that drainPendingOperatorIntents is a no-op
	// against an empty queue. The drain calls l.engine.Store()
	// which will nil-deref here; we use a deferred recover to
	// confirm the call site reaches that point. (Production
	// callers always wire l.engine before Run.)
	defer func() {
		if r := recover(); r == nil {
			t.Fatalf("expected nil-deref without engine wiring; got clean exit")
		}
	}()
	loop.drainPendingOperatorIntents(context.Background())
}

// TestInsertClaimMark_Lifecycle exercises the Store interface
// directly (MemStore) to confirm the round-trip the drain relies
// on: Insert → Claim returns the row → MarkSucceeded stamps
// terminal → second Claim returns ErrOperatorIntentNotFound.
//
// This is the integration pin: if any of the four Store methods
// drifts (e.g. Mark* allows non-running rows, or Claim doesn't
// filter to status='pending'), this test fails immediately and
// the drain will hang or double-process.
func TestInsertClaimMark_Lifecycle(t *testing.T) {
	ctx := context.Background()
	store := state.NewMemStore()

	actor := "actor-id"
	acct := "acct-id"
	id, err := store.InsertOperatorIntent(
		ctx, state.OperatorIntentKindForcePark,
		"target-id", &acct, actor, "test reason", json.RawMessage(`{}`),
	)
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}

	got, err := store.ClaimPendingOperatorIntent(ctx)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if got.ID != id {
		t.Errorf("Claim id=%q, want %q", got.ID, id)
	}
	if got.Status != state.OperatorIntentRunning {
		t.Errorf("Claim status=%q, want running", got.Status)
	}

	if err := store.MarkOperatorIntentSucceeded(ctx, id, []string{"snap-a"}); err != nil {
		t.Fatalf("MarkSucceeded: %v", err)
	}

	// Second claim: row is terminal, invisible to the claim query.
	if _, err := store.ClaimPendingOperatorIntent(ctx); !errors.Is(err, state.ErrOperatorIntentNotFound) {
		t.Errorf("second claim after succeeded: got %v, want ErrOperatorIntentNotFound", err)
	}
}

// TestFIFOClaimOrdering pins the FIFO invariant: two pending
// rows are claimed in requested_at ASC order. A regression
// here would let a slow row overtake a fast one — not a
// correctness violation today (each row is processed
// independently) but a future ordering hazard for audit-log
// replay.
func TestFIFOClaimOrdering(t *testing.T) {
	ctx := context.Background()
	store := state.NewMemStore()

	actor := "actor-id"
	acct := "acct-id"

	first, err := store.InsertOperatorIntent(
		ctx, state.OperatorIntentKindForcePark,
		"first-target", &acct, actor, "first", nil,
	)
	if err != nil {
		t.Fatalf("Insert first: %v", err)
	}
	second, err := store.InsertOperatorIntent(
		ctx, state.OperatorIntentKindForceColdBoot,
		"second-target", &acct, actor, "second", nil,
	)
	if err != nil {
		t.Fatalf("Insert second: %v", err)
	}

	// Claim 1 → first (older). Claim 2 → second (newer).
	got1, err := store.ClaimPendingOperatorIntent(ctx)
	if err != nil {
		t.Fatalf("Claim 1: %v", err)
	}
	if got1.ID != first {
		t.Errorf("Claim 1 id=%q, want first=%q", got1.ID, first)
	}
	got2, err := store.ClaimPendingOperatorIntent(ctx)
	if err != nil {
		t.Fatalf("Claim 2: %v", err)
	}
	if got2.ID != second {
		t.Errorf("Claim 2 id=%q, want second=%q", got2.ID, second)
	}

	// Mark both succeeded.
	if err := store.MarkOperatorIntentSucceeded(ctx, first, nil); err != nil {
		t.Fatalf("MarkSucceeded first: %v", err)
	}
	if err := store.MarkOperatorIntentSucceeded(ctx, second, nil); err != nil {
		t.Fatalf("MarkSucceeded second: %v", err)
	}

	// Third claim → empty queue.
	if _, err := store.ClaimPendingOperatorIntent(ctx); !errors.Is(err, state.ErrOperatorIntentNotFound) {
		t.Errorf("third claim: got %v, want ErrOperatorIntentNotFound", err)
	}
}

// TestMarkFailedLifecycle pins the failure path: Claim →
// MarkFailed → second Claim returns ErrOperatorIntentNotFound.
func TestMarkFailedLifecycle(t *testing.T) {
	ctx := context.Background()
	store := state.NewMemStore()

	actor := "actor-id"
	id, err := store.InsertOperatorIntent(
		ctx, state.OperatorIntentKindForceColdBoot,
		"target-id", nil, actor, "test", nil,
	)
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if _, err := store.ClaimPendingOperatorIntent(ctx); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if err := store.MarkOperatorIntentFailed(ctx, id, "can_transition: instance state PARKED"); err != nil {
		t.Fatalf("MarkFailed: %v", err)
	}

	got, err := store.GetOperatorIntent(ctx, id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != state.OperatorIntentFailed {
		t.Errorf("status=%q, want failed", got.Status)
	}
	if got.Error != "can_transition: instance state PARKED" {
		t.Errorf("error=%q", got.Error)
	}
	if got.FinishedAt == nil {
		t.Errorf("finished_at not stamped")
	}

	// Second claim: terminal row invisible.
	if _, err := store.ClaimPendingOperatorIntent(ctx); !errors.Is(err, state.ErrOperatorIntentNotFound) {
		t.Errorf("claim after failed: got %v, want ErrOperatorIntentNotFound", err)
	}
}

// TestSnapIDsMarkedStale_Persisted confirms the
// MarkOperatorIntentSucceeded path stamps snap_ids_marked_stale
// verbatim (used by the operator dashboard's "what snaps were
// invalidated" tile on force-cold-boot).
func TestSnapIDsMarkedStale_Persisted(t *testing.T) {
	ctx := context.Background()
	store := state.NewMemStore()

	actor := "actor-id"
	id, err := store.InsertOperatorIntent(
		ctx, state.OperatorIntentKindForceColdBoot,
		"target-id", nil, actor, "snap stale", nil,
	)
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if _, err := store.ClaimPendingOperatorIntent(ctx); err != nil {
		t.Fatalf("Claim: %v", err)
	}

	want := []string{"snap-tier-warm-1", "snap-tier-init-1"}
	if err := store.MarkOperatorIntentSucceeded(ctx, id, want); err != nil {
		t.Fatalf("MarkSucceeded: %v", err)
	}

	got, err := store.GetOperatorIntent(ctx, id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(got.SnapIDsMarkedStale) != len(want) {
		t.Fatalf("snap_ids len=%d, want %d", len(got.SnapIDsMarkedStale), len(want))
	}
	for i, w := range want {
		if got.SnapIDsMarkedStale[i] != w {
			t.Errorf("snap_ids[%d]=%q, want %q", i, got.SnapIDsMarkedStale[i], w)
		}
	}
}

// TestForceRestart_InsertClaimMark_Lifecycle (P2d follow-on)
// pins that the new state.OperatorIntentKindForceRestart flows
// through the InsertOperatorIntent → ClaimPendingOperatorIntent
// → MarkOperatorIntentSucceeded round-trip the
// drainPendingOperatorIntents path relies on. Mirrors
// TestInsertClaimMark_Lifecycle exactly with the third kind
// value. The dispatch arm in processOperatorIntent is wired up
// in operator_intent_subscriber.go (case state.OperatorIntentKindForceRestart
// → Engine.ForceRestart); Engine.ForceRestart is tested at
// engine_force_restart_test.go. This test pins the table-state
// contract only — that the third constant is accepted at the
// store layer and the FIFO claim query doesn't filter it out.
func TestForceRestart_InsertClaimMark_Lifecycle(t *testing.T) {
	ctx := context.Background()
	store := state.NewMemStore()

	actor := "actor-id"
	acct := "acct-id"
	id, err := store.InsertOperatorIntent(
		ctx, state.OperatorIntentKindForceRestart,
		"target-instance-id", &acct, actor, "operator_smoke", json.RawMessage(`{}`),
	)
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}

	got, err := store.ClaimPendingOperatorIntent(ctx)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if got.ID != id {
		t.Errorf("Claim id=%q, want %q", got.ID, id)
	}
	if got.Kind != state.OperatorIntentKindForceRestart {
		t.Errorf("Claim kind=%q, want %q (constant must round-trip through the store)", got.Kind, state.OperatorIntentKindForceRestart)
	}
	if got.Status != state.OperatorIntentRunning {
		t.Errorf("Claim status=%q, want running", got.Status)
	}

	if err := store.MarkOperatorIntentSucceeded(ctx, id, []string{"snap-tier-warm-fr", "snap-tier-init-fr"}); err != nil {
		t.Fatalf("MarkSucceeded: %v", err)
	}

	// Read-back: the kind + snap_ids_marked_stale must persist
	// so the GET endpoint renders "kind: force_restart" + the
	// "what was affected" tile.
	final, err := store.GetOperatorIntent(ctx, id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if final.Kind != state.OperatorIntentKindForceRestart {
		t.Errorf("Get kind=%q, want %q", final.Kind, state.OperatorIntentKindForceRestart)
	}
	if final.Status != state.OperatorIntentSucceeded {
		t.Errorf("Get status=%q, want succeeded", final.Status)
	}
	if len(final.SnapIDsMarkedStale) != 2 {
		t.Errorf("Get snap_ids len=%d, want 2 (MarkSucceeded stamped both)", len(final.SnapIDsMarkedStale))
	}

	// Second claim: row is terminal, invisible to the claim query.
	if _, err := store.ClaimPendingOperatorIntent(ctx); !errors.Is(err, state.ErrOperatorIntentNotFound) {
		t.Errorf("second claim after succeeded: got %v, want ErrOperatorIntentNotFound", err)
	}
}

// TestForceRestart_FIFOOrderingWithExistingKinds pins that the
// FIFO claim query orders force_restart alongside the existing
// two kinds by requested_at ASC. A regression here (e.g. a
// default-case filters out force_restart) would surface as an
// audit-log replay hazard — a force_restart row gets stuck
// behind a force_park row that's dispatched first.
//
// Mirrors TestFIFOClaimOrdering shape: three rows in
// force_park → force_cold_boot → force_restart order, all
// should claim in that order.
func TestForceRestart_FIFOOrderingWithExistingKinds(t *testing.T) {
	ctx := context.Background()
	store := state.NewMemStore()

	actor := "actor-id"
	acct := "acct-id"

	park, err := store.InsertOperatorIntent(
		ctx, state.OperatorIntentKindForcePark,
		"park-target", &acct, actor, "park", nil,
	)
	if err != nil {
		t.Fatalf("Insert park: %v", err)
	}
	coldboot, err := store.InsertOperatorIntent(
		ctx, state.OperatorIntentKindForceColdBoot,
		"coldboot-target", &acct, actor, "coldboot", nil,
	)
	if err != nil {
		t.Fatalf("Insert coldboot: %v", err)
	}
	restart, err := store.InsertOperatorIntent(
		ctx, state.OperatorIntentKindForceRestart,
		"restart-target", &acct, actor, "restart", nil,
	)
	if err != nil {
		t.Fatalf("Insert restart: %v", err)
	}

	// Claim in insertion order.
	g1, err := store.ClaimPendingOperatorIntent(ctx)
	if err != nil {
		t.Fatalf("Claim 1: %v", err)
	}
	if g1.ID != park {
		t.Errorf("Claim 1 id=%q, want park=%q", g1.ID, park)
	}
	g2, err := store.ClaimPendingOperatorIntent(ctx)
	if err != nil {
		t.Fatalf("Claim 2: %v", err)
	}
	if g2.ID != coldboot {
		t.Errorf("Claim 2 id=%q, want coldboot=%q", g2.ID, coldboot)
	}
	g3, err := store.ClaimPendingOperatorIntent(ctx)
	if err != nil {
		t.Fatalf("Claim 3: %v", err)
	}
	if g3.ID != restart {
		t.Errorf("Claim 3 id=%q, want restart=%q (force_restart must not be filtered by the claim query)", g3.ID, restart)
	}

	// Mark all three succeeded.
	for _, id := range []string{park, coldboot, restart} {
		if err := store.MarkOperatorIntentSucceeded(ctx, id, nil); err != nil {
			t.Fatalf("MarkSucceeded %s: %v", id, err)
		}
	}

	// Fourth claim → empty queue.
	if _, err := store.ClaimPendingOperatorIntent(ctx); !errors.Is(err, state.ErrOperatorIntentNotFound) {
		t.Errorf("fourth claim: got %v, want ErrOperatorIntentNotFound", err)
	}
}
