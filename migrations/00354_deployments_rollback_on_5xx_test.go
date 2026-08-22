//go:build !no_pg

// Migration-apply test for 00354_deployments_rollback_on_5xx.sql
// (Mega-C PR-2 / issue #961 leaf 8).
//
// Pins:
//
//  1. Migration set applies cleanly through 00354 (no goose
//     duplicate-version panic). Slot 00354 was picked as the
//     next free slot above:
//       - 00347 (PR-1's preview_destroy_commented_at — same
//         mega-PR)
//       - 00340-00346 (origin/main / PR #984 / PR #1005)
//     Re-verify against open PRs immediately before push via
//     scripts/ci/check_migration_slots.sh.
//  2. The six new deployments columns exist with the expected
//     types + nullability + defaults:
//       rollback_on_5xx            BOOLEAN     NOT NULL DEFAULT false
//       first_wake_at              TIMESTAMPTZ NULL
//       first_5xx_window_ends_at   TIMESTAMPTZ NULL
//       first_5xx_count            INT         NOT NULL DEFAULT 0
//       last_auto_rollback_at      TIMESTAMPTZ NULL
//       last_auto_rollback_reason  TEXT        NULL
//  3. The closed-set vocabulary CHECK on last_auto_rollback_reason
//     pins the schedd-side emit vocabulary (threshold_exceeded,
//     first_window_expired) — regression guard against a typo in
//     the schedd emit path widening the schema silently.
//  4. The partial index deployments_rollback_on_5xx_pending_idx
//     exists and pins the WHERE clause the schedd scan uses.
//  5. Replay safety: re-running db.MigrateUp is a no-op (all
//     ALTER statements use IF NOT EXISTS / DROP IF EXISTS).

package migrations_test

import (
	"context"
	"strings"
	"testing"

	"github.com/onebox-faas/faas/pkg/db"
	"github.com/onebox-faas/faas/pkg/db/pgtest"
)

func TestMigrations_00354_DeploymentsRollbackOn5xx(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.Open(t)

	// (1) Apply through 00354.
	if err := db.MigrateUp(ctx, pool); err != nil {
		t.Fatalf("db.MigrateUp: %v (regression: 00354 must apply cleanly; check open-PR fences via scripts/ci/check_migration_slots.sh)", err)
	}

	// (2) Six columns exist with the expected shape.
	cols := []struct {
		col      string
		wantNull bool
		wantType string
		wantDef  string // substring match against column_default
	}{
		{"rollback_on_5xx", false, "boolean", "false"},
		{"first_wake_at", true, "timestamp with time zone", ""},
		{"first_5xx_window_ends_at", true, "timestamp with time zone", ""},
		{"first_5xx_count", false, "integer", "0"},
		{"last_auto_rollback_at", true, "timestamp with time zone", ""},
		{"last_auto_rollback_reason", true, "text", ""},
	}
	for _, c := range cols {
		var typ, nullable string
		var def *string
		err := pool.QueryRow(ctx, `
			select data_type, is_nullable, column_default
			  from information_schema.columns
			 where table_schema = current_schema()
			   and table_name = 'deployments'
			   and column_name = $1
		`, c.col).Scan(&typ, &nullable, &def)
		if err != nil {
			t.Fatalf("query column %s: %v", c.col, err)
		}
		if !strings.EqualFold(typ, c.wantType) {
			t.Errorf("column %s type = %q, want %q", c.col, typ, c.wantType)
		}
		if (nullable == "YES") != c.wantNull {
			t.Errorf("column %s nullable = %v, want %v", c.col, nullable == "YES", c.wantNull)
		}
		if c.wantDef != "" {
			if def == nil || !strings.Contains(strings.ToLower(*def), c.wantDef) {
				t.Errorf("column %s default = %v, want containing %q", c.col, def, c.wantDef)
			}
		}
	}

	// (3) Closed-set vocabulary CHECK on last_auto_rollback_reason.
	var chk string
	err := pool.QueryRow(ctx, `
		select pg_get_constraintdef(c.oid)
		  from pg_constraint c
		  join pg_class t on t.oid = c.conrelid
		 where t.relname = 'deployments'
		   and c.conname = 'deployments_last_auto_rollback_reason_check'
	`).Scan(&chk)
	if err != nil {
		t.Fatalf("CHECK constraint not found: %v", err)
	}
	if !strings.Contains(chk, "threshold_exceeded") {
		t.Errorf("CHECK missing 'threshold_exceeded' token: %s", chk)
	}
	if !strings.Contains(chk, "first_window_expired") {
		t.Errorf("CHECK missing 'first_window_expired' token: %s", chk)
	}

	// (4) Partial index on the schedd-side scan.
	var idxdef string
	err = pool.QueryRow(ctx, `
		select indexdef from pg_indexes
		 where schemaname = current_schema()
		   and indexname = 'deployments_rollback_on_5xx_pending_idx'
	`).Scan(&idxdef)
	if err != nil {
		t.Fatalf("partial index missing: %v", err)
	}
	if !strings.Contains(idxdef, "WHERE") {
		t.Errorf("partial index missing WHERE clause: %s", idxdef)
	}
	if !strings.Contains(idxdef, "rollback_on_5xx") {
		t.Errorf("partial index WHERE missing rollback_on_5xx token: %s", idxdef)
	}

	// (5) Replay safety.
	if err := db.MigrateUp(ctx, pool); err != nil {
		t.Fatalf("replay db.MigrateUp: %v (00354 must be replay-safe via IF NOT EXISTS / IF EXISTS)", err)
	}
}
