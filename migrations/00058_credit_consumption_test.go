//go:build !no_pg

// Migration-apply test for 00058 (issue #279 PR-C, credit consumption).
//
// Asserts the unique partial index credit_ledger_invoice_credit_idx
// (provider_invoice_id, credit_id) WHERE provider_invoice_id IS NOT NULL
// exists on credit_ledger after the migration set applies, and that the
// constraint rejects a second row for the same (provider_invoice_id,
// credit_id) pair — the dedupe that closes the idempotency story for
// webhook redelivery and admin endpoint replay.
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

func TestMigrations_00058_CreditConsumption_LandsUniquePartialIndex(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.Open(t)
	if err := db.MigrateUp(ctx, pool); err != nil {
		t.Fatalf("db.MigrateUp: %v", err)
	}

	// (1) The new partial unique index exists on credit_ledger.
	//     schemaname = current_schema() follows the canonical pattern
	//     in apply_walk_test.go:124 — without it, a multi-schema dev
	//     box returns a stale row from a previous test's schema and
	//     the assertion passes-by-accident.
	var idxName string
	if err := pool.QueryRow(ctx, `
		select indexname
		  from pg_indexes
		 where schemaname = current_schema()
		   and tablename = 'credit_ledger'
		   and indexname = 'credit_ledger_invoice_credit_idx'
	`).Scan(&idxName); err != nil {
		t.Fatalf("credit_ledger_invoice_credit_idx not present after migrations apply: %v", err)
	}

	// (2) The index is partial — pg_index.indpred is non-NULL when the
	//     index carries a WHERE clause. The reducer relies on this
	//     partial shape so issuance rows (provider_invoice_id NULL) do
	//     NOT participate in the uniqueness check.
	var indpred any
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
		t.Errorf("credit_ledger_invoice_credit_idx indpred = NULL, want partial WHERE clause (provider_invoice_id IS NOT NULL)")
	}

	// (3) provider_invoice_id column exists on credit_ledger.
	//     Issuance rows are NULL; consumption rows set it. The column
	//     being nullable is the contract that keeps issuance free of
	//     the partial unique constraint.
	var hasColumn bool
	if err := pool.QueryRow(ctx, `
		select exists (
			select 1
			  from information_schema.columns
			 where table_schema = current_schema()
			   and table_name = 'credit_ledger'
			   and column_name = 'provider_invoice_id'
		)
	`).Scan(&hasColumn); err != nil {
		t.Fatalf("information_schema.columns lookup: %v", err)
	}
	if !hasColumn {
		t.Errorf("credit_ledger.provider_invoice_id column missing after migrations apply")
	}

	// (4) Dedupe round-trip — seed two accounts + one credit each, then
	//     insert two consumption rows that share the same
	//     (provider_invoice_id, credit_id) pair. The second insert
	//     must fail with SQLSTATE 23505 (unique_violation); the
	//     first insert succeeds and is left as cleanup.
	acctA := uuid.NewString()
	acctB := uuid.NewString()
	creditA := uuid.NewString()
	creditB := uuid.NewString()
	for _, acct := range []string{acctA, acctB} {
		if _, err := pool.Exec(ctx, `
			insert into accounts (id, email, plan, created_at)
			values ($1, $2, 'pro', now())
		`, acct, acct+"@example.com"); err != nil {
			t.Fatalf("seed account: %v", err)
		}
	}
	if _, err := pool.Exec(ctx, `
		insert into account_credits (id, account_id, cents_remaining, reason, created_at)
		values ($1, $2, 100, 'consumption-reducer-test', now())
	`, creditA, acctA); err != nil {
		t.Fatalf("seed account_credit A: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		insert into account_credits (id, account_id, cents_remaining, reason, created_at)
		values ($1, $2, 200, 'consumption-reducer-test', now())
	`, creditB, acctB); err != nil {
		t.Fatalf("seed account_credit B: %v", err)
	}

	const providerInvoiceID = "in_test_consumption_reducer"
	if _, err := pool.Exec(ctx, `
		insert into credit_ledger (account_id, credit_id, delta_cents, reason, actor, provider_invoice_id)
		values ($1, $2, -50, 'first consumption', 'apid', $3)
	`, acctA, creditA, providerInvoiceID); err != nil {
		t.Fatalf("first consumption insert: %v", err)
	}

	// Second insert for the SAME (provider_invoice_id, credit_id) pair
	// must be rejected. The reducer uses ON CONFLICT DO NOTHING; this
	// test pins the underlying constraint so a future migration can't
	// silently drop it without a CI failure.
	_, err := pool.Exec(ctx, `
		insert into credit_ledger (account_id, credit_id, delta_cents, reason, actor, provider_invoice_id)
		values ($1, $2, -50, 'duplicate consumption (must fail)', 'apid', $3)
	`, acctA, creditA, providerInvoiceID)
	if err == nil {
		t.Fatalf("duplicate (provider_invoice_id, credit_id) must be rejected by credit_ledger_invoice_credit_idx")
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("duplicate error not a *pgconn.PgError: %v", err)
	}
	if pgErr.Code != "23505" {
		t.Errorf("duplicate (provider_invoice_id, credit_id) SQLSTATE = %q, want 23505 (unique_violation); full: %v", pgErr.Code, err)
	}

	// (5) Issuance rows (provider_invoice_id NULL) are NOT subject to
	//     the partial constraint — multiple issuance rows for the same
	//     credit_id with NULL provider_invoice_id must all succeed.
	//     This is the path that PR #337 / #279 PR-A relies on; without
	//     it the issuance surface would be impossible.
	if _, err := pool.Exec(ctx, `
		insert into credit_ledger (account_id, credit_id, delta_cents, reason, actor, provider_invoice_id)
		values ($1, $2, 500, 'issuance 1', 'apid', NULL)
	`, acctB, creditB); err != nil {
		t.Errorf("issuance row 1 with provider_invoice_id NULL must succeed: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		insert into credit_ledger (account_id, credit_id, delta_cents, reason, actor, provider_invoice_id)
		values ($1, $2, 600, 'issuance 2', 'apid', NULL)
	`, acctB, creditB); err != nil {
		t.Errorf("issuance row 2 with provider_invoice_id NULL must succeed: %v", err)
	}
}
