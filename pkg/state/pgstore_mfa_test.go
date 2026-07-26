// MFA pgstore tests for issue #329 — pin the (matched, lastCode,
// remaining) return shape from ConsumeRecoveryCode against the
// real SELECT … FOR UPDATE + UPDATE SQL in pgstore.go.
//
// The pgtest harness in pgstore_test.go opens a fresh schema per
// test, so the assertions don't need to scrub shared state.
// Mirror coverage of pkg/state/memstore_test.go::TestConsumeRecoveryCode_*
// exists for MemStore; this file is the Postgres-side pin.
//
// The remaining-count branch is the new contract that
// pkg/mail.RecoveryCodeBurnedBody depends on for tone — see
// issue #329. Regressing `remaining` would silently re-rank every
// customer into the wrong bucket ("one-of-many" tone for a
// customer on their last code, etc.).

package state_test

import (
	"testing"

	"github.com/onebox-faas/faas/pkg/api"
	"github.com/onebox-faas/faas/pkg/auth"
)

// TestPg_ConsumeRecoveryCode_RemainingCount pins the SELECT … FOR
// UPDATE + UPDATE round-trip's `remaining` output against a real
// Postgres. Two cases:
//
//   - burn 1 of 10 codes → remaining == 9 (the common case; mailer
//     lands in the "one-of-many" branch of RecoveryCodeBurnedBody)
//   - burn the last code → remaining == 0 (mailer lands in the
//     "NO codes left" branch; only reachable on /disable's
//     recovery_code path today, but the store contract must
//     report the post-burn count honestly so future paths can
//     rely on it)
func TestPg_ConsumeRecoveryCode_RemainingCount(t *testing.T) {
	s, ctx := pgStore(t)
	acct, err := s.CreateAccount(ctx, pgTestEmail(t), api.PlanPro)
	if err != nil {
		t.Fatalf("CreateAccount: %v", err)
	}
	plaintexts, hashes, err := auth.NewRecoveryCodes(auth.RecoveryCodeCount)
	if err != nil {
		t.Fatalf("NewRecoveryCodes: %v", err)
	}
	if err := s.SetMFASecret(ctx, acct.ID, []byte("sealed"), hashes); err != nil {
		t.Fatalf("SetMFASecret: %v", err)
	}

	// Case 1: burn one of ten. remaining must drop to 9.
	matched, lastCode, remaining, err := s.ConsumeRecoveryCode(ctx, acct.ID, auth.HashRecoveryCode(plaintexts[0]))
	if err != nil {
		t.Fatalf("ConsumeRecoveryCode: %v", err)
	}
	if !matched || lastCode {
		t.Errorf("burn #1: matched=%v lastCode=%v, want true/false", matched, lastCode)
	}
	if remaining != auth.RecoveryCodeCount-1 {
		t.Errorf("burn #1: remaining = %d, want %d (issue #329 mailer tone bucket)", remaining, auth.RecoveryCodeCount-1)
	}

	// Case 2: burn down to zero by driving the store primitive
	// directly (the /recover handler refuses the last code; the
	// store contract is allowed to consume it).
	for i := 1; i < auth.RecoveryCodeCount; i++ {
		matched, lastCode, remaining, err := s.ConsumeRecoveryCode(ctx, acct.ID, auth.HashRecoveryCode(plaintexts[i]))
		if err != nil {
			t.Fatalf("burn %d: %v", i, err)
		}
		if !matched {
			t.Errorf("burn %d: matched = false, want true", i)
		}
		if i == auth.RecoveryCodeCount-1 {
			if !lastCode {
				t.Errorf("burn %d: lastCode = false, want true (final consume)", i)
			}
			if remaining != 0 {
				t.Errorf("burn %d: remaining = %d, want 0 (mailer NO-codes-left branch)", i, remaining)
			}
		} else if remaining != auth.RecoveryCodeCount-1-i {
			t.Errorf("burn %d: remaining = %d, want %d", i, remaining, auth.RecoveryCodeCount-1-i)
		}
	}

	// And the row's mfa_recovery_codes_hash is empty.
	after, err := s.AccountByID(ctx, acct.ID)
	if err != nil {
		t.Fatalf("AccountByID: %v", err)
	}
	if len(after.MFARecoveryCodesHash) != 0 {
		t.Errorf("mfa_recovery_codes_hash length = %d, want 0", len(after.MFARecoveryCodesHash))
	}
}

// TestPg_ConsumeRecoveryCode_NoMatchReturnsZero pins the
// `matched=false → remaining=0` invariant. The mailer must NOT
// receive a non-zero remaining count on a no-match (it would
// mis-render the body as if a burn had succeeded). This is the
// SQL-side pin for the MemStore's TestConsumeRecoveryCode_NoMatch
// assertion.
func TestPg_ConsumeRecoveryCode_NoMatchReturnsZero(t *testing.T) {
	s, ctx := pgStore(t)
	acct, err := s.CreateAccount(ctx, pgTestEmail(t), api.PlanPro)
	if err != nil {
		t.Fatalf("CreateAccount: %v", err)
	}
	_, hashes, err := auth.NewRecoveryCodes(auth.RecoveryCodeCount)
	if err != nil {
		t.Fatalf("NewRecoveryCodes: %v", err)
	}
	if err := s.SetMFASecret(ctx, acct.ID, []byte("sealed"), hashes); err != nil {
		t.Fatalf("SetMFASecret: %v", err)
	}

	matched, lastCode, remaining, err := s.ConsumeRecoveryCode(ctx, acct.ID, []byte("not-a-real-hash"))
	if err != nil {
		t.Fatalf("ConsumeRecoveryCode: %v", err)
	}
	if matched || lastCode {
		t.Errorf("matched=%v lastCode=%v, want false/false on a no-match", matched, lastCode)
	}
	if remaining != 0 {
		t.Errorf("remaining = %d, want 0 (no-match must not report a post-burn count)", remaining)
	}
}
