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
		&acct, actor, "wedged instance", json.RawMessage(`{}`),
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
		"first-target", &acct, actor, "first", nil,
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
		"second-target", &acct, actor, "second", nil,
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
		"44444444-4444-4444-4444-444444444444", nil, actor, "stale snap", nil,
	)
	if err != nil {
		t.Fatalf("InsertOperatorIntent: %v", err)
	}
	if _, err := store.ClaimPendingOperatorIntent(ctx); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if err := store.MarkOperatorIntentFailed(ctx, id, "deployment not found"); err != nil {
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
		"55555555-5555-5555-5555-555555555555", nil, actor, "no claim", nil,
	)
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}
	// Mark succeeded on a row that's still pending → error.
	if err := store.MarkOperatorIntentSucceeded(ctx, id, nil); !errors.Is(err, state.ErrOperatorIntentNotFound) {
		t.Errorf("MarkSucceeded on pending row: got %v, want ErrOperatorIntentNotFound", err)
	}
}
