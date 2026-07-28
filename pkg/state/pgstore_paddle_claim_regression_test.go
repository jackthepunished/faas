package state_test

// Regression net for the fresh-INSERT claim path of
// PgStore.ClaimPaddleOverageWindow. Pre-fix the tripwire fails because
// the Step 2 UPDATE WHERE clause excludes freshly-inserted rows
// (claimed_at = now() from Step 1; now() < now() - lease is FALSE).
//
// The fix lives in pkg/state/pgstore.go:3881-3892 — the WHERE now
// also includes `state = 'completed'` so Step 1's fresh 'completed'
// rows are claimable, matching the design intent documented in the
// commit message of 1979b95.
//
// Tripwire lifecycle (per memory pre-fix-redox-proof.md):
//   1. Test is added BEFORE the fix is applied.
//   2. CI surfaces the failure on the fix PR.
//   3. The pgstore.go patch flips the test green.
//
// This is the load-bearing branch — without it, every meterd Paddle
// pusher call would silently treat "fresh window" as "another pod
// holds the claim" and skip the Paddle POST forever.

import (
	"testing"
	"time"
)

// TestPg_ClaimPaddleOverageWindow_FreshSucceeds pins the "this pod
// claimed the fresh window" branch. The MemStore equivalent has
// passed for months, but PgStore's INSERT-then-UPDATE pattern needs
// both rows to match — see pkg/state/pgstore.go:3875-3892 (comment
// "Three branches map to claimable rows").
//
// Steps:
//  1. createAccount (auto-creates an empty schema row).
//  2. ClaimPaddleOverageWindow on a fresh (acct, window_start).
//  3. Assert claimed=true.
//
// Pre-fix output:
//
//	pgstore_paddle_claim_regression_test.go:NN:
//	  ClaimPaddleOverageWindow(fresh) = false, want true
//	--- FAIL: TestPg_ClaimPaddleOverageWindow_FreshSucceeds (0.17s)
//
// Post-fix output: ok.
func TestPg_ClaimPaddleOverageWindow_FreshSucceeds(t *testing.T) {
	s, ctx := pgStore(t)
	acctID := createAccount(t, s, ctx, pgTestEmail(t))
	window := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)

	claimed, err := s.ClaimPaddleOverageWindow(ctx, acctID, window, "pod-regression", 5*time.Minute)
	if err != nil {
		t.Fatalf("ClaimPaddleOverageWindow(fresh): %v", err)
	}
	if !claimed {
		t.Fatalf("ClaimPaddleOverageWindow(fresh) = false, want true (fresh-row claim path)")
	}
}

// TestPg_ClaimPaddleOverageWindow_ReclaimAfterCompleteIsPermitted pins
// the design-comment-documented "row exists and is completed → treat
// as fresh re-claim" branch. After a successful Claim → Complete
// cycle the row is in state='completed', and a subsequent Claim from
// another pod MUST succeed: this is how the retry-after-failure path
// works without waiting on a lease to expire.
//
// A prior PR-B attempt asserted claim-after-complete returns false,
// but that contradicts the design comment at pgstore.go:3846-3848:
// "row already exists and is completed (which is a stale pre-PR-#204
// row that the caller should treat as a fresh re-claim)". This is the
// corrected version — moved here from PR-B so the fix PR carries the
// single coherent Paddle claim contract net.
func TestPg_ClaimPaddleOverageWindow_ReclaimAfterCompleteIsPermitted(t *testing.T) {
	s, ctx := pgStore(t)
	acctID := createAccount(t, s, ctx, pgTestEmail(t))
	window := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)

	if _, err := s.ClaimPaddleOverageWindow(ctx, acctID, window, "pod-A", 5*time.Minute); err != nil {
		t.Fatalf("ClaimPaddleOverageWindow: %v", err)
	}
	if err := s.CompletePaddleOverageWindow(ctx, acctID, window, 1024); err != nil {
		t.Fatalf("CompletePaddleOverageWindow: %v", err)
	}

	// Re-claim by pod-B is intentionally permitted: the row is in
	// state='completed' and Step 2's WHERE matches (state='completed'
	// OR claimed_at IS NULL OR claimed_at < now() - lease). See the
	// comment block at pgstore.go:3875-3897.
	claimed, err := s.ClaimPaddleOverageWindow(ctx, acctID, window, "pod-B", 5*time.Minute)
	if err != nil {
		t.Fatalf("ClaimPaddleOverageWindow(pod-B after complete): %v", err)
	}
	if !claimed {
		t.Errorf("ClaimPaddleOverageWindow(pod-B after complete) = false, want true (re-claim on completed is the documented design — retry-after-failure path)")
	}
}
