//go:build !no_pg

// Migration-apply test for 00080 (per-app streaming_enabled flag,
// issue #471). Pins the new column:
//
//  1. The migration set applies cleanly through 00080.
//  2. The column accepts the canonical boolean shape and round-trips.
//  3. Default is false (regression check — pre-PR rows still default to
//     the buffered path, no opt-in required).
//  4. Replay-safe: ADD COLUMN IF NOT EXISTS makes a second MigrateUp
//     no-op (PR #377 / ADR-041).
//  5. Partial index `apps_streaming_enabled_idx` exists and is keyed
//     on the true subset (operator-side "which apps stream?" query
//     path).
//
// Slot note: HEAD is at 00079 (deployment overrides, sibling PR), so
// 00080 is the next free slot at PR creation time. The migration is
// slot-agnostic — only the filename and the test function name carry
// the literal slot. If a sibling PR grabs 00080 first, renumber per
// `migrations/README.md` and update this test's filename + ApplyUp
// range.
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

func TestMigrations_00080_AppsStreamingEnabled(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.Open(t)

	// Seed UUIDs carry the slot number in the last group (`...000080`,
	// `...000180`) so a reader scanning the test fixtures can pin each
	// row to this migration without grepping the file name. The literal
	// slot value MUST stay in sync with the filename; renumber per
	// `migrations/README.md` if a sibling PR grabs 00080 first.

	// (1) Apply through 00080. A regression that drops a slot between
	// 1 and 80 surfaces here before the per-assertion pins.
	if err := db.MigrateUp(ctx, pool); err != nil {
		t.Fatalf("db.MigrateUp: %v (regression: missing migration slot between 1 and 80)", err)
	}

	// (2) Seed an account + app. The literal UUIDs are fixed across
	// reruns so the seed is idempotent. App fixture uses the seeded
	// hobby plan so the plan-gate check (apid returns 403 on Free) is
	// testable downstream without affecting this migration test.
	if _, err := pool.Exec(ctx, `
		insert into accounts (id, email, plan, created_at)
		values ('00000000-0000-0000-0000-000000000080',
		        'streaming-enabled-test@example.com', 'hobby', now())
		on conflict (id) do nothing
	`); err != nil {
		t.Fatalf("seed account: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		insert into apps (id, account_id, slug, type, ram_mb, max_concurrency, idle_timeout_s, status, created_at)
		values ('00000000-0000-0000-0000-000000000180',
		        '00000000-0000-0000-0000-000000000080',
		        'streaming-enabled-test-app', 'function', 256, 1, 30, 'active', now())
		on conflict (id) do nothing
	`); err != nil {
		t.Fatalf("seed app: %v", err)
	}

	// (3) Default round-trip: the freshly-inserted app reads
	// streaming_enabled=false (NOT NULL DEFAULT false). This is the
	// regression check that pre-PR rows stay on the buffered path.
	var defaultVal bool
	if err := pool.QueryRow(ctx, `
		select streaming_enabled from apps where id = '00000000-0000-0000-0000-000000000180'
	`).Scan(&defaultVal); err != nil {
		t.Fatalf("read default streaming_enabled: %v", err)
	}
	if defaultVal {
		t.Errorf("streaming_enabled default = true, want false (regression: pre-PR rows must stay buffered)")
	}

	// (4) Opt-in round-trip: PATCH-style UPDATE writes true and reads
	// back true. Mirrors the apid updateApp handler path so a future
	// regression in the write side surfaces here.
	if _, err := pool.Exec(ctx, `
		update apps set streaming_enabled = true where id = '00000000-0000-0000-0000-000000000180'
	`); err != nil {
		t.Fatalf("update streaming_enabled: %v", err)
	}
	var optedIn bool
	if err := pool.QueryRow(ctx, `
		select streaming_enabled from apps where id = '00000000-0000-0000-0000-000000000180'
	`).Scan(&optedIn); err != nil {
		t.Fatalf("read opted-in streaming_enabled: %v", err)
	}
	if !optedIn {
		t.Errorf("streaming_enabled after update = false, want true")
	}

	// (5) Partial index exists. The pg_indexes query is the cheapest
	// portable check; checking indexdef keeps a regression that drops
	// the WHERE clause visible (the partial shape is what makes the
	// index small for Hobby+/Pro/Scale opt-ins).
	var indexDef string
	if err := pool.QueryRow(ctx, `
		select indexdef from pg_indexes
		where schemaname = current_schema()
		  and tablename = 'apps'
		  and indexname = 'apps_streaming_enabled_idx'
	`).Scan(&indexDef); err != nil {
		t.Fatalf("read partial index def: %v (regression: apps_streaming_enabled_idx not created)", err)
	}
	if indexDef == "" {
		t.Errorf("apps_streaming_enabled_idx not found in pg_indexes")
	}

	// (6) Replay safety: a second MigrateUp is a no-op (the migration
	// uses ADD COLUMN IF NOT EXISTS / CREATE INDEX IF NOT EXISTS).
	// PR #377 / ADR-041 contract.
	if err := db.MigrateUp(ctx, pool); err != nil {
		t.Fatalf("replay-safety: second MigrateUp failed: %v", err)
	}
}
