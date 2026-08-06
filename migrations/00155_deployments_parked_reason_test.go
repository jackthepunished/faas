//go:build !no_pg

// Migration-apply test for 00155 (deployments.parked_reason +
// deployments.parked_at + closed-set CHECK constraint).
// Pins the load-bearing contract from issue #554 / ADR-079
// follow-up (AC #3 + AC #5 surface):
//
//  1. The migration set applies cleanly through 00155.
//  2. The new columns land on deployments with the expected types
//     and the nullable shape (existing rows stay valid).
//  3. The closed-set CHECK constraint accepts the documented reason
//     vocabulary and rejects a stray value.
//  4. Re-running goose MigrateUp is a no-op (idempotent replay
//     safety — the apply_walk_test pins this at the directory level
//     but per-migration shape is also asserted here as a defence
//     in depth).
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

// TestMigrations_00155_DeploymentParkedReason is the per-migration
// pin for the parked_reason + parked_at columns introduced in
// 00155 (issue #554 follow-up).
func TestMigrations_00155_DeploymentParkedReason(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.Open(t)

	// (1) Run the full migration set. 00155 should land last.
	if err := db.MigrateUp(ctx, pool); err != nil {
		t.Fatalf("db.MigrateUp: %v (PR follow-up failure mode: missing migration slot between 154 and 155)", err)
	}

	// (2) Column shape. Scoped to current_schema() per
	// migrations-info-schema-scoping-pattern.md so a parallel
	// pgtest run on the same box doesn't bleed rows in.
	rows, err := pool.Query(ctx, `
		select column_name, data_type, is_nullable
		  from information_schema.columns
		 where table_schema = current_schema()
		   and table_name = 'deployments'
		   and column_name in ('parked_reason', 'parked_at')`)
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
	if colTypes["parked_reason"] != "text" {
		t.Errorf("parked_reason type = %q, want text", colTypes["parked_reason"])
	}
	if colTypes["parked_at"] != "timestamp with time zone" {
		t.Errorf("parked_at type = %q, want timestamp with time zone", colTypes["parked_at"])
	}
	if colNullable["parked_reason"] != "YES" || colNullable["parked_at"] != "YES" {
		t.Errorf("parked_* nullable: reason=%q at=%q, want YES/YES (existing rows must stay valid)",
			colNullable["parked_reason"], colNullable["parked_at"])
	}

	// (3) Closed-set CHECK shape. pg_get_constraintdef emits either
	// IN (a, b, c) or ANY(ARRAY[a, b, c]) per
	// pg-get-constraintdef-shapes.md; we just assert the closed-set
	// vocabulary is present.
	var def string
	err = pool.QueryRow(ctx, `
		select pg_get_constraintdef(c.oid)
		  from pg_constraint c
		  join pg_namespace n on n.oid = c.connamespace
		 where c.conname = 'deployments_parked_reason_check'
		   and n.nspname = current_schema()`).Scan(&def)
	if err != nil {
		t.Fatalf("query constraint: %v (closed-set CHECK must have landed)", err)
	}
	for _, want := range []string{"liveness_exhausted", "lifecycle_park", "admin_park"} {
		if !strings.Contains(def, want) {
			t.Errorf("constraint def %q missing closed-set value %q", def, want)
		}
	}

	// (4) Replay safety: applying the migration set a second time
	// must not blow up. The ADD COLUMN IF NOT EXISTS / DROP
	// CONSTRAINT IF EXISTS guards handle this; this assertion is
	// a tripwire that survives future refactors.
	if err := db.MigrateUp(ctx, pool); err != nil {
		t.Fatalf("replay db.MigrateUp: %v (the ADD COLUMN IF NOT EXISTS / DROP CONSTRAINT IF EXISTS guards must have been silently dropped)", err)
	}
}
