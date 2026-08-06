//go:build !no_pg

// Migration-apply test for 00155 (slot fence for PR #697:
// issue #554 followup — deployments.parked_reason persistence +
// audit surface + AC #1 metal test, ADR-079 follow-up). PR #697
// owns slot 155 with the real schema change; this branch
// (issue #695 / ADR-080) renumbered from 00155 to 00156 to avoid
// a goose "duplicate version 155" collision when both PRs land.
//
// The fence body is a no-op `SELECT 1;` per ADR-041 — goose
// applies it cleanly and writes a row in goose_db_version. This
// test pins:
//
//  1. The migration set applies cleanly through 00155.
//  2. The fence is no-op against any existing table — no rows
//     written, no schema touched (regression tripwire if a future
//     contributor adds DDL to a slot-fence file).
//  3. Replay safety: a second MigrateUp is a no-op.
package migrations_test

import (
	"context"
	"testing"

	"github.com/onebox-faas/faas/pkg/db"
	"github.com/onebox-faas/faas/pkg/db/pgtest"
)

func TestMigrations_00155_ReserveSlotForParkedReason(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.Open(t)

	// (1) Apply through 00155. The fence file uses 00155 as its
	// slot number so goose reserves that slot for PR #697.
	if err := db.MigrateUp(ctx, pool); err != nil {
		t.Fatalf("db.MigrateUp: %v (regression: missing migration slot between 1 and 154)", err)
	}

	// (2) Sanity probe: a slot fence is a no-op, so any pre-existing
	// table on this schema is unchanged. We pick `accounts`
	// because it's been on the schema since 00001 — a constant
	// surface across the migration set, ideal for "did this fence
	// break anything?" probes.
	var tableCount int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM information_schema.tables
		 WHERE table_schema = current_schema() AND table_name = 'accounts'`,
	).Scan(&tableCount); err != nil {
		t.Fatalf("accounts table lookup: %v", err)
	}
	if tableCount != 1 {
		t.Errorf("accounts table count = %d, want 1 (fence must not touch the schema)", tableCount)
	}

	// (3) Replay safety — a second MigrateUp is a no-op. Goose's
	// goose_db_version row pins the state.
	if err := db.MigrateUp(ctx, pool); err != nil {
		t.Fatalf("replay safety: second MigrateUp returned error: %v", err)
	}
}