//go:build !no_pg

// Migration-apply test for 00043 (Go 1.24 alpine variant of the
// go124 runtime). Pins the runtime CHECK widening:
//
//   1. The migration set applies cleanly through 00043.
//   2. A new function app with `runtime: go124-alpine` round-trips.
//   3. The widened constraint accepts 'go124-alpine' alongside the
//      older three runtimes.
//   4. A bogus runtime (e.g. 'ruby33') is still rejected with
//      SQLSTATE 23514 against `apps_runtime_check`.
//
// Slot note: this branch planned 00038, but origin/main landed
// 00038_oauth_links / 00039_account_passwords / 00040_rename_stripe /
// 00041_paddle_overage / 00042_builds_running_idx between plan
// approval and PR push. 00043 is the next free slot on origin/main.
// Renumber per the precedent set by PRs #153, #159, #175, #179, #180
// (see `migration-slot-pr180-final-37-38.md`) and the
// `Migration slot renumber at PR creation` memory entry. The
// migration is slot-agnostic — the only places the literal `38`
// appears are the filename and the test function name.
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

func TestMigrations_00043_AppRuntimeGo124Alpine(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.Open(t)

	// (1) Apply through 00043. A regression that drops a slot between
	// 1 and 43 surfaces here before the per-assertion pins.
	if err := db.MigrateUp(ctx, pool); err != nil {
		t.Fatalf("db.MigrateUp: %v (regression: missing migration slot between 1 and 43)", err)
	}

	// (2) Seed an account. The literal UUIDs are fixed across reruns
	// so the seed is idempotent.
	if _, err := pool.Exec(ctx, `
		insert into accounts (id, email, plan, created_at)
		values ('00000000-0000-0000-0000-000000000043',
		        'go124-alpine-runtime-test@example.com', 'hobby', now())
		on conflict (id) do nothing
	`); err != nil {
		t.Fatalf("seed account: %v", err)
	}

	// (3) Insert a function app with runtime=go124-alpine. This is the
	// primary acceptance: the widened CHECK accepts 'go124-alpine'.
	if _, err := pool.Exec(ctx, `
		insert into apps (id, account_id, slug, type, runtime, ram_mb, max_concurrency, idle_timeout_s, status, created_at)
		values ('00000000-0000-0000-0000-000000000043',
		        '00000000-0000-0000-0000-000000000043',
		        'go124-alpine-function-test', 'function', 'go124-alpine', 256, 1, 30, 'active', now())
	`); err != nil {
		t.Fatalf("insert function app with runtime=go124-alpine: %v", err)
	}

	// (4) Read back — pin the value round-trips (no normalization
	// collapse to a sibling runtime).
	var runtime string
	if err := pool.QueryRow(ctx, `
		select runtime from apps where id = '00000000-0000-0000-0000-000000000043'
	`).Scan(&runtime); err != nil {
		t.Fatalf("read runtime: %v", err)
	}
	if runtime != "go124-alpine" {
		t.Errorf("runtime round-trip = %q, want %q", runtime, "go124-alpine")
	}

	// (5) Existing runtimes still pass — confirm the constraint
	// widening did not regress node22 / python312 / go124.
	if _, err := pool.Exec(ctx, `
		insert into apps (id, account_id, slug, type, runtime, ram_mb, max_concurrency, status, created_at)
		values ('00000000-0000-0000-0000-000000000143',
		        '00000000-0000-0000-0000-000000000043',
		        'node22-still-allowed', 'function', 'node22', 256, 1, 'active', now())
	`); err != nil {
		t.Errorf("regression: node22 was rejected by widened CHECK: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		insert into apps (id, account_id, slug, type, runtime, ram_mb, max_concurrency, status, created_at)
		values ('00000000-0000-0000-0000-000000000243',
		        '00000000-0000-0000-0000-000000000043',
		        'python312-still-allowed', 'function', 'python312', 256, 1, 'active', now())
	`); err != nil {
		t.Errorf("regression: python312 was rejected by widened CHECK: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		insert into apps (id, account_id, slug, type, runtime, ram_mb, max_concurrency, status, created_at)
		values ('00000000-0000-0000-0000-000000000343',
		        '00000000-0000-0000-0000-000000000043',
		        'go124-still-allowed', 'function', 'go124', 256, 1, 'active', now())
	`); err != nil {
		t.Errorf("regression: go124 was rejected by widened CHECK: %v", err)
	}

	// (6) Bogus runtime is still rejected — same SQLSTATE, same
	// constraint name. The widened CHECK is exhaustive over the
	// known runtime strings, not "any value" with a hole punched.
	_, err := pool.Exec(ctx, `
		insert into apps (id, account_id, slug, type, runtime, ram_mb, max_concurrency, status, created_at)
		values ('00000000-0000-0000-0000-000000000443',
		        '00000000-0000-0000-0000-000000000043',
		        'ruby33-should-fail', 'function', 'ruby33', 256, 1, 'active', now())
	`)
	if err == nil {
		t.Fatalf("UPDATE with runtime='ruby33' unexpectedly succeeded; apps_runtime_check did not fire")
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("bogus runtime error not a *pgconn.PgError: %v", err)
	}
	if pgErr.Code != "23514" {
		t.Errorf("bogus runtime SQLSTATE = %q, want %q (check_violation); full: %v", pgErr.Code, "23514", err)
	}
	if pgErr.ConstraintName != "apps_runtime_check" {
		t.Errorf("bogus runtime constraint name = %q, want %q; full: %v", pgErr.ConstraintName, "apps_runtime_check", err)
	}
}
