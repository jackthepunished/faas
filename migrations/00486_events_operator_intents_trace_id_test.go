//go:build !no_pg

// Migration 00486 — adds the `trace_id` column to both `events`
// (the live audit log) and `operator_intents` (the dispatch table)
// so the operator-action observability layer can join enqueue ↔
// terminal-outcome audit rows on one column.
//
// Slot history: this migration has been renumbered six times
// during the post-PR-#1111 rebase cycle as adjacent slots on
// main + other open PRs got claimed:
//
//   1. Originally 00456 when the branch was created against
//      pre-PR-#1115 main.
//   2. Renumbered to 00469 to clear main's bridge-reservation
//      fence at 00456.
//   3. Renumbered to 00472 once main consumed 00469-00471
//      (runtime configuration + snapshot replicas).
//   4. Renumbered to 00475 after the post-rebase CI flagged
//      open PRs #1090 (CORS preset FK), #1064 (deployment audit
//      + 90-day backfill), and #1083 (alert-presets enable
//      signals) each claiming slots in the 00472-00474 cluster.
//      Main now owns through 00471; the 00472 and 00474 gaps
//      are allowlisted in the migrations-contiguity gate
//      (other PRs' claims); the fence parked at 00473 fills
//      the unallowlisted gap between them.
//   5. Renumbered to 00484 after main commit 2d7eaffd9
//      (`fix: restore contiguous migration reservation slot`)
//      parked a reservation fence at 00475, occupying the
//      slot this migration had taken. 00484 is the next free
//      slot past main's 00483 schema-integrity repair
//      migration.
//   6. Renumbered to 00486 after PR #1136 (grype cache fix)
//      merged into main, adding 00485_deployments_canary_state.sql
//      (real) on top of 00484_reserve_slot.sql (fence, also new
//      on main). 00484 was already taken by both main's
//      reservation fence and the prior rebase's 00484 placement
//      of this migration, so the slot-collision gate would have
//      failed locally AND cross-PR. 00486 sits one slot above
//      main's 00485 canary-state migration and two slots above
//      the 00484 reservation fence; no other open PR claims
//      00486. This migration now lives at 00486.
//
// Pins:
//
//  1. `events.trace_id` exists, is `text`, and is nullable (the
//     column is added without a default; pre-PR rows stay NULL).
//  2. The regex CHECK constraint `^[0-9a-f]{32}$` is present on
//     `events.trace_id` — guards against a future migration
//     widening the format accidentally.
//  3. The partial index `events_trace_idx` exists with
//     `WHERE trace_id IS NOT NULL` — same shape as
//     `request_telemetry_trace_idx` at 00427.
//  4. `operator_intents.trace_id` exists, is `text`, nullable,
//     has the same regex CHECK, and `operator_intents_trace_idx`
//     exists with the same partial predicate.
//  5. Replay-safety: second `MigrateUp` is a no-op (every Up
//     uses IF NOT EXISTS).
//  6. Constraint enforcement: a row with a non-hex trace_id is
//     rejected with SQLSTATE 23514 (check_violation) on INSERT.
//
// Build tag matches the rest of the migration tests; set
// FAAS_SKIP_PG_TESTS=1 to skip locally (see migrations/README.md).

package migrations_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/onebox-faas/faas/pkg/db"
	"github.com/onebox-faas/faas/pkg/db/pgtest"
)

func TestMigration_00486_EventsOperatorIntentsTraceID(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.Open(t)
	defer pool.Close()
	migrateUpOnce(ctx, t, pool)

	// (1) events.trace_id column shape.
	assertColumnShape(t, ctx, pool, "events", "trace_id", "text", "YES")
	// (4) operator_intents.trace_id column shape.
	assertColumnShape(t, ctx, pool, "operator_intents", "trace_id", "text", "YES")

	// (2) events.trace_id CHECK is the OTel regex.
	body := mustQueryColumnCheckBody(t, pool, "events", "trace_id")
	if !strings.Contains(body, "'^[0-9a-f]{32}$'") {
		t.Errorf("events.trace_id CHECK body missing OTel regex; full body=%s", body)
	}
	// (4) operator_intents.trace_id CHECK is the OTel regex.
	body = mustQueryColumnCheckBody(t, pool, "operator_intents", "trace_id")
	if !strings.Contains(body, "'^[0-9a-f]{32}$'") {
		t.Errorf("operator_intents.trace_id CHECK body missing OTel regex; full body=%s", body)
	}

	// (3) + (4) partial indexes exist with the right predicate.
	assertPartialIndex(t, ctx, pool, "events_trace_idx", "events", "trace_id")
	assertPartialIndex(t, ctx, pool, "operator_intents_trace_idx", "operator_intents", "trace_id")

	// (5) Replay-safe: second MigrateUp is a no-op (the IF NOT
	// EXISTS guards in 00486's Up block make this true; pgtest
	// gives a fresh schema per test, so we explicitly re-apply
	// here to pin the property).
	if err := mustMigrateUp(ctx, pool); err != nil {
		t.Fatalf("replay MigrateUp: %v", err)
	}

	// (6) Insert with an invalid trace_id is rejected with
	// SQLSTATE 23514 (check_violation). Pre-inserts a row in
	// `events` with actor='test', kind='test.kind', subject=NULL,
	// data='{}', then attempts the same INSERT with a
	// non-conforming trace_id.
	if _, err := pool.Exec(ctx, `
		insert into events (actor, kind, data)
		values ('test', 'test.null_trace_id', '{}'::jsonb)
	`); err != nil {
		t.Fatalf("insert null-trace_id row: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		insert into events (actor, kind, trace_id, data)
		values ('test', 'test.bad_trace_id', 'not-hex', '{}'::jsonb)
	`); err == nil {
		t.Fatalf("expected CHECK violation for non-hex trace_id; got nil")
	} else {
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "23514" {
			t.Errorf("non-hex trace_id rejection: want SQLSTATE 23514, got %v (code=%s)", err, pgErrCode(pgErr))
		}
	}
	// A valid 32-char OTel hex is accepted.
	if _, err := pool.Exec(ctx, `
		insert into events (actor, kind, trace_id, data)
		values ('test', 'test.hex_trace_id', '4bf92f3577b34da6a3ce929d0e0e4736', '{}'::jsonb)
	`); err != nil {
		t.Errorf("valid OTel hex trace_id should INSERT cleanly: %v", err)
	}
}

// assertColumnShape reads information_schema.columns for table/col
// and asserts data_type + is_nullable match. Returns silently on
// match; t.Errorf on mismatch; t.Errorf on missing column.
func assertColumnShape(t *testing.T, ctx context.Context, pool *pgxpool.Pool, table, column, wantType, wantNullable string) {
	t.Helper()
	var dataType, nullable string
	if err := pool.QueryRow(ctx, `
		select data_type, is_nullable
		  from information_schema.columns
		 where table_schema = current_schema()
		   and table_name   = $1
		   and column_name  = $2
	`, table, column).Scan(&dataType, &nullable); err != nil {
		t.Errorf("%s.%s not present after migration: %v", table, column, err)
		return
	}
	if dataType != wantType {
		t.Errorf("%s.%s data_type = %q, want %q", table, column, dataType, wantType)
	}
	if nullable != wantNullable {
		t.Errorf("%s.%s is_nullable = %q, want %q", table, column, nullable, wantNullable)
	}
}

// mustQueryColumnCheckBody reads pg_get_constraintdef for the
// auto-named CHECK on (table, column). The auto-name for an
// anonymous column CHECK is `<table>_<column>_check` per Postgres
// convention. Pins the OTel regex body (the migration names the
// CHECK inline as `CHECK (trace_id IS NULL OR trace_id ~ '^…$')`).
func mustQueryColumnCheckBody(t *testing.T, pool *pgxpool.Pool, table, column string) string {
	t.Helper()
	conname := table + "_" + column + "_check"
	var body string
	if err := pool.QueryRow(context.Background(), `
		select pg_get_constraintdef(oid)
		  from pg_constraint
		 where conname = $1
		   and conrelid = $2::regclass
	`, conname, table).Scan(&body); err != nil {
		t.Fatalf("query pg_constraint for %s: %v", conname, err)
	}
	return body
}

// assertPartialIndex confirms that pg_index has an entry for
// idxname with `indpred IS NOT NULL` (i.e. it's a partial index),
// referencing table(column), and the predicate text mentions
// the column. Pinned shape matches the 00427 precedent at
// `request_telemetry_trace_idx`.
func assertPartialIndex(t *testing.T, ctx context.Context, pool *pgxpool.Pool, idxname, table, column string) {
	t.Helper()
	var pred string
	if err := pool.QueryRow(ctx, `
		select coalesce(pg_get_expr(indpred, indrelid), '')
		  from pg_index
		  join pg_class  on pg_class.oid  = pg_index.indexrelid
		  join pg_stat_user_indexes ui on ui.indexrelid = pg_index.indexrelid
		 where pg_class.relname = $1
		   and ui.schemaname    = current_schema()
	`, idxname).Scan(&pred); err != nil {
		t.Errorf("index %s not present on %s: %v", idxname, table, err)
		return
	}
	if pred == "" {
		t.Errorf("index %s on %s is not partial (no WHERE predicate); want partial predicate mentioning %s IS NOT NULL",
			idxname, table, column)
		return
	}
	if !strings.Contains(strings.ToLower(pred), strings.ToLower(column)) {
		t.Errorf("index %s on %s partial predicate does not reference %s; got %q", idxname, table, column, pred)
	}
	if !strings.Contains(strings.ToUpper(pred), "IS NOT NULL") {
		t.Errorf("index %s on %s partial predicate missing IS NOT NULL; got %q", idxname, table, pred)
	}
}

// mustMigrateUp re-applies the migration set against the test
// pool. Returns the error from MigrateUp (nil on success).
// Pattern mirrors 00074_projects_and_workloads_test.go:412.
func mustMigrateUp(ctx context.Context, pool *pgxpool.Pool) error {
	return db.MigrateUp(ctx, pool)
}
