//go:build !no_pg

// Migration-apply test for 00073 (PR #428 review blocker #3).
// Pins the snapshots_live_idx partial index shape — the partial
// predicate (`WHERE stale = false`) is the load-bearing piece; a
// future migration that drops the predicate or broadens it to
// `(deployment_id)` would silently turn the storage rollup cron
// into a full table scan again.

package migrations_test

import (
	"context"
	"strings"
	"testing"

	"github.com/onebox-faas/faas/pkg/db"
	"github.com/onebox-faas/faas/pkg/db/pgtest"
)

func TestMigrations_00073_SnapshotsLiveIdx(t *testing.T) {
	pool := pgtest.Open(t)
	defer pool.Close()

	ctx := context.Background()
	if err := db.MigrateUp(ctx, pool); err != nil {
		t.Fatalf("db.MigrateUp: %v", err)
	}

	var indexDef string
	err := pool.QueryRow(ctx,
		`select indexdef from pg_indexes
		  where schemaname = current_schema()
		    and tablename = 'snapshots'
		    and indexname = 'snapshots_live_idx'`).
		Scan(&indexDef)
	if err != nil {
		t.Fatalf("query snapshots_live_idx: %v", err)
	}
	// Postgres' indexdef string for a partial index always renders
	// the predicate as the trailing WHERE clause. Pin both the
	// column list and the predicate so a future refactor that
	// drops the partial filter is caught at unit-test time. The
	// test schema is per-test (e.g. `faas_test_19e383339e728f9f`),
	// so we assert only the suffix after `ON ` to stay
	// schema-agnostic; the leading literal schema name is
	// pgtest.Open's choice and is not part of the migration
	// contract.
	const wantSuffix = "snapshots USING btree (deployment_id) WHERE (stale = false)"
	if !strings.HasSuffix(indexDef, wantSuffix) {
		t.Errorf("snapshots_live_idx def drifted:\n got  %s\n want suffix %s", indexDef, wantSuffix)
	}
}
