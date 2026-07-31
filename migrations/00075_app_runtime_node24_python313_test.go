//go:build !no_pg

// Migration-apply test for 00075 (Node 24 and Python 3.13 addition to the
// apps.runtime CHECK). Pins the runtime CHECK widening:
//
//  1. The migration set applies cleanly through 00075.
//  2. A new function app with `runtime: node24` round-trips.
//  3. A new function app with `runtime: python313` round-trips.
//  4. Existing runtimes (node22 / python312 / go124 / go124-alpine)
//     remain accepted — no regression on prior behavior.
//  5. A bogus runtime (e.g. 'ruby33') is still rejected with
//     SQLSTATE 23514 against `apps_runtime_check`.
//
// Slot note: HEAD is at 00074 (repo-decomposition / projects), so 00075 is
// the next free slot at PR creation time. The migration is slot-agnostic —
// only the filename and the test function name carry the literal slot.
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

func TestMigrations_00075_AppRuntimeNode24Python313(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.Open(t)

	// (1) Apply through 00075. A regression that drops a slot between
	// 1 and 75 surfaces here before the per-assertion pins.
	if err := db.MigrateUp(ctx, pool); err != nil {
		t.Fatalf("db.MigrateUp: %v (regression: missing migration slot between 1 and 75)", err)
	}

	// (2) Seed an account. The literal UUIDs are fixed across reruns
	// so the seed is idempotent.
	if _, err := pool.Exec(ctx, `
		insert into accounts (id, email, plan, created_at)
		values ('00000000-0000-0000-0000-000000000075',
		        'node24-python313-runtime-test@example.com', 'hobby', now())
		on conflict (id) do nothing
	`); err != nil {
		t.Fatalf("seed account: %v", err)
	}

	// (3a) Insert a function app with runtime=node24. Primary
	// acceptance of the widening for Node 24 LTS.
	if _, err := pool.Exec(ctx, `
		insert into apps (id, account_id, slug, type, runtime, ram_mb, max_concurrency, idle_timeout_s, status, created_at)
		values ('00000000-0000-0000-0000-000000000175',
		        '00000000-0000-0000-0000-000000000075',
		        'node24-function-test', 'function', 'node24', 256, 1, 30, 'active', now())
	`); err != nil {
		t.Fatalf("insert function app with runtime=node24: %v", err)
	}

	// (3b) Insert a function app with runtime=python313. Primary
	// acceptance of the widening for Python 3.13.
	if _, err := pool.Exec(ctx, `
		insert into apps (id, account_id, slug, type, runtime, ram_mb, max_concurrency, idle_timeout_s, status, created_at)
		values ('00000000-0000-0000-0000-000000000275',
		        '00000000-0000-0000-0000-000000000075',
		        'python313-function-test', 'function', 'python313', 256, 1, 30, 'active', now())
	`); err != nil {
		t.Fatalf("insert function app with runtime=python313: %v", err)
	}

	// (4) Read back — pin both values round-trip (no normalization
	// collapse to a sibling runtime).
	cases := []struct {
		appID string
		want  string
	}{
		{"00000000-0000-0000-0000-000000000175", "node24"},
		{"00000000-0000-0000-0000-000000000275", "python313"},
	}
	for _, tc := range cases {
		var runtime string
		if err := pool.QueryRow(ctx, `select runtime from apps where id = $1`, tc.appID).Scan(&runtime); err != nil {
			t.Fatalf("read runtime for %s: %v", tc.appID, err)
		}
		if runtime != tc.want {
			t.Errorf("runtime round-trip for app %s = %q, want %q", tc.appID, runtime, tc.want)
		}
	}

	// (5) Existing runtimes still pass — confirm the constraint
	// widening did not regress node22 / python312 / go124 / go124-alpine.
	olderRuntimes := []struct {
		slug    string
		runtime string
	}{
		{"node22-still-allowed", "node22"},
		{"python312-still-allowed", "python312"},
		{"go124-still-allowed", "go124"},
		{"go124-alpine-still-allowed", "go124-alpine"},
	}
	for _, tc := range olderRuntimes {
		// Each older runtime gets its own app row, on its own UUID, so
		// the regression check is independent per runtime.
		if _, err := pool.Exec(ctx, `
			insert into apps (id, account_id, slug, type, runtime, ram_mb, max_concurrency, status, created_at)
			values (gen_random_uuid(),
			        '00000000-0000-0000-0000-000000000075',
			        $1, 'function', $2, 256, 1, 'active', now())
		`, tc.slug, tc.runtime); err != nil {
			t.Errorf("regression: %s was rejected by widened CHECK: %v", tc.runtime, err)
		}
	}

	// (6) Bogus runtime is still rejected — same SQLSTATE, same
	// constraint name. The widened CHECK is exhaustive over the
	// known runtime strings, not "any value" with a hole punched.
	_, err := pool.Exec(ctx, `
		insert into apps (id, account_id, slug, type, runtime, ram_mb, max_concurrency, status, created_at)
		values (gen_random_uuid(),
		        '00000000-0000-0000-0000-000000000075',
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
