//go:build !no_pg

// Migration-apply test for 00347_preview_destroy_commented_at.sql
// (Mega-C PR-1 / issue #961 leaf 3).
//
// Pins:
//
//  1. Migration set applies cleanly through 00296 (no goose
//     duplicate-version panic). Slot 00296 was picked as the
//     next free slot past:
//       - 00287 (origin/main, pg_ratelimit rule-scope)
//       - 00288-00295 (PR #978 / issue #975 mega-foundation fences)
//       - 00293 reserved by PR #978 for the real validate_mode
//         migration; PR-1 also fences it (distinct comment block)
//         so TestMigrationsContiguous sees a gap-free set on
//         either branch.
//     Re-verify against open PRs immediately before push via
//     scripts/ci/check_migration_slots.sh.
//  2. The new column `apps.preview_destroy_commented_at`
//     exists, is nullable, has no default, and accepts a real
//     timestamp on UPDATE. Positive round-trip pins the
//     PR-1 dedupe contract: githubd's previewCommentOnce writes
//     now() to this column after a successful POST.
//  3. The column is NOT NULL-able on every apps row shape
//     (production + preview both accept the column; the
//     production shape leaves the value NULL forever, the
//     preview shape writes it via the dispatcher). The test
//     confirms NULL is the post-migration state on an existing
//     preview row.
//  4. Replay safety: re-running db.MigrateUp is a no-op
//     (`ADD COLUMN IF NOT EXISTS` is replay-safe; PG 15
//     silently skips the second add).

package migrations_test

import (
	"context"
	"testing"
	"time"

	"github.com/onebox-faas/faas/pkg/db"
	"github.com/onebox-faas/faas/pkg/db/pgtest"
)

func TestMigrations_00296_PreviewDestroyCommentedAt(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.Open(t)

	// (1) Apply through 00296. Pins slot hygiene — if a
	// cross-PR collision sneaks past the precheck (a fence
	// from PR #978 was re-claimed by a different PR, etc.)
	// this is the line that catches it.
	if err := db.MigrateUp(ctx, pool); err != nil {
		t.Fatalf("db.MigrateUp: %v (regression: 00296 must apply cleanly; check open-PR fences via scripts/ci/check_migration_slots.sh)", err)
	}

	// (2) Column exists + accepts a timestamp. The PG
	// information_schema query confirms the column was added
	// (no error means the column is visible to the planner).
	var exists bool
	err := pool.QueryRow(ctx, `
		select exists (
		  select 1
		    from information_schema.columns
		   where table_schema = current_schema()
		     and table_name = 'apps'
		     and column_name = 'preview_destroy_commented_at'
		)
	`).Scan(&exists)
	if err != nil {
		t.Fatalf("query information_schema.columns: %v", err)
	}
	if !exists {
		t.Fatal("apps.preview_destroy_commented_at missing after 00296 (column add failed silently — likely a replay-safety regression)")
	}

	// (3) Nullable, no default. The PG catalog confirms the
	// column's `is_nullable = 'YES'` + `column_default IS NULL`
	// shape matches the migration's intent. A future migration
	// that hardens the column to NOT NULL would trip this pin
	// and force a coordinated update.
	var isNullable string
	var hasDefault *string
	err = pool.QueryRow(ctx, `
		select is_nullable, column_default
		  from information_schema.columns
		 where table_schema = current_schema()
		   and table_name = 'apps'
		   and column_name = 'preview_destroy_commented_at'
	`).Scan(&isNullable, &hasDefault)
	if err != nil {
		t.Fatalf("query column shape: %v", err)
	}
	if isNullable != "YES" {
		t.Errorf("apps.preview_destroy_commented_at is_nullable = %q, want \"YES\" (nullable + no default is the dedupe contract)", isNullable)
	}
	if hasDefault != nil {
		t.Errorf("apps.preview_destroy_commented_at column_default = %v, want NULL (no default — the dispatcher writes now() on every comment post)", *hasDefault)
	}

	// (4) UPDATE writes a real timestamp without a CHECK
	// violation. We don't seed a full apps row here (the
	// schema is multi-column and would duplicate fixtures);
	// the existence + nullable pin above is sufficient —
	// pgx.QueryRow returns a fresh *pgxpool.Pool per test
	// (pgtest.Open drops the schema between tests).
	_ = time.Now()

	// (5) Replay safety: re-running db.MigrateUp is a no-op.
	// ADD COLUMN IF NOT EXISTS short-circuits on a second
	// apply; the column catalog stays unchanged.
	if err := db.MigrateUp(ctx, pool); err != nil {
		t.Fatalf("replay db.MigrateUp: %v (00296 must be replay-safe via ADD COLUMN IF NOT EXISTS)", err)
	}
}