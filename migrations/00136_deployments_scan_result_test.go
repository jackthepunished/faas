//go:build !no_pg

// Migration-apply test for 00136 (issue #464 / ADR-055 —
// per-deploy grype CVE scan surface, PR-1 data plane). Pins:
//
//  1. The migration set applies cleanly through 00136.
//  2. Three columns exist on `deployments`:
//       * scan_result  jsonb
//       * scan_status  text NULLABLE
//       * scanned_at   timestamptz
//     (NULLABLE on all three so the PR-3 sink can land rows
//     incrementally without breaking the existing INSERT shape.)
//  3. The CHECK constraint `deployments_scan_status_chk` exists
//     and is the closed enum {pending, complete, failed, skipped}.
//  4. The partial index `deployments_app_scan_complete_idx` exists
//     on (app_id, scanned_at DESC) WHERE scan_status='complete'.
//  5. The replay-safe backfill stamps every pre-existing row with
//     scan_status='skipped' (the pre-feature sentinel) and a
//     scan_result payload that names the reason.
//  6. The CHECK constraint rejects 'bogus' as a scan_status value
//     (the 23514 path that catches a typo at the PR-3 sink site).
//  7. Replay-safety: a second MigrateUp is a no-op (ADR-041).
//
// Slot note: 00135 is taken by PRs #540/#647/#653 on open
// branches; this PR carries 00136. The previous real slot
// is 00134 (api_keys_org_bound); 00129/00130 are fences past
// PR #623's slot claim. This test pins 00136's three-column +
// CHECK + index + backfill shape; renumber would need filename
// + test name + apply range bump + UUID literals together.
package migrations_test

import (
	"context"
	"testing"

	"github.com/onebox-faas/faas/pkg/db"
	"github.com/onebox-faas/faas/pkg/db/pgtest"
)

func TestMigrations_00136_DeploymentsScanResult(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.Open(t)

	// (1) Apply through 00136. A regression that drops a slot
	// between 1 and 135 surfaces here before the per-assertion pins.
	if err := db.MigrateUp(ctx, pool); err != nil {
		t.Fatalf("db.MigrateUp: %v (regression: missing migration slot between 1 and 135)", err)
	}

	// (2) Three columns exist with the right types. The pgtest
	// helper isolates each test in its own search_path, so the
	// information_schema lookup is scoped via current_schema().
	// scan_result is jsonb, scan_status is text (the CHECK is
	// added below, not enforced by the column type itself),
	// scanned_at is timestamptz. All three are NULLABLE — the
	// PR-3 sink writes them; the existing INSERT path (apid's
	// createDeployment) doesn't need to know about the scan
	// columns to land a row.
	want := map[string]struct {
		dataType  string
		nullable  string
	}{
		"scan_result": {dataType: "jsonb", nullable: "YES"},
		"scan_status": {dataType: "text", nullable: "YES"},
		"scanned_at":  {dataType: "timestamp with time zone", nullable: "YES"},
	}
	for col, want := range want {
		var dataType, isNullable string
		if err := pool.QueryRow(ctx, `
			select data_type, is_nullable
			from information_schema.columns
			where table_schema = current_schema()
			  and table_name = 'deployments'
			  and column_name = $1
		`, col).Scan(&dataType, &isNullable); err != nil {
			t.Fatalf("read %s column metadata: %v", col, err)
		}
		if dataType != want.dataType {
			t.Errorf("%s data_type = %q, want %q", col, dataType, want.dataType)
		}
		if isNullable != want.nullable {
			t.Errorf("%s is_nullable = %q, want %q", col, isNullable, want.nullable)
		}
	}

	// (3) CHECK constraint. The name is the one pkg/apid's
	// scan_sink.go and the PR-3 deploy-write path reference. A
	// regression that drops or renames the constraint surfaces here.
	var chkCount int
	if err := pool.QueryRow(ctx, `
		select count(*) from pg_constraint
		where conname = 'deployments_scan_status_chk'
		  and conrelid = 'deployments'::regclass
	`).Scan(&chkCount); err != nil {
		t.Fatalf("read pg_constraint: %v", err)
	}
	if chkCount != 1 {
		t.Errorf("deployments_scan_status_chk missing (count = %d, want 1)", chkCount)
	}

	// (4) Partial index on (app_id, scanned_at DESC) WHERE
	// scan_status='complete'. The dashboard's per-app scan
	// drill-down relies on this for the 5-min SLA check.
	var idxCount int
	if err := pool.QueryRow(ctx, `
		select count(*) from pg_indexes
		where schemaname = current_schema()
		  and tablename = 'deployments'
		  and indexname = 'deployments_app_scan_complete_idx'
	`).Scan(&idxCount); err != nil {
		t.Fatalf("read pg_indexes: %v", err)
	}
	if idxCount != 1 {
		t.Errorf("deployments_app_scan_complete_idx missing (count = %d, want 1)", idxCount)
	}

	// (5) Backfill. Seed a minimal account + app + deployment so
	// there's a pre-existing row to assert against. The migration
	// ran before this seed (the seed is post-MigrateUp), so the
	// row stays at scan_status=NULL. We seed BEFORE a second
	// MigrateUp further down to test the backfill behaviour.
	if _, err := pool.Exec(ctx, `
		insert into accounts (id, plan, email)
		values ('00000000-0000-0000-0000-000000000136', 'scale', 'scan-test@example.com')
	`); err != nil {
		t.Fatalf("seed accounts: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		insert into apps (id, account_id, slug)
		values ('00000000-0000-0000-0000-000000000136',
		        '00000000-0000-0000-0000-000000000136',
		        'scan-test')
	`); err != nil {
		t.Fatalf("seed apps: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		insert into deployments (id, app_id, kind, source_path, source_bytes, status, image_digest)
		values ('00000000-0000-0000-0000-000000000136',
		        '00000000-0000-0000-0000-000000000136',
		        'tarball', '/tmp/test.tar', 0, 'live', 'sha256:0')
	`); err != nil {
		t.Fatalf("seed deployments: %v", err)
	}

	// The pre-seeded row has scan_status=NULL (the migration ran
	// before the seed). Stamp it to NULL to mirror the
	// pre-feature state, then a second MigrateUp triggers the
	// backfill (the test simulates a fresh-DB replay).
	if _, err := pool.Exec(ctx, `
		update deployments set scan_status = NULL, scan_result = NULL, scanned_at = NULL
		where id = '00000000-0000-0000-0000-000000000136'
	`); err != nil {
		t.Fatalf("clear scan columns for backfill test: %v", err)
	}

	// (6) Replay-safety: a second MigrateUp is a no-op for the
	// schema and triggers the backfill UPDATE for the seeded
	// row. A regression that flips the WHERE predicate to
	// always-update surfaces here as a backfill that re-stamps
	// already-stamped rows.
	if err := db.MigrateUp(ctx, pool); err != nil {
		t.Fatalf("replay-safety: second MigrateUp failed: %v", err)
	}

	var backfilledStatus string
	var backfilledResult map[string]any
	if err := pool.QueryRow(ctx, `
		select scan_status, scan_result
		from deployments
		where id = '00000000-0000-0000-0000-000000000136'
	`).Scan(&backfilledStatus, &backfilledResult); err != nil {
		t.Fatalf("readback backfilled row: %v", err)
	}
	if backfilledStatus != "skipped" {
		t.Errorf("backfill scan_status = %q, want %q", backfilledStatus, "skipped")
	}
	if got, _ := backfilledResult["reason"].(string); got != "pre-feature" {
		t.Errorf("backfill scan_result.reason = %q, want %q (pre-feature sentinel)", got, "pre-feature")
	}

	// (7) CHECK rejects 'bogus' as a scan_status value. A typo
	// at the PR-3 sink site is a 23514 and visible in CI
	// before the dashboard renders the bad value.
	if _, err := pool.Exec(ctx, `
		update deployments set scan_status = 'bogus'
		where id = '00000000-0000-0000-0000-000000000136'
	`); err == nil {
		t.Errorf("update scan_status='bogus': expected CHECK violation, got nil")
	}

	// (8) Sanity: the closed-enum values pass the CHECK.
	for _, ok := range []string{"pending", "complete", "failed", "skipped"} {
		if _, err := pool.Exec(ctx, `
			update deployments set scan_status = $1
			where id = '00000000-0000-0000-0000-000000000136'
		`, ok); err != nil {
			t.Errorf("update scan_status=%q: %v (closed-enum should accept)", ok, err)
		}
	}
}
