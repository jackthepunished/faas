//go:build !no_pg

// Migration-apply test for 00138 (issue #475 — eviction_priority).
// Pins the new column + CHECK constraint + default + replay-safety
// contract.
//
// Pins:
//
//  1. The migration set applies cleanly through 00138.
//  2. The column is NOT NULL with default 'best_effort' (pre-#475 rows
//     stay on the historical LRU path bit-for-bit; the per-plan default
//     is applied at apid create-time, not by the column default).
//  3. CHECK accepts both 'best_effort' and 'reserved'; rejects 'foo'
//     with 23514 (check_violation).
//  4. NOT NULL rejects NULL with 23502 (not_null_violation).
//  5. Opt-in round-trip: UPDATE writes 'reserved' and reads back
//     'reserved'.
//  6. Replay-safe: ADD COLUMN IF NOT EXISTS + the DO-block +
//     pg_catalog.pg_constraint guard make a second MigrateUp a no-op
//     (PR #377 / ADR-041; see migrations/00082_apps_scaling_policy.sql,
//     00109_apps_warm_snapshot.sql, 00074_projects_and_workloads.sql).
//
// Slot note: HEAD on origin/main is 00134 (api_keys_org_bound).
// Slot 135 is the next contiguous real-schema slot at PR-creation
// time. The migration itself is slot-agnostic — only the filename,
// the test function name, the seed UUIDs, and e2eMigrationTarget
// carry the literal slot. Bump pkg/e2etest/harness.go::
// e2eMigrationTarget to match the filename when renumbering.
//
// Build tag matches the rest of the migration tests; set
// FAAS_SKIP_PG_TESTS=1 to skip locally (see migrations/README.md).
package migrations_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/onebox-faas/faas/pkg/db"
	"github.com/onebox-faas/faas/pkg/db/pgtest"
)

func TestMigrations_00138_AppsEvictionPriority(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.Open(t)

	// Seed UUIDs carry the slot number in the last group (`...000138`,
	// `...000238`) so a reader scanning the test fixtures can pin each
	// row to this migration without grepping the file name. The literal
	// slot value MUST stay in sync with the filename; renumber per
	// `migrations/README.md` if a sibling PR grabs 00138 first.

	// (1) Apply through 00138. A regression that drops a slot between
	// 1 and 135 surfaces here before the per-assertion pins.
	if err := db.MigrateUp(ctx, pool); err != nil {
		t.Fatalf("db.MigrateUp: %v (regression: missing migration slot between 1 and 135)", err)
	}

	// (2) Seed an account + app. The literal UUIDs are fixed across
	// reruns so the seed is idempotent.
	if _, err := pool.Exec(ctx, `
		insert into accounts (id, email, plan, created_at)
		values ('00000000-0000-0000-0000-000000000138',
		        'eviction-priority-test@example.com', 'pro', now())
		on conflict (id) do nothing
	`); err != nil {
		t.Fatalf("seed account: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		insert into apps (id, account_id, slug, type, ram_mb, max_concurrency, idle_timeout_s, status, created_at)
		values ('00000000-0000-0000-0000-000000000238',
		        '00000000-0000-0000-0000-000000000138',
		        'eviction-priority-test-app', 'function', 256, 1, 30, 'active', now())
		on conflict (id) do nothing
	`); err != nil {
		t.Fatalf("seed app: %v", err)
	}

	// (3) Default round-trip: the freshly-inserted app reads
	// eviction_priority='best_effort'. This is the regression check
	// that pre-PR rows stay on the historical LRU path; the plan
	// default (Free/Hobby not allowed; Pro/Scale may opt in) is
	// enforced at apid, not by the column default.
	var got string
	if err := pool.QueryRow(ctx, `
		select eviction_priority
		  from apps where id = '00000000-0000-0000-0000-000000000238'
	`).Scan(&got); err != nil {
		t.Fatalf("read default eviction_priority: %v", err)
	}
	if got != "best_effort" {
		t.Errorf("eviction_priority default = %q, want \"best_effort\" (regression: pre-PR rows must stay on the LRU path)", got)
	}

	// (4) Opt-in round-trip: UPDATE writes 'reserved' and reads it
	// back. Mirrors the apid updateApp handler path so a future
	// regression in the write side surfaces here.
	if _, err := pool.Exec(ctx, `
		update apps
		   set eviction_priority = 'reserved'
		 where id = '00000000-0000-0000-0000-000000000238'
	`); err != nil {
		t.Fatalf("update eviction_priority: %v", err)
	}
	var got2 string
	if err := pool.QueryRow(ctx, `
		select eviction_priority
		  from apps where id = '00000000-0000-0000-0000-000000000238'
	`).Scan(&got2); err != nil {
		t.Fatalf("read opted-in eviction_priority: %v", err)
	}
	if got2 != "reserved" {
		t.Errorf("eviction_priority after update = %q, want \"reserved\"", got2)
	}

	// (5) CHECK rejects values outside the closed set with 23514.
	// Mirrors the apps_workload_class_chk constraint test in
	// migration 00086.
	_, err := pool.Exec(ctx, `
		update apps set eviction_priority = 'foo'
		 where id = '00000000-0000-0000-0000-000000000238'
	`)
	var pgErr *pgconn.PgError
	if err == nil {
		t.Errorf("eviction_priority='foo' should be rejected by CHECK")
	} else if !errors.As(err, &pgErr) || pgErr.Code != "23514" {
		t.Errorf("eviction_priority='foo' error = %v, want pgx 23514 (check_violation)", err)
	}

	// (6) NOT NULL rejects NULL with 23502. The column is declared
	// NOT NULL with DEFAULT 'best_effort' so a fresh INSERT with an
	// explicit NULL must fail — same regression shape as
	// apps_workload_class NOT NULL.
	_, err = pool.Exec(ctx, `
		insert into apps (id, account_id, slug, type, ram_mb, max_concurrency, status, created_at, eviction_priority)
		values ('00000000-0000-0000-0000-000000000338',
		        '00000000-0000-0000-0000-000000000138',
		        'eviction-priority-null-app', 'function', 256, 1, 'active', now(), NULL)
	`)
	if err == nil {
		t.Errorf("eviction_priority=NULL should be rejected by NOT NULL")
	} else if !errors.As(err, &pgErr) || pgErr.Code != "23502" {
		t.Errorf("eviction_priority=NULL error = %v, want pgx 23502 (not_null_violation)", err)
	}

	// (7) Replay safety: a second MigrateUp is a no-op. The migration
	// uses ADD COLUMN IF NOT EXISTS for the column and a DO-block +
	// pg_catalog.pg_constraint lookup for the CHECK constraint
	// (Postgres has no `ADD CONSTRAINT IF NOT EXISTS`). PR #377 /
	// ADR-041 contract; precedent at
	// migrations/00082_apps_scaling_policy.sql:50-77.
	if err := db.MigrateUp(ctx, pool); err != nil {
		t.Fatalf("replay-safety: second MigrateUp failed: %v", err)
	}
}
