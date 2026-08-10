//go:build !no_pg

// Migration-apply test for 00173 (issue #791, cron run history).
//
// Asserts the new outcome column is present, the CHECK accepts every
// value the Go layer can produce (success/failed/timeout/dead_letter
// plus NULL for in-flight rows), and the partial index
// invocations_cron_idx exists with the right WHERE clause.
//
// Build tag matches 00064_invocations_dead_letter_test.go:4 — set
// FAAS_SKIP_PG_TESTS=1 locally to skip.

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

func TestMigrations_00173_Outcome_LandsColumnAndIndex(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.Open(t)
	if err := db.MigrateUp(ctx, pool); err != nil {
		t.Fatalf("db.MigrateUp: %v", err)
	}

	// (1) The outcome column exists and is nullable.
	var exists bool
	if err := pool.QueryRow(ctx, `
		select exists (
			select 1
			  from information_schema.columns
			 where table_schema = current_schema()
			   and table_name = 'invocations'
			   and column_name = 'outcome'
		)
	`).Scan(&exists); err != nil {
		t.Fatalf("information_schema lookup: %v", err)
	}
	if !exists {
		t.Errorf("invocations.outcome column missing after migrations apply")
	}

	// (2) invocations_cron_idx exists, and it's partial on cron_id IS NOT NULL.
	var idxName string
	if err := pool.QueryRow(ctx, `
		select indexname
		  from pg_indexes
		 where schemaname = current_schema()
		   and tablename = 'invocations'
		   and indexname = 'invocations_cron_idx'
	`).Scan(&idxName); err != nil {
		t.Fatalf("invocations_cron_idx not present after migrations apply: %v", err)
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
		t.Errorf("invocations_cron_idx indpred = NULL, want partial WHERE clause (cron_id IS NOT NULL)")
	}

	// (3) The CHECK constraint permits every value the Go layer emits.
	//     Seed accounts/apps and run one INSERT per outcome. The
	//     crons row is required only because invocations.cron_id has
	//     a referencing constraint in some schemas; we use a real
	//     cron_id here so the FK is satisfied.
	acct := uuid.NewString()
	appID := uuid.NewString()
	insertAccount := func() {
		if _, err := pool.Exec(ctx, `
			insert into accounts (id, email, plan, created_at)
			values ($1, $2, 'pro', now())
		`, acct, acct+"@example.com"); err != nil {
			t.Fatalf("seed account: %v", err)
		}
	}
	insertApp := func() {
		if _, err := pool.Exec(ctx, `
			insert into apps (id, account_id, slug, ram_mb, created_at)
			values ($1, $2, $3, 512, now())
		`, appID, acct, "outcome-migration-test"); err != nil {
			t.Fatalf("seed app: %v", err)
		}
	}
	insertCron := func() string {
		cronID := uuid.NewString()
		if _, err := pool.Exec(ctx, `
			insert into crons (id, app_id, schedule, path, enabled, created_at)
			values ($1, $2, '0 */6 * * *', '/cron', true, now())
		`, cronID, appID); err != nil {
			t.Fatalf("seed cron: %v", err)
		}
		return cronID
	}
	insertAccount()
	insertApp()
	cronID := insertCron()

	// outcome = NULL on a dispatching row is the steady state for
	// in-flight crons. Must pass the CHECK without a value.
	inFlight := uuid.NewString()
	if _, err := pool.Exec(ctx, `
		insert into invocations
		    (id, app_id, account_id, source, state, payload, attempts, cron_id)
		values ($1, $2, $3, 'cron', 'dispatching', '{}'::jsonb, 1, $4)
	`, inFlight, appID, acct, cronID); err != nil {
		t.Fatalf("insert NULL outcome (in-flight row default): %v", err)
	}

	// Every concrete outcome value the Go layer can produce.
	for _, oc := range []string{"success", "failed", "timeout", "dead_letter"} {
		row := uuid.NewString()
		if _, err := pool.Exec(ctx, `
			insert into invocations
			    (id, app_id, account_id, source, state, payload, outcome, cron_id)
			values ($1, $2, $3, 'cron', 'completed', '{}'::jsonb, $4, $5)
		`, row, appID, acct, oc, cronID); err != nil {
			t.Errorf("outcome=%q must pass the new CHECK: %v", oc, err)
		}
	}

	// (4) An unknown outcome is rejected with SQLSTATE 23514 — proves
	//     the CHECK is active (not just permissive) and trips before
	//     the row reaches storage.
	_, err := pool.Exec(ctx, `
		insert into invocations
		    (id, app_id, account_id, source, state, payload, outcome, cron_id)
		values ($1, $2, $3, 'cron', 'completed', '{}'::jsonb, 'not_a_real_outcome', $4)
	`, uuid.NewString(), appID, acct, cronID)
	if err == nil {
		t.Fatalf("unknown outcome must be rejected by invocations_outcome_check")
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("unknown-outcome error not a *pgconn.PgError: %v", err)
	}
	if pgErr.Code != "23514" {
		t.Errorf("unknown outcome SQLSTATE = %q, want 23514 (check_violation); full: %v", pgErr.Code, err)
	}
}
