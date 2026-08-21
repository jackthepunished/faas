// wait_for_branches_test.go — branch coverage for the
// precondition guards at the top of pkg/db/wait_for.go's
// WaitForNotification helper.
//
// WaitForNotification's signature requires a real
// *pgxpool.Pool, which we can't construct without
// DATABASE_URL. The guarded precondition checks fire BEFORE
// pool.Acquire; a typed-nil `(*pgxpool.Pool)(nil)` is enough
// to exercise the FIRST guard without spinning up Postgres.
//
// What this file pins (the only branch reachable without DB):
//
//   - pool == nil → "nil pool" descriptive error
//   - timeout <= 0 → ErrWaitTimeout (separate from channel/pool
//     guards; ordering in WaitForNotification puts it AFTER the
//     pool/channel/predicate guards but BEFORE pool.Acquire, so
//     when the pool IS nil we never reach the timeout check)
//
// The deeper branches (channel/predicate guards, LISTEN
// success path, payload matching) are covered by the existing
// wait_for_test.go which uses pgtest.Open(t).
//
// Whitebox test (package db).
package db

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// TestWaitForNotification_NilPool pins the pool == nil guard:
// a typed-nil pool with valid channel/predicate/timeout
// surfaces a descriptive "nil pool" error before pool.Acquire.
//
// WaitForNotification's guard ordering is:
//
//   1. pool == nil
//   2. channel == ""
//   3. predicate == nil
//   4. timeout <= 0
//   5. → pool.Acquire (DB-bound)
//
// A typed-nil pool stops at guard #1; the rest can't run
// without spinning up Postgres.
func TestWaitForNotification_NilPool(t *testing.T) {
	got, err := WaitForNotification(context.Background(),
		(*pgxpool.Pool)(nil),
		"ch",
		func(string) bool { return true },
		time.Second)
	if err == nil {
		t.Fatal("nil pool: want err, got nil")
	}
	if got != "" {
		t.Errorf("errored call returned payload %q, want empty", got)
	}
	if !contains(err.Error(), "nil pool") {
		t.Errorf("err = %v, want substring 'nil pool'", err)
	}
}

// TestWaitForNotification_NilPoolEvenWithZeroTimeout pins the
// guard-ordering invariant: pool == nil fires BEFORE
// timeout <= 0. A nil pool with a zero timeout surfaces the
// nil-pool error (NOT ErrWaitTimeout). This locks the
// precedence: a future refactor that reorders the guards
// trips here.
func TestWaitForNotification_NilPoolEvenWithZeroTimeout(t *testing.T) {
	_, err := WaitForNotification(context.Background(),
		(*pgxpool.Pool)(nil),
		"ch",
		func(string) bool { return true },
		0) // zero timeout
	if err == nil {
		t.Fatal("nil pool + zero timeout: want err, got nil")
	}
	if errors.Is(err, ErrWaitTimeout) {
		t.Errorf("ordering changed: nil pool + zero timeout returned ErrWaitTimeout; the pool guard should fire first")
	}
	if !contains(err.Error(), "nil pool") {
		t.Errorf("err = %v, want 'nil pool' (ordering invariant)", err)
	}
}

// --- helpers ---

// contains is a tiny strings.Contains replacement so this file
// doesn't pull in the strings package just for two checks.
func contains(s, substr string) bool {
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
