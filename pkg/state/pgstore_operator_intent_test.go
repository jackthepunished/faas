//go:build !no_pg

// pgstore_operator_intent_test.go — operator_intents table CRUD
// against a real Postgres (pgtest.Open). Pins:
//
//  1. InsertOperatorIntent → row exists with status='pending'.
//  2. ClaimPendingOperatorIntent returns the same row with
//     status='running' + started_at stamped.
//  3. SKIP LOCKED semantics: a second Claim while the first
//     row is `running` returns ErrOperatorIntentNotFound
//     (the running row is invisible to the next claim).
//  4. MarkOperatorIntentSucceeded → status='succeeded' +
//     finished_at + snap_ids_marked_stale stamped.
//  5. MarkOperatorIntentFailed → status='failed' + error +
//     finished_at stamped.
//  6. GetOperatorIntent returns the row by id;
//     ErrOperatorIntentNotFound for missing id.
//  7. Replay-safe: a second claim after MarkSucceeded returns
//     ErrOperatorIntentNotFound (the row is terminal).
//
// Build tag matches the rest of the pgstore tests; set
// FAAS_SKIP_PG_TESTS=1 to skip locally (see migrations/README.md).
package state_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/onebox-faas/faas/pkg/db/pgtest"
	"github.com/onebox-faas/faas/pkg/state"
)

func TestPgStore_OperatorIntent_FullLifecycle(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.Open(t)
	store := state.NewPgStore(pool)

	// 1. Insert.
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

	// 2. Claim.
	got, err := store.ClaimPendingOperatorIntent(ctx)
	if err != nil {
		t.Fatalf("ClaimPendingOperatorIntent: %v", err)
	}
	if got.ID != id {
		t.Errorf("Claim returned id=%q, want %q", got.ID, id)
	}
	if got.Status != state.OperatorIntentRunning {
		t.Errorf("Claim status=%q, want running", got.Status)
	}
	if got.StartedAt == nil {
		t.Errorf("Claim did not stamp started_at")
	}

	// 3. SKIP LOCKED — second claim returns
	// ErrOperatorIntentNotFound (the running row is
	// invisible to the next claim).
	if _, err := store.ClaimPendingOperatorIntent(ctx); !errors.Is(err, state.ErrOperatorIntentNotFound) {
		t.Errorf("second claim while running: got %v, want ErrOperatorIntentNotFound", err)
	}

	// 4. Mark succeeded.
	if err := store.MarkOperatorIntentSucceeded(ctx, id, []string{"snap-1", "snap-2"}); err != nil {
		t.Fatalf("MarkOperatorIntentSucceeded: %v", err)
	}

	// 5. Get returns terminal state.
	got2, err := store.GetOperatorIntent(ctx, id)
	if err != nil {
		t.Fatalf("GetOperatorIntent: %v", err)
	}
	if got2.Status != state.OperatorIntentSucceeded {
		t.Errorf("Get status=%q, want succeeded", got2.Status)
	}
	if got2.FinishedAt == nil {
		t.Errorf("Get did not stamp finished_at")
	}
	if len(got2.SnapIDsMarkedStale) != 2 || got2.SnapIDsMarkedStale[0] != "snap-1" {
		t.Errorf("Get snap_ids=%v, want [snap-1 snap-2]", got2.SnapIDsMarkedStale)
	}

	// 6. Replay-safe: third claim returns
	// ErrOperatorIntentNotFound (the row is terminal,
	// invisible to the claim query).
	if _, err := store.ClaimPendingOperatorIntent(ctx); !errors.Is(err, state.ErrOperatorIntentNotFound) {
		t.Errorf("claim after succeeded: got %v, want ErrOperatorIntentNotFound", err)
	}
}

func TestPgStore_OperatorIntent_FailurePath(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.Open(t)
	store := state.NewPgStore(pool)

	acct := "11111111-1111-1111-1111-111111111111"
	actor := "22222222-2222-2222-2222-222222222222"
	id, err := store.InsertOperatorIntent(
		ctx, state.OperatorIntentKindForceColdBoot,
		"44444444-4444-4444-4444-444444444444",
		&acct, actor, "stale snap", nil,
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
		t.Fatalf("GetOperatorIntent: %v", err)
	}
	if got.Status != state.OperatorIntentFailed {
		t.Errorf("status=%q, want failed", got.Status)
	}
	if got.Error != "deployment not found" {
		t.Errorf("error=%q, want %q", got.Error, "deployment not found")
	}
}

func TestPgStore_OperatorIntent_GetNotFound(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.Open(t)
	store := state.NewPgStore(pool)

	_, err := store.GetOperatorIntent(ctx, "deadbeef-dead-beef-dead-beefdeadbeef")
	if !errors.Is(err, state.ErrOperatorIntentNotFound) {
		t.Errorf("GetOperatorIntent(missing): got %v, want ErrOperatorIntentNotFound", err)
	}
}

func TestPgStore_OperatorIntent_NilAccountID(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.Open(t)
	store := state.NewPgStore(pool)

	actor := "22222222-2222-2222-2222-222222222222"
	id, err := store.InsertOperatorIntent(
		ctx, state.OperatorIntentKindForcePark,
		"55555555-5555-5555-5555-555555555555",
		nil, actor, "fleet-level", json.RawMessage(`{}`),
	)
	if err != nil {
		t.Fatalf("InsertOperatorIntent: %v", err)
	}

	got, err := store.GetOperatorIntent(ctx, id)
	if err != nil {
		t.Fatalf("GetOperatorIntent: %v", err)
	}
	if got.AccountID != nil {
		t.Errorf("AccountID=%v, want nil for fleet-level", got.AccountID)
	}
}
