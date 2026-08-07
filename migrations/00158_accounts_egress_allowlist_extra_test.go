//go:build !no_pg

// Migration-apply test for 00158 (per-account egress allowlist
// extra budget, issue #679 / PR-B / ADR-082). Pins the new
// column:
//
//  1. The migration set applies cleanly through 00158.
//  2. The column accepts the canonical integer shape and round-trips.
//  3. Default is 0 (regression check — pre-PR rows keep using
//     the plan cap alone, no opt-in required, no customer breakage).
//  4. The DB CHECK constraint refuses negative values.
//  5. Replay-safe: ADD COLUMN IF NOT EXISTS makes a second
//     MigrateUp a no-op (PR #377 / ADR-041).
//
// Slot note: HEAD is at 00155 (apps.websocket_enabled, issue #676
// ADR-080); 00158 is the next free slot at PR creation time. The
// migration is slot-agnostic — only the filename and the test
// function name carry the literal slot. If a sibling PR grabs
// 00158 first, renumber per `migrations/README.md` and update
// this test's filename + the ApplyUp range below.
//
// Build tag matches the rest of the migration tests; set
// FAAS_SKIP_PG_TESTS=1 to skip locally (see migrations/README.md).
package migrations_test

import (
	"context"
	"testing"

	"github.com/onebox-faas/faas/pkg/db"
	"github.com/onebox-faas/faas/pkg/db/pgtest"
)

func TestMigrations_00158_AccountsEgressAllowlistExtra(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.Open(t)

	// Seed UUIDs carry the slot number in the last group
	// (`...000158`, `...000256`) so a reader scanning the test
	// fixtures can pin each row to this migration without
	// grepping the file name. The literal slot value MUST stay
	// in sync with the filename; renumber per
	// `migrations/README.md` if a sibling PR grabs 00158 first.

	// (1) Apply through 00158. A regression that drops a slot
	// between 1 and 155 surfaces here before the per-assertion
	// pins.
	if err := db.MigrateUp(ctx, pool); err != nil {
		t.Fatalf("db.MigrateUp: %v (regression: missing migration slot between 1 and 158)", err)
	}

	// (2) Seed an account. The literal UUID is fixed across
	// reruns so the seed is idempotent. The plan choice (hobby)
	// is informational — this migration test only exercises the
	// column shape, not the plan-gate check at the apid layer.
	if _, err := pool.Exec(ctx, `
		insert into accounts (id, email, plan, created_at)
		values ('00000000-0000-0000-0000-000000000158',
		        'egress-extra-test@example.com', 'hobby', now())
		on conflict (id) do nothing
	`); err != nil {
		t.Fatalf("seed account: %v", err)
	}

	// (3) Default round-trip: the freshly-inserted account reads
	// egress_allowlist_extra=0 (NOT NULL DEFAULT 0). This is the
	// regression check that pre-PR rows keep using the plan cap
	// alone — the load-bearing invariant for issue #679 (no
	// existing customer breakage, every existing account keeps
	// working without an admin override).
	var defaultVal int
	if err := pool.QueryRow(ctx, `
		select egress_allowlist_extra from accounts where id = '00000000-0000-0000-0000-000000000158'
	`).Scan(&defaultVal); err != nil {
		t.Fatalf("read default egress_allowlist_extra: %v", err)
	}
	if defaultVal != 0 {
		t.Errorf("egress_allowlist_extra default = %d, want 0 (regression: pre-PR rows must stay on plan cap)", defaultVal)
	}

	// (4) PATCH-style round-trip: UPDATE bumps the field to a
	// non-zero value and reads it back. Mirrors the apid
	// setAccountEgressAllowlistExtra handler path so a future
	// regression in the write side surfaces here.
	if _, err := pool.Exec(ctx, `
		update accounts set egress_allowlist_extra = 32 where id = '00000000-0000-0000-0000-000000000158'
	`); err != nil {
		t.Fatalf("update egress_allowlist_extra=32: %v", err)
	}
	var setVal int
	if err := pool.QueryRow(ctx, `
		select egress_allowlist_extra from accounts where id = '00000000-0000-0000-0000-000000000158'
	`).Scan(&setVal); err != nil {
		t.Fatalf("read set egress_allowlist_extra: %v", err)
	}
	if setVal != 32 {
		t.Errorf("egress_allowlist_extra after update = %d, want 32", setVal)
	}

	// (5) Replay safety: a second MigrateUp is a no-op (the
	// migration uses ADD COLUMN IF NOT EXISTS). ADR-041 contract.
	if err := db.MigrateUp(ctx, pool); err != nil {
		t.Fatalf("replay-safety: second MigrateUp failed: %v", err)
	}

	// (6) DB CHECK constraint: negative values are rejected at the
	// DB layer. The apid layer also enforces >= 0 (the
	// ErrAccountEgressAllowlistExtraOutOfRange guard) but the DB
	// CHECK is the wire-bypass backstop. PostreSQL returns 23514
	// (check_violation) for a CHECK constraint reject.
	if _, err := pool.Exec(ctx, `
		update accounts set egress_allowlist_extra = -1 where id = '00000000-0000-0000-0000-000000000158'
	`); err == nil {
		t.Errorf("egress_allowlist_extra = -1 accepted; want CHECK violation (regression: wire-bypass let a negative value land)")
	}
}
