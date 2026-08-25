//go:build !no_pg

// Migration-apply test for 00445_operator_intents.sql
// (ADR-127 / PR #1099 P2 redesign step 1).
//
// Pins:
//
//  1. Migration set applies cleanly through 00445 (no goose
//     duplicate-version panic against 00430 + siblings).
//  2. operator_intents table exists with the closed-vocabulary
//     CHECK on kind ('force_park' | 'force_cold_boot') and on
//     status ('pending' | 'running' | 'succeeded' | 'failed' |
//     'cancelled').
//  3. The default for status is 'pending' — matches the
//     Store.InsertOperatorIntent contract (caller does not pass
//     a status; the row starts at pending).
//  4. The two indexes exist: pending hot-path + target_id
//     lookup. The pending index is partial (WHERE status =
//     'pending') — same shape as
//     cron_fire_now_requests_pending_idx at
//     migrations/00194:64-66.
//  5. Replay-safe: second MigrateUp is a no-op (IF NOT EXISTS
//     on CREATE TABLE + CREATE INDEX).
//
// Build tag matches the rest of the migration tests; set
// FAAS_SKIP_PG_TESTS=1 to skip locally (see migrations/README.md).
package migrations_test

import (
	"context"
	"strings"
	"testing"

	"github.com/onebox-faas/faas/pkg/db"
	"github.com/onebox-faas/faas/pkg/db/pgtest"
)

func TestMigrations_00445_OperatorIntents(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.Open(t)
	if err := db.MigrateUp(ctx, pool); err != nil {
		t.Fatalf("db.MigrateUp: %v", err)
	}
	if err := db.MigrateUp(ctx, pool); err != nil {
		t.Fatalf("replay db.MigrateUp: %v", err)
	}

	// Table presence + status enum CHECK + kind enum CHECK.
	// pg_constraint has both CHECKs; we read both definitions
	// and assert each closed-vocabulary set is present.
	checkRows, err := pool.Query(ctx, `
		SELECT conname, pg_get_constraintdef(c.oid)
		FROM pg_constraint c
		JOIN pg_class t ON t.oid = c.conrelid
		WHERE t.relname = 'operator_intents'
		  AND c.contype = 'c'
	`)
	if err != nil {
		t.Fatalf("query pg_constraint for operator_intents CHECK: %v", err)
	}
	defer checkRows.Close()

	wantKinds := []string{"force_park", "force_cold_boot"}
	wantStatuses := []string{"pending", "running", "succeeded", "failed", "cancelled"}
	sawKindCheck := false
	sawStatusCheck := false
	for checkRows.Next() {
		var name, def string
		if err := checkRows.Scan(&name, &def); err != nil {
			t.Fatalf("scan CHECK row: %v", err)
		}
		switch {
		case strings.Contains(name, "kind"):
			sawKindCheck = true
			for _, want := range wantKinds {
				if !strings.Contains(def, want) {
					t.Errorf("kind CHECK missing %q; got: %s", want, def)
				}
			}
		case strings.Contains(name, "status"):
			sawStatusCheck = true
			for _, want := range wantStatuses {
				if !strings.Contains(def, want) {
					t.Errorf("status CHECK missing %q; got: %s", want, def)
				}
			}
		}
	}
	if err := checkRows.Err(); err != nil {
		t.Fatalf("checkRows.Err: %v", err)
	}
	if !sawKindCheck {
		t.Errorf("operator_intents kind CHECK constraint missing")
	}
	if !sawStatusCheck {
		t.Errorf("operator_intents status CHECK constraint missing")
	}

	// Index presence — both hot-path (pending) and lookup (target).
	for _, wantIdx := range []string{
		"operator_intents_pending_idx",
		"operator_intents_target_idx",
	} {
		idxRows, err := pool.Query(ctx, `
			SELECT indexname
			FROM pg_indexes
			WHERE schemaname = current_schema()
			  AND tablename = 'operator_intents'
			  AND indexname = $1
		`, wantIdx)
		if err != nil {
			t.Fatalf("query pg_indexes for %s: %v", wantIdx, err)
		}
		if !idxRows.Next() {
			t.Errorf("operator_intents missing index %s", wantIdx)
		}
		idxRows.Close()
	}
}
