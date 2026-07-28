//go:build !no_pg

// Migration-apply test for 00060 (issue #394, queue introspection).
//
// Asserts the new CHECK allows 'dead_letter' as a terminal state and
// that the new partial index invocations_app_dead_letter_idx exists
// with the exact shape the QueueDeadLetter read depends on.
//
// Build tag mirrors apply_walk_test.go:4 — set FAAS_SKIP_PG_TESTS=1
// locally to skip.

package migrations_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/onebox-faas/faas/pkg/db"
	"github.com/onebox-faas/faas/pkg/db/pgtest"
)

func TestMigrations_00060_DeadLetter_LandsStateAndIndex(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.Open(t)
	if err := db.MigrateUp(ctx, pool); err != nil {
		t.Fatalf("db.MigrateUp: %v", err)
	}

	// (1) invocations_app_dead_letter_idx exists, and it's partial.
	//     schemaname = current_schema() follows the canonical pattern
	//     in apply_walk_test.go:124 — without it, a multi-schema dev
	//     box returns a stale row from a previous test's schema and
	//     the assertion passes-by-accident.
	var idxName string
	if err := pool.QueryRow(ctx, `
		select indexname
		  from pg_indexes
		 where schemaname = current_schema()
		   and tablename = 'invocations'
		   and indexname = 'invocations_app_dead_letter_idx'
	`).Scan(&idxName); err != nil {
		t.Fatalf("invocations_app_dead_letter_idx not present after migrations apply: %v", err)
	}

	var indpred *string
	if err := pool.QueryRow(ctx, `
		select pg_index.indpred::text
		  from pg_index
		  join pg_class on pg_class.oid = pg_index.indexrelid
		  join pg_namespace on pg_namespace.oid = pg_class.relnamespace
		 where pg_namespace.nspname = current_schema()
		   and pg_class.relname = $1
	`, idxName).Scan(&indpred); err != nil {
		t.Fatalf("pg_index lookup: %v", err)
	}
	if indpred == nil {
		t.Errorf("invocations_app_dead_letter_idx indpred = NULL, want partial WHERE clause (state = 'dead_letter')")
	}

	// (2) The CHECK constraint permits 'dead_letter' as a state value.
	//     Seed a row with state='dead_letter' and assert it inserts
	//     without complaint. The negative case (an unknown state) is
	//     tested implicitly because the table-level CHECK trips before
	//     we get here — no extra seed needed.
	acct := uuid.NewString()
	appID := uuid.NewString()
	if _, err := pool.Exec(ctx, `
		insert into accounts (id, email, plan, created_at)
		values ($1, $2, 'pro', now())
	`, acct, acct+"@example.com"); err != nil {
		t.Fatalf("seed account: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		insert into apps (id, account_id, slug, plan, ram_mb, created_at)
		values ($1, $2, $3, 'pro', 512, now())
	`, appID, acct, "dead-letter-test"); err != nil {
		t.Fatalf("seed app: %v", err)
	}
	rowID := uuid.NewString()
	if _, err := pool.Exec(ctx, `
		insert into invocations (id, app_id, account_id, source, state, payload, attempts, last_error, completed_at)
		values ($1, $2, $3, 'queue', 'dead_letter', '{}'::jsonb, 3, 'budget exhausted', now())
	`, rowID, appID, acct); err != nil {
		t.Fatalf("insert state='dead_letter' (must succeed under the new CHECK): %v", err)
	}

	// (3) The pre-existing states still pass the CHECK — guard against
	//     an over-broad DROP/ADD that swallowed values.
	for _, st := range []string{"pending", "dispatching", "completed", "failed", "cancelled"} {
		row := uuid.NewString()
		if _, err := pool.Exec(ctx, `
			insert into invocations (id, app_id, account_id, source, state, payload)
			values ($1, $2, $3, 'queue', $4, '{}'::jsonb)
		`, row, appID, acct, st); err != nil {
			t.Errorf("legacy state %q must still pass the new CHECK: %v", st, err)
		}
	}

	// (4) An unknown state value is rejected with SQLSTATE 23514
	//     (check_violation) — proves the CHECK is active (not just
	//     permissive) and trips before the row reaches the storage.
	_, err := pool.Exec(ctx, `
		insert into invocations (id, app_id, account_id, source, state, payload)
		values ($1, $2, $3, 'queue', 'not_a_real_state', '{}'::jsonb)
	`, uuid.NewString(), appID, acct)
	if err == nil {
		t.Fatalf("unknown state must be rejected by invocations_state_check")
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("unknown-state error not a *pgconn.PgError: %v", err)
	}
	if pgErr.Code != "23514" {
		t.Errorf("unknown state SQLSTATE = %q, want 23514 (check_violation); full: %v", pgErr.Code, err)
	}
}
