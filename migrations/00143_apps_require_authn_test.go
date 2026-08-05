//go:build !no_pg

// Migration-apply test for 00143 (per-app require_authn flag,
// issue #560). Pins the new column:
//
//  1. The migration set applies cleanly through 00143.
//  2. The column accepts the canonical boolean shape and round-trips.
//  3. Default is false (regression check — pre-PR rows stay
//     public-by-default, no opt-in required, no customer breakage).
//  4. Replay-safe: ADD COLUMN IF NOT EXISTS makes a second MigrateUp
//     no-op (ADR-041).
//
// Slot note: HEAD is at 00134 (api_keys_org_bound), so 00143 is the
// next contiguous slot at PR creation time. The migration is
// slot-agnostic — only the filename and the test function name carry
// the literal slot. If a sibling PR grabs 00143 first, renumber per
// `migrations/README.md` and update this test's filename + the
// ApplyUp range below.
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

func TestMigrations_00143_AppsRequireAuthn(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.Open(t)

	// Seed UUIDs carry the slot number in the last group (`...000143`,
	// `...000236`) so a reader scanning the test fixtures can pin each
	// row to this migration without grepping the file name. The literal
	// slot value MUST stay in sync with the filename; renumber per
	// `migrations/README.md` if a sibling PR grabs 00143 first.

	// (1) Apply through 00143. A regression that drops a slot between
	// 1 and 137 surfaces here before the per-assertion pins.
	if err := db.MigrateUp(ctx, pool); err != nil {
		t.Fatalf("db.MigrateUp: %v (regression: missing migration slot between 1 and 139)", err)
	}

	// (2) Seed an account + app. The literal UUIDs are fixed across
	// reruns so the seed is idempotent. App fixture uses the seeded
	// pro plan so the plan-gate check (apid returns 403 on Free/Hobby)
	// is testable downstream without affecting this migration test.
	if _, err := pool.Exec(ctx, `
		insert into accounts (id, email, plan, created_at)
		values ('00000000-0000-0000-0000-000000000143',
		        'require-authn-test@example.com', 'pro', now())
		on conflict (id) do nothing
	`); err != nil {
		t.Fatalf("seed account: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		insert into apps (id, account_id, slug, type, ram_mb, max_concurrency, idle_timeout_s, status, created_at)
		values ('00000000-0000-0000-0000-000000000236',
		        '00000000-0000-0000-0000-000000000143',
		        'require-authn-test-app', 'function', 512, 5, 300, 'active', now())
		on conflict (id) do nothing
	`); err != nil {
		t.Fatalf("seed app: %v", err)
	}

	// (3) Default round-trip: the freshly-inserted app reads
	// require_authn=false (NOT NULL DEFAULT false). This is the
	// regression check that pre-PR rows stay public-by-default — the
	// load-bearing invariant for issue #560 AC #5 (no existing
	// customer breakage).
	var defaultVal bool
	if err := pool.QueryRow(ctx, `
		select require_authn from apps where id = '00000000-0000-0000-0000-000000000236'
	`).Scan(&defaultVal); err != nil {
		t.Fatalf("read default require_authn: %v", err)
	}
	if defaultVal {
		t.Errorf("require_authn default = true, want false (regression: pre-PR rows must stay public)")
	}

	// (4) Opt-in round-trip: PATCH-style UPDATE writes true and reads
	// back true. Mirrors the apid updateApp handler path so a future
	// regression in the write side surfaces here.
	if _, err := pool.Exec(ctx, `
		update apps set require_authn = true where id = '00000000-0000-0000-0000-000000000236'
	`); err != nil {
		t.Fatalf("update require_authn: %v", err)
	}
	var optedIn bool
	if err := pool.QueryRow(ctx, `
		select require_authn from apps where id = '00000000-0000-0000-0000-000000000236'
	`).Scan(&optedIn); err != nil {
		t.Fatalf("read opted-in require_authn: %v", err)
	}
	if !optedIn {
		t.Errorf("require_authn after update = false, want true")
	}

	// (5) Opt-out round-trip: a second UPDATE writes false and reads
	// back false. Mirrors the PATCH--no-require-authn / the plan-default
	// flip path so a future regression in the write side (e.g. a CHECK
	// constraint the planner couldn't see) surfaces here.
	if _, err := pool.Exec(ctx, `
		update apps set require_authn = false where id = '00000000-0000-0000-0000-000000000236'
	`); err != nil {
		t.Fatalf("update require_authn back to false: %v", err)
	}
	var optedOut bool
	if err := pool.QueryRow(ctx, `
		select require_authn from apps where id = '00000000-0000-0000-0000-000000000236'
	`).Scan(&optedOut); err != nil {
		t.Fatalf("read opted-out require_authn: %v", err)
	}
	if optedOut {
		t.Errorf("require_authn after opt-out = true, want false")
	}

	// (6) Replay safety: a second MigrateUp is a no-op (the migration
	// uses ADD COLUMN IF NOT EXISTS). ADR-041 contract — the column
	// is the storage; the apid layer's SetRequireAuthn boolean is the
	// "did the caller touch this" signal at the PATCH layer.
	if err := db.MigrateUp(ctx, pool); err != nil {
		t.Fatalf("replay-safety: second MigrateUp failed: %v", err)
	}
}
