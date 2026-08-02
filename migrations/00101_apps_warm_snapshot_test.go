//go:build !no_pg

// Migration-apply test for 00101AppsWarmSnapshot #470,
// ADR-055). Pins the new columns + CHECK bounds + replay-safety
// contract.
//
// Pins:
//
//  1. The migration set applies cleanly through 00101.
//  2. The three columns accept the canonical shapes and round-trip.
//  3. Defaults are false / 5 / 2000 (regression check — pre-PR rows
//     stay on the init-tier path; warm-snapshot is operator opt-in).
//  4. Opt-in round-trip: UPDATE writes true and reads back true.
//  5. CHECK bounds reject out-of-range values for both min columns.
//  6. Replay-safe: ADD COLUMN IF NOT EXISTS + the DO-block +
//     pg_catalog.pg_constraint guard make a second MigrateUp a no-op
//     (PR #377 / ADR-041; see migrations/00082_apps_scaling_policy.sql
//     and migrations/00074_projects_and_workloads.sql for the
//     precedent — Postgres has no `ADD CONSTRAINT IF NOT EXISTS`).
//
// Slot note: HEAD on origin/main is 00086 (apps_require_signed).
// Slot 101 is the next contiguous real-schema slot at PR-creation
// time; slot 87 is a reserve_slot placeholder per ADR-041. The
// migration itself is slot-agnostic — only the filename, the test
// function name, the seed UUIDs, and e2eMigrationTarget carry the
// literal slot. Bump pkg/e2etest/harness.go::e2eMigrationTarget to
// match the filename when renumbering.
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

func TestMigrations_00101_AppsWarmSnapshot(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.Open(t)

	// Seed UUIDs carry the slot number in the last group (`...000101`,
	// `...000201`) so a reader scanning the test fixtures can pin each
	// row to this migration without grepping the file name. The literal
	// slot value MUST stay in sync with the filename; renumber per
	// `migrations/README.md` if a sibling PR grabs 00101 first.

	// (1) Apply through 00101. A regression that drops a slot between
	// 1 and 101 surfaces here before the per-assertion pins.
	if err := db.MigrateUp(ctx, pool); err != nil {
		t.Fatalf("db.MigrateUp: %v (regression: missing migration slot between 1 and 101)", err)
	}

	// (2) Seed an account + app. The literal UUIDs are fixed across
	// reruns so the seed is idempotent.
	if _, err := pool.Exec(ctx, `
		insert into accounts (id, email, plan, created_at)
		values ('00000000-0000-0000-0000-000000000101',
		        'warm-snapshot-test@example.com', 'pro', now())
		on conflict (id) do nothing
	`); err != nil {
		t.Fatalf("seed account: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		insert into apps (id, account_id, slug, type, ram_mb, max_concurrency, idle_timeout_s, status, created_at)
		values ('00000000-0000-0000-0000-000...000201',
		        '00000000-0000-0000-0000-000000000101',
		        'warm-snapshot-test-app', 'function', 256, 1, 30, 'active', now())
		on conflict (id) do nothing
	`); err != nil {
		t.Fatalf("seed app: %v", err)
	}

	// (3) Default round-trip: the freshly-inserted app reads
	// warm_snapshot_enabled=false, warm_snapshot_min_requests=5,
	// warm_snapshot_min_ms=2000 (NOT NULL with those constants).
	// This is the regression check that pre-PR rows stay on the
	// init-tier path; the per-plan default (Pro/Scale = true) is
	// applied at apid create-time, not by the column default.
	var (
		enabled bool
		minReqs int
		minMs   int
	)
	if err := pool.QueryRow(ctx, `
		select warm_snapshot_enabled, warm_snapshot_min_requests, warm_snapshot_min_ms
		  from apps where id = '00000000-0000-0000-0000-000...000201'
	`).Scan(&enabled, &minReqs, &minMs); err != nil {
		t.Fatalf("read default warm_snapshot_*: %v", err)
	}
	if enabled {
		t.Errorf("warm_snapshot_enabled default = true, want false (regression: pre-PR rows must stay init-tier)")
	}
	if minReqs != 5 {
		t.Errorf("warm_snapshot_min_requests default = %d, want 5", minReqs)
	}
	if minMs != 2000 {
		t.Errorf("warm_snapshot_min_ms default = %d, want 2000", minMs)
	}

	// (4) Opt-in round-trip: UPDATE writes enabled=true + custom
	// min values and reads them back. Mirrors the apid updateApp
	// handler path so a future regression in the write side surfaces
	// here.
	if _, err := pool.Exec(ctx, `
		update apps
		   set warm_snapshot_enabled = true,
		       warm_snapshot_min_requests = 10,
		       warm_snapshot_min_ms = 3000
		 where id = '00000000-0000-0000-0000-000...000201'
	`); err != nil {
		t.Fatalf("update warm_snapshot_*: %v", err)
	}
	var (
		enabled2 bool
		minReqs2 int
		minMs2   int
	)
	if err := pool.QueryRow(ctx, `
		select warm_snapshot_enabled, warm_snapshot_min_requests, warm_snapshot_min_ms
		  from apps where id = '00000000-0000-0000-0000-000...000201'
	`).Scan(&enabled2, &minReqs2, &minMs2); err != nil {
		t.Fatalf("read opted-in warm_snapshot_*: %v", err)
	}
	if !enabled2 {
		t.Errorf("warm_snapshot_enabled after update = false, want true")
	}
	if minReqs2 != 10 {
		t.Errorf("warm_snapshot_min_requests after update = %d, want 10", minReqs2)
	}
	if minMs2 != 3000 {
		t.Errorf("warm_snapshot_min_ms after update = %d, want 3000", minMs2)
	}

	// (5) CHECK bounds: warm_snapshot_min_requests BETWEEN 1 AND 100.
	if _, err := pool.Exec(ctx, `
		update apps set warm_snapshot_min_requests = 0
		 where id = '00000000-0000-0000-0000-000...000201'
	`); err == nil {
		t.Errorf("warm_snapshot_min_requests=0 should be rejected by CHECK (lower bound)")
	}
	if _, err := pool.Exec(ctx, `
		update apps set warm_snapshot_min_requests = 101
		 where id = '00000000-0000-0000-0000-000...000201'
	`); err == nil {
		t.Errorf("warm_snapshot_min_requests=101 should be rejected by CHECK (upper bound)")
	}

	// (6) CHECK bounds: warm_snapshot_min_ms BETWEEN 100 AND 60000.
	if _, err := pool.Exec(ctx, `
		update apps set warm_snapshot_min_ms = 50
		 where id = '00000000-0000-0000-0000-000...000201'
	`); err == nil {
		t.Errorf("warm_snapshot_min_ms=50 should be rejected by CHECK (lower bound)")
	}
	if _, err := pool.Exec(ctx, `
		update apps set warm_snapshot_min_ms = 70000
		 where id = '00000000-0000-0000-0000-000...000201'
	`); err == nil {
		t.Errorf("warm_snapshot_min_ms=70000 should be rejected by CHECK (upper bound)")
	}

	// (7) Replay safety: a second MigrateUp is a no-op. The migration
	// uses ADD COLUMN IF NOT EXISTS for the three columns and a
	// DO-block + pg_catalog.pg_constraint lookup for the two CHECK
	// constraints (Postgres has no `ADD CONSTRAINT IF NOT EXISTS`).
	// PR #377 / ADR-041 contract; precedent at
	// migrations/00082_apps_scaling_policy.sql:50-77.
	if err := db.MigrateUp(ctx, pool); err != nil {
		t.Fatalf("replay-safety: second MigrateUp failed: %v", err)
	}
}
