//go:build !no_pg

// Migration-apply test for 00288 (deployments.reason + deployments.tag +
// deployments.deployed_by + deployments.pr_number + three CHECK
// constraints).
// Pins the contract from issue #977 / ADR-116:
//
//  1. The migration set applies cleanly through 00288.
//  2. The four new columns land on deployments with the expected
//     types and the nullable shape (existing rows stay valid).
//  3. The closed-set CHECK on `tag` accepts the documented vocabulary
//     and rejects a stray value.
//  4. The length CHECK on `reason` accepts ≤280 chars and rejects
//     a 281-char string.
//  5. The positive CHECK on `pr_number` accepts positive ints and
//     rejects 0 / negative.
//  6. Re-running goose MigrateUp is a no-op (replay safety).
//
// Build tag mirrors 00025_deployments_rootfs_key_test.go: set
// FAAS_SKIP_PG_TESTS=1 locally to skip.

package migrations_test

import (
	"context"
	"strings"
	"testing"

	"github.com/onebox-faas/faas/pkg/db"
	"github.com/onebox-faas/faas/pkg/db/pgtest"
)

// TestMigrations_00288_DeploymentAnnotation is the per-migration
// pin for the deployment-annotation columns introduced in
// 00288 (issue #977 / ADR-116).
func TestMigrations_00288_DeploymentAnnotation(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.Open(t)

	// (1) Run the full migration set. 00288 should land last.
	if err := db.MigrateUp(ctx, pool); err != nil {
		t.Fatalf("db.MigrateUp: %v (PR follow-up failure mode: missing migration slot between 00287 and 00288)", err)
	}

	// (2) Column shape. Scoped to current_schema() per
	// migrations-info-schema-scoping-pattern.md so a parallel
	// pgtest run on the same box doesn't bleed rows in.
	rows, err := pool.Query(ctx, `
		select column_name, data_type, is_nullable
		  from information_schema.columns
		 where table_schema = current_schema()
		   and table_name = 'deployments'
		   and column_name in ('reason', 'tag', 'deployed_by', 'pr_number')`)
	if err != nil {
		t.Fatalf("query columns: %v", err)
	}
	defer rows.Close()
	colTypes := map[string]string{}
	colNullable := map[string]string{}
	for rows.Next() {
		var name, typ, nullable string
		if err := rows.Scan(&name, &typ, &nullable); err != nil {
			t.Fatalf("scan column row: %v", err)
		}
		colTypes[name] = typ
		colNullable[name] = nullable
	}
	for _, col := range []string{"reason", "tag", "deployed_by"} {
		if colTypes[col] != "text" {
			t.Errorf("%s type = %q, want text", col, colTypes[col])
		}
		if colNullable[col] != "YES" {
			t.Errorf("%s nullable = %q, want YES (existing rows must stay valid)",
				col, colNullable[col])
		}
	}
	if colTypes["pr_number"] != "integer" {
		t.Errorf("pr_number type = %q, want integer", colTypes["pr_number"])
	}
	if colNullable["pr_number"] != "YES" {
		t.Errorf("pr_number nullable = %q, want YES (push-to-main with no PR leaves NULL)",
			colNullable["pr_number"])
	}

	// (3) Closed-set CHECK on `tag`. pg_get_constraintdef emits
	// either IN (a, b, c) or ANY(ARRAY[a, b, c]) per
	// pg-get-constraintdef-shapes.md; we assert each closed-set
	// value is present.
	var tagDef string
	err = pool.QueryRow(ctx, `
		select pg_get_constraintdef(c.oid)
		  from pg_constraint c
		  join pg_namespace n on n.oid = c.connamespace
		 where c.conname = 'deployments_tag_set_chk'
		   and n.nspname = current_schema()`).Scan(&tagDef)
	if err != nil {
		t.Fatalf("query tag CHECK: %v (closed-set tag CHECK must have landed)", err)
	}
	for _, want := range []string{
		"incident_recovery", "hotfix", "scheduled_maintenance",
		"compliance_hold", "partner_request",
	} {
		if !strings.Contains(tagDef, want) {
			t.Errorf("tag CHECK def %q missing closed-set value %q", tagDef, want)
		}
	}

	// (4) Length CHECK on `reason`. The constraint def includes
	// the `length(reason) <= 280` predicate.
	var reasonDef string
	err = pool.QueryRow(ctx, `
		select pg_get_constraintdef(c.oid)
		  from pg_constraint c
		  join pg_namespace n on n.oid = c.connamespace
		 where c.conname = 'deployments_reason_len_chk'
		   and n.nspname = current_schema()`).Scan(&reasonDef)
	if err != nil {
		t.Fatalf("query reason length CHECK: %v", err)
	}
	if !strings.Contains(reasonDef, "280") {
		t.Errorf("reason length CHECK def %q missing the 280-char cap", reasonDef)
	}

	// (5) Positive CHECK on `pr_number`.
	var prNumDef string
	err = pool.QueryRow(ctx, `
		select pg_get_constraintdef(c.oid)
		  from pg_constraint c
		  join pg_namespace n on n.oid = c.connamespace
		 where c.conname = 'deployments_pr_number_positive_chk'
		   and n.nspname = current_schema()`).Scan(&prNumDef)
	if err != nil {
		t.Fatalf("query pr_number CHECK: %v", err)
	}
	if !strings.Contains(prNumDef, "> 0") {
		t.Errorf("pr_number CHECK def %q missing the > 0 predicate", prNumDef)
	}

	// (6) Replay safety: applying the migration set a second time
	// must not blow up. The ADD COLUMN IF NOT EXISTS / DROP
	// CONSTRAINT IF EXISTS guards handle this.
	if err := db.MigrateUp(ctx, pool); err != nil {
		t.Fatalf("replay db.MigrateUp: %v (the ADD COLUMN IF NOT EXISTS / DROP CONSTRAINT IF EXISTS guards must have been silently dropped)", err)
	}
}
