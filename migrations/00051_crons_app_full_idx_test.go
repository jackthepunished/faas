//go:build !no_pg

// Backfill-semantics test for migration 00050 (PR #340 follow-up).
//
// Asserts the non-partial index crons_app_full_idx (app_id) exists on
// crons after the migration set applies. The companion partial index
// crons_app_idx (app_id) WHERE enabled stays untouched — schedd's
// ListEnabledCrons scan still relies on it.
//
// Build tag mirrors apply_walk_test.go:4 — set FAAS_SKIP_PG_TESTS=1
// locally to skip.

package migrations_test

import (
	"context"
	"testing"

	"github.com/onebox-faas/faas/pkg/db"
	"github.com/onebox-faas/faas/pkg/db/pgtest"
)

func TestMigrations_00050_CronsAppFullIdx_LandsNonPartialIndex(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.Open(t)
	if err := db.MigrateUp(ctx, pool); err != nil {
		t.Fatalf("db.MigrateUp: %v", err)
	}

	// (1) The new non-partial index exists on crons(app_id).
	//     table_schema = current_schema() follows the canonical
	//     pattern in apply_walk_test.go:124 — without it, a
	//     multi-schema dev box returns a stale row from a previous
	//     test's schema and the assertion passes-by-accident.
	var idxName string
	if err := pool.QueryRow(ctx, `
		select indexname
		  from pg_indexes
		 where schemaname = current_schema()
		   and tablename = 'crons'
		   and indexname = 'crons_app_full_idx'
	`).Scan(&idxName); err != nil {
		t.Fatalf("crons_app_full_idx not present after migrations apply: %v", err)
	}

	// (2) The new index is non-partial — i.e. it covers disabled rows
	//     too, which is what CreateCronIfUnderQuota's count(*) needs.
	//     pg_indexes doesn't expose the WHERE clause; pg_index has
	//     indpred IS NULL for a plain (non-partial) index.
	var indpred any
	if err := pool.QueryRow(ctx, `
		select pg_index.indpred
		  from pg_index
		  join pg_class on pg_class.oid = pg_index.indexrelid
		  join pg_namespace on pg_namespace.oid = pg_class.relnamespace
		 where pg_namespace.nspname = current_schema()
		   and pg_class.relname = $1
	`, idxName).Scan(&indpred); err != nil {
		t.Fatalf("pg_index lookup: %v", err)
	}
	if indpred != nil {
		t.Errorf("crons_app_full_idx indpred = %v, want NULL (non-partial)", indpred)
	}

	// (3) The pre-existing partial index crons_app_idx is still
	//     present (regression guard — schedd's ListEnabledCrons scan
	//     depends on it).
	var partialIdx string
	if err := pool.QueryRow(ctx, `
		select indexname
		  from pg_indexes
		 where schemaname = current_schema()
		   and tablename = 'crons'
		   and indexname = 'crons_app_idx'
	`).Scan(&partialIdx); err != nil {
		t.Fatalf("crons_app_idx (partial) missing — regression: %v", err)
	}
}
