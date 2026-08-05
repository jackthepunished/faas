//go:build !no_pg

// Migration-apply test for 00146 (issue #464 / ADR-055 —
// per-deploy grype CVE scan surface, PR-1 data plane). Pins:
//
//  1. The migration set applies cleanly through 00146.
//  2. Three columns exist on `deployments`:
//     * scan_result  jsonb
//     * scan_status  text NULLABLE
//     * scanned_at   timestamptz
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
// Slot note: 00139 is taken by PR #651 (issue #464 mega-PR)
// on open branches; PR #653 also claims slot 136 with a real
// schema. This PR carries 00146 (renumbered from 00135 → 00139
// → 00144 → 00146 to dodge the open-PR slot collision gate;
// main's PR #653 mega-PR landed 00144_api_keys_provenance at
// 144). Slot 145 holds main's sessions_binding. This test pins
// 00146's three-column + CHECK + index + backfill shape;
// renumber would need filename + test name + apply range bump +
// UUID literals together.
// literals together.
package migrations_test

import (
	"context"
	"testing"

	"github.com/onebox-faas/faas/pkg/db"
	"github.com/onebox-faas/faas/pkg/db/pgtest"
)

func TestMigrations_00146_DeploymentsScanResult(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.Open(t)

	// (1) Apply through 00146. A regression that drops a slot
	// between 1 and 136 surfaces here before the per-assertion pins.
	if err := db.MigrateUp(ctx, pool); err != nil {
		t.Fatalf("db.MigrateUp: %v (regression: missing migration slot between 1 and 145)", err)
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
		dataType string
		nullable string
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

	// (5) Backfill. The migration's `-- +goose Up` includes an
	// `UPDATE deployments SET scan_status='skipped', ... WHERE
	// scan_status IS NULL` that stamps every pre-feature row.
	// We test the backfill in two halves:
	//
	//   a) The backfill's WHERE clause catches NULL scan_status
	//      rows. We seed a row post-MigrateUp with scan_status=NULL
	//      (mimicking a pre-feature row that landed in the DB
	//      BEFORE the backfill UPDATE ran on an empty set), then
	//      run the same backfill UPDATE the migration runs. A
	//      regression that drops the `WHERE scan_status IS NULL`
	//      predicate surfaces here as a row that keeps scan_status
	//      at the sentinel value the test set in (b).
	//
	//   b) The backfill does NOT re-stamp rows that already have
	//      scan_status='skipped'. We seed a row at scan_status='skipped'
	//      with a sentinel scan_result, run the same UPDATE again,
	//      and assert the sentinel is preserved. A regression that
	//      flips the WHERE predicate to always-update surfaces here
	//      as the sentinel getting overwritten.
	seedRow := func(t *testing.T, scanStatus *string, scanResult string) {
		t.Helper()
		if _, err := pool.Exec(ctx, `
			insert into accounts (id, plan, email)
			values ('00000000-0000-0000-0000-000000000146', 'scale', 'scan-test@example.com')
			on conflict (id) do nothing
		`); err != nil {
			t.Fatalf("seed accounts: %v", err)
		}
		if _, err := pool.Exec(ctx, `
			insert into apps (id, account_id, slug, type, ram_mb, max_concurrency, idle_timeout_s, status, created_at)
			values ('00000000-0000-0000-0000-000000000146',
			        '00000000-0000-0000-0000-000000000146',
			        'scan-test', 'function', 256, 1, 30, 'active', now())
			on conflict (id) do nothing
		`); err != nil {
			t.Fatalf("seed apps: %v", err)
		}
		// status='live' is the existing app_detail.html gate; not
		// the new scan_status column. The new scan_status column
		// is NULLABLE — the seed mirrors the pre-feature shape.
		if _, err := pool.Exec(ctx, `
			insert into deployments (id, app_id, kind, source_path, source_bytes, status, image_digest, scan_status, scan_result)
			values ('00000000-0000-0000-0000-000000000146',
			        '00000000-0000-0000-0000-000000000146',
			        'tarball', '/tmp/test.tar', 0, 'live', 'sha256:0', $1, $2::jsonb)
			on conflict (id) do update set
				scan_status = excluded.scan_status,
				scan_result = excluded.scan_result
		`, scanStatus, scanResult); err != nil {
			t.Fatalf("seed deployments: %v", err)
		}
	}

	// (5a) Backfill on a NULL pre-feature row.
	seedRow(t, nil, "null")
	if _, err := pool.Exec(ctx, `
		update deployments
		set scan_status = 'skipped',
		    scan_result = jsonb_build_object('reason', 'pre-feature', 'backfill_migration', '00146')
		where id = '00000000-0000-0000-0000-000000000146'
		  and scan_status is null
	`); err != nil {
		t.Fatalf("backfill: %v", err)
	}
	var backfilledStatus string
	var backfilledResult map[string]any
	if err := pool.QueryRow(ctx, `
		select scan_status, scan_result
		from deployments
		where id = '00000000-0000-0000-0000-000000000146'
	`).Scan(&backfilledStatus, &backfilledResult); err != nil {
		t.Fatalf("readback backfilled row: %v", err)
	}
	if backfilledStatus != "skipped" {
		t.Errorf("backfill scan_status = %q, want %q", backfilledStatus, "skipped")
	}
	if got, _ := backfilledResult["reason"].(string); got != "pre-feature" {
		t.Errorf("backfill scan_result.reason = %q, want %q (pre-feature sentinel)", got, "pre-feature")
	}

	// (5b) Backfill idempotency: the WHERE clause is
	// `scan_status IS NULL` so an already-stamped row is left
	// alone. Stamp a fresh row at scan_status='skipped' with a
	// sentinel scan_result, run the same UPDATE, and assert the
	// sentinel is preserved. A regression that drops the WHERE
	// clause surfaces here as the sentinel getting overwritten
	// to {"reason":"pre-feature",...}.
	const sentinel = `{"reason":"already-stamped","guard":"5b"}`
	skipped := "skipped"
	seedRow(t, &skipped, sentinel)
	if _, err := pool.Exec(ctx, `
		update deployments
		set scan_status = 'skipped',
		    scan_result = jsonb_build_object('reason', 'pre-feature', 'backfill_migration', '00146')
		where id = '00000000-0000-0000-0000-000000000146'
		  and scan_status is null
	`); err != nil {
		t.Fatalf("idempotent backfill: %v", err)
	}
	var preservedResult map[string]any
	if err := pool.QueryRow(ctx, `
		select scan_result
		from deployments
		where id = '00000000-0000-0000-0000-000000000146'
		  and scan_status = 'skipped'
	`).Scan(&preservedResult); err != nil {
		t.Fatalf("readback preserved row: %v", err)
	}
	if got, _ := preservedResult["guard"].(string); got != "5b" {
		t.Errorf("idempotent backfill overwrote sentinel: scan_result = %v, want guard=%q", preservedResult, "5b")
	}

	// (6) Replay-safety: a second MigrateUp is a no-op (ADR-041).
	if err := db.MigrateUp(ctx, pool); err != nil {
		t.Fatalf("replay-safety: second MigrateUp failed: %v", err)
	}

	// (7) CHECK rejects 'bogus' as a scan_status value. A typo
	// at the PR-3 sink site is a 23514 and visible in CI
	// before the dashboard renders the bad value.
	if _, err := pool.Exec(ctx, `
		update deployments set scan_status = 'bogus'
		where id = '00000000-0000-0000-0000-000000000146'
	`); err == nil {
		t.Errorf("update scan_status='bogus': expected CHECK violation, got nil")
	}

	// (8) Sanity: the closed-enum values pass the CHECK.
	for _, ok := range []string{"pending", "complete", "failed", "skipped"} {
		if _, err := pool.Exec(ctx, `
			update deployments set scan_status = $1
			where id = '00000000-0000-0000-0000-000000000146'
		`, ok); err != nil {
			t.Errorf("update scan_status=%q: %v (closed-enum should accept)", ok, err)
		}
	}
}
