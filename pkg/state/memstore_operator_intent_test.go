// memstore_operator_intent_test.go — MemStore operator_intents
// CRUD. Mirrors pgstore_operator_intent_test.go at the
// in-process layer so handler tests can construct a MemStore
// without spinning up Postgres. Pins:
//
//  1. InsertOperatorIntent → row exists with status='pending'.
//  2. ClaimPendingOperatorIntent returns the row FIFO
//     (oldest requested_at first), transitions to
//     status='running', stamps started_at.
//  3. MarkOperatorIntentSucceeded / Failed stamp terminal
//     state with finished_at.
//  4. GetOperatorIntent returns ErrOperatorIntentNotFound
//     for missing id.
//  5. FIFO claim semantics: with two pending rows, the
//     earlier-requested row is claimed first.
//  6. ReclaimStuckRunningOperatorIntents resets rows whose
//     StartedAt is older than the threshold back to
//     `pending` (StartedAt cleared), leaves terminal rows
//     alone, and is idempotent.
//
// Build tag matches the rest of the memstore tests; no
// Postgres required.
package state_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/onebox-faas/faas/pkg/state"
)

func TestMemStore_OperatorIntent_FullLifecycle(t *testing.T) {
	ctx := context.Background()
	store := state.NewMemStore()

	acct := "11111111-1111-1111-1111-111111111111"
	actor := "22222222-2222-2222-2222-222222222222"
	id, err := store.InsertOperatorIntent(
		ctx, state.OperatorIntentKindForcePark,
		"33333333-3333-3333-3333-333333333333",
		&acct, actor, "wedged instance", json.RawMessage(`{}`), nil,
	)
	if err != nil {
		t.Fatalf("InsertOperatorIntent: %v", err)
	}
	if id == "" {
		t.Fatalf("InsertOperatorIntent returned empty id")
	}

	got, err := store.ClaimPendingOperatorIntent(ctx)
	if err != nil {
		t.Fatalf("ClaimPendingOperatorIntent: %v", err)
	}
	if got.ID != id {
		t.Errorf("Claim id=%q, want %q", got.ID, id)
	}
	if got.Status != state.OperatorIntentRunning {
		t.Errorf("Claim status=%q, want running", got.Status)
	}
	if got.StartedAt == nil {
		t.Errorf("Claim did not stamp started_at")
	}

	if err := store.MarkOperatorIntentSucceeded(ctx, id, []string{"snap-1"}); err != nil {
		t.Fatalf("MarkOperatorIntentSucceeded: %v", err)
	}

	got2, err := store.GetOperatorIntent(ctx, id)
	if err != nil {
		t.Fatalf("GetOperatorIntent: %v", err)
	}
	if got2.Status != state.OperatorIntentSucceeded {
		t.Errorf("status=%q, want succeeded", got2.Status)
	}
	if got2.FinishedAt == nil {
		t.Errorf("finished_at not stamped")
	}
}

func TestMemStore_OperatorIntent_FIFOClaim(t *testing.T) {
	ctx := context.Background()
	store := state.NewMemStore()

	actor := "22222222-2222-2222-2222-222222222222"
	acct := "11111111-1111-1111-1111-111111111111"

	// Insert two intents — older first, then newer.
	first, err := store.InsertOperatorIntent(
		ctx, state.OperatorIntentKindForcePark,
		"first-target", &acct, actor, "first", nil, nil,
	)
	if err != nil {
		t.Fatalf("Insert first: %v", err)
	}
	// Sleep to ensure requested_at ordering survives at
	// the time.Now() resolution. memstore uses
	// time.Now().UTC(); 1ms is plenty.
	time.Sleep(time.Millisecond)
	second, err := store.InsertOperatorIntent(
		ctx, state.OperatorIntentKindForcePark,
		"second-target", &acct, actor, "second", nil, nil,
	)
	if err != nil {
		t.Fatalf("Insert second: %v", err)
	}

	// Claim should return the older (first) row first.
	got1, err := store.ClaimPendingOperatorIntent(ctx)
	if err != nil {
		t.Fatalf("Claim 1: %v", err)
	}
	if got1.ID != first {
		t.Errorf("first claim id=%q, want %q (older row)", got1.ID, first)
	}

	got2, err := store.ClaimPendingOperatorIntent(ctx)
	if err != nil {
		t.Fatalf("Claim 2: %v", err)
	}
	if got2.ID != second {
		t.Errorf("second claim id=%q, want %q (newer row)", got2.ID, second)
	}

	// Third claim → ErrOperatorIntentNotFound (queue empty).
	if _, err := store.ClaimPendingOperatorIntent(ctx); !errors.Is(err, state.ErrOperatorIntentNotFound) {
		t.Errorf("third claim: got %v, want ErrOperatorIntentNotFound", err)
	}
}

func TestMemStore_OperatorIntent_MarkFailed(t *testing.T) {
	ctx := context.Background()
	store := state.NewMemStore()

	actor := "22222222-2222-2222-2222-222222222222"
	id, err := store.InsertOperatorIntent(
		ctx, state.OperatorIntentKindForceColdBoot,
		"44444444-4444-4444-4444-444444444444", nil, actor, "stale snap", nil, nil,
	)
	if err != nil {
		t.Fatalf("InsertOperatorIntent: %v", err)
	}
	if _, err := store.ClaimPendingOperatorIntent(ctx); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if err := store.MarkOperatorIntentFailed(ctx, id, "deployment not found", nil); err != nil {
		t.Fatalf("MarkOperatorIntentFailed: %v", err)
	}
	got, err := store.GetOperatorIntent(ctx, id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != state.OperatorIntentFailed {
		t.Errorf("status=%q, want failed", got.Status)
	}
	if got.Error != "deployment not found" {
		t.Errorf("error=%q, want %q", got.Error, "deployment not found")
	}
}

func TestMemStore_OperatorIntent_GetNotFound(t *testing.T) {
	ctx := context.Background()
	store := state.NewMemStore()
	if _, err := store.GetOperatorIntent(ctx, "missing-id"); !errors.Is(err, state.ErrOperatorIntentNotFound) {
		t.Errorf("Get(missing): got %v, want ErrOperatorIntentNotFound", err)
	}
}

func TestMemStore_OperatorIntent_MarkWithoutClaimReturnsNotFound(t *testing.T) {
	ctx := context.Background()
	store := state.NewMemStore()
	actor := "22222222-2222-2222-2222-222222222222"
	id, err := store.InsertOperatorIntent(
		ctx, state.OperatorIntentKindForcePark,
		"55555555-5555-5555-5555-555555555555", nil, actor, "no claim", nil, nil,
	)
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}
	// Mark succeeded on a row that's still pending → error.
	if err := store.MarkOperatorIntentSucceeded(ctx, id, nil); !errors.Is(err, state.ErrOperatorIntentNotFound) {
		t.Errorf("MarkSucceeded on pending row: got %v, want ErrOperatorIntentNotFound", err)
	}
}

// TestMemStore_OperatorIntent_ReclaimStuckRunning mirrors
// the pgstore test: claim a row (status -> running, stamps
// started_at), then assert that
// ReclaimStuckRunningOperatorIntents:
//
//   - flips the row's status back to `pending` with started_at
//     cleared, when the threshold is in the future (matches
//     every running row regardless of when it started);
//   - leaves terminal rows alone (where clause filters on
//     status='running');
//   - is idempotent on a second call;
//   - allows the reclaimed row to be Claimed again.
//
// Unlike the pgstore test we don't back-date started_at
// (the MemStore map is a struct value, not a pointer, and
// there's no exposed StartedAt setter); using a future
// threshold achieves the same WHERE-clause match — a row is
// reclaimed iff status='running' AND started_at IS NOT NULL
// AND started_at < threshold. A threshold in the future
// selects every running row.
func TestMemStore_OperatorIntent_ReclaimStuckRunning(t *testing.T) {
	ctx := context.Background()
	store := state.NewMemStore()

	actor := "22222222-2222-2222-2222-222222222222"
	acct := "11111111-1111-1111-1111-111111111111"

	stuckID, err := store.InsertOperatorIntent(
		ctx, state.OperatorIntentKindForcePark,
		"66666666-6666-6666-6666-666666666666",
		&acct, actor, "stuck", nil, nil,
	)
	if err != nil {
		t.Fatalf("InsertOperatorIntent(stuck): %v", err)
	}
	if _, err := store.ClaimPendingOperatorIntent(ctx); err != nil {
		t.Fatalf("ClaimPendingOperatorIntent(stuck): %v", err)
	}

	// Terminal row: insert + claim + MarkSucceeded.
	terminalID, err := store.InsertOperatorIntent(
		ctx, state.OperatorIntentKindForceColdBoot,
		"77777777-7777-7777-7777-777777777777",
		&acct, actor, "terminal", nil, nil,
	)
	if err != nil {
		t.Fatalf("InsertOperatorIntent(terminal): %v", err)
	}
	if _, err := store.ClaimPendingOperatorIntent(ctx); err != nil {
		t.Fatalf("ClaimPendingOperatorIntent(terminal): %v", err)
	}
	if err := store.MarkOperatorIntentSucceeded(ctx, terminalID, []string{"s1"}); err != nil {
		t.Fatalf("MarkOperatorIntentSucceeded(terminal): %v", err)
	}

	// Future threshold matches the just-claimed row (whose
	// started_at is roughly time.Now()) because every
	// started_at is in the past relative to it.
	threshold := time.Now().Add(time.Minute)
	n, err := store.ReclaimStuckRunningOperatorIntents(ctx, threshold)
	if err != nil {
		t.Fatalf("ReclaimStuckRunningOperatorIntents: %v", err)
	}
	if n != 1 {
		t.Errorf("reclaim count = %d, want 1", n)
	}

	got, err := store.GetOperatorIntent(ctx, stuckID)
	if err != nil {
		t.Fatalf("GetOperatorIntent(stuck post-reclaim): %v", err)
	}
	if got.Status != state.OperatorIntentPending {
		t.Errorf("stuck.Status = %q, want pending", got.Status)
	}
	if got.StartedAt != nil {
		t.Errorf("stuck.StartedAt = %v, want nil after reclaim", got.StartedAt)
	}

	terminal, err := store.GetOperatorIntent(ctx, terminalID)
	if err != nil {
		t.Fatalf("GetOperatorIntent(terminal): %v", err)
	}
	if terminal.Status != state.OperatorIntentSucceeded {
		t.Errorf("terminal.Status = %q, want succeeded", terminal.Status)
	}

	// Idempotency.
	n2, err := store.ReclaimStuckRunningOperatorIntents(ctx, threshold)
	if err != nil {
		t.Fatalf("ReclaimStuckRunningOperatorIntents (idempotent): %v", err)
	}
	if n2 != 0 {
		t.Errorf("reclaim idempotent count = %d, want 0", n2)
	}

	// Reclaimed row is now claimable again.
	if _, err := store.ClaimPendingOperatorIntent(ctx); err != nil {
		t.Errorf("ClaimPendingOperatorIntent after reclaim: %v", err)
	}
}
