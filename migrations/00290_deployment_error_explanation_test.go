//go:build !no_pg

// Migration-apply test for 00290_deployment_error_explanation.sql
// (error-explanations cluster, spec §6.4 amendment 1, ADR-110
// amendment 1).
//
// Pins:
//
//  1. Migration set applies cleanly through 00290 (no goose
//     duplicate-version panic). Slot 00290 was picked as the next
//     free slot on origin/main past the open-PR reservations:
//       - PR #910 (00281-00289 trigger cluster — 00288 + 00289 are
//         reserve_slot fences the trigger cluster reserved for
//         itself; 00290 is the next free slot)
//     Re-verify against open PRs immediately before push via
//     scripts/ci/check_migration_slots.sh.
//  2. The four new columns (error_hint / error_why / error_fix /
//     error_relevant_logs) exist on the deployments table after the
//     walk. Catches a typo where the ALTER TABLE listed only some
//     of the four columns the catalog code paths write to.
//  3. error_relevant_logs is jsonb (not text or json) — the
//     pgstore write path uses pgx's jsonb codec on
//     []api.LogExcerpt and a wrong type here fails every
//     SetDeploymentFailed call with a type mismatch.
//  4. Replay safety: re-running db.MigrateUp is a no-op (the
//     migration is replay-safe via IF NOT EXISTS on every column —
//     see the migration body for the exact pattern).

package migrations_test

import (
	"context"
	"testing"

	"github.com/onebox-faas/faas/pkg/db"
	"github.com/onebox-faas/faas/pkg/db/pgtest"
)

func TestMigrations_00290_DeploymentErrorExplanation(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.Open(t)

	// (1) Apply through 00290.
	if err := db.MigrateUp(ctx, pool); err != nil {
		t.Fatalf("db.MigrateUp: %v (regression: missing migration slot between 00287 pg_ratelimit widen and 00290 deployment_error_explanation)", err)
	}

	// (2) + (3) The four columns exist on deployments, with the
	// right types. information_schema.columns is the same source
	// pg_dump uses for schema introspection — canonical probe.
	type colSpec struct {
		name     string
		dataType string
	}
	want := []colSpec{
		{"error_hint", "text"},
		{"error_why", "text"},
		{"error_fix", "text"},
		{"error_relevant_logs", "jsonb"},
	}
	for _, c := range want {
		var gotType string
		err := pool.QueryRow(ctx, `
			select data_type from information_schema.columns
			 where table_schema = current_schema()
			   and table_name = 'deployments'
			   and column_name = $1
		`, c.name).Scan(&gotType)
		if err != nil {
			t.Fatalf("query deployments.%s: %v (the four error-explanation columns must exist after 00290; catalog code paths in pkg/whycopy.Decorate + pgstore SetDeploymentFailed write all four)", c.name, err)
		}
		if gotType != c.dataType {
			t.Errorf("deployments.%s: got type=%s want=%s (the catalog code paths wire this column with the corresponding Go type — wrong type here fails every SetDeploymentFailed call)", c.name, gotType, c.dataType)
		}
	}

	// (4) Replay safety: re-running db.MigrateUp is a no-op via
	// IF NOT EXISTS on every column.
	if err := db.MigrateUp(ctx, pool); err != nil {
		t.Fatalf("replay db.MigrateUp: %v (migration must be replay-safe — IF NOT EXISTS on every column is the load-bearing carve-out)", err)
	}
}
