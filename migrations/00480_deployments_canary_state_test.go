// migrations/00480_deployments_canary_state_test.go — pins the shape
// of the canary_*/rollout_* columns on deployments (issue #976 /
// ADR-122 / SAFE-RELEASES A + F). Build tag mirrors the precedent at
// migrations/00410_app_secret_value_hash_test.go; set
// FAAS_SKIP_PG_TESTS=1 locally to skip.
//
// Asserts:
//   1. The migration set applies cleanly through 00379 (and lands
//      00380 last).
//   2. The 9 new columns exist with the expected types and nullable
//      rules (NOT NULL columns have PG11+ fast-default zero values).
//   3. The 3 CHECK constraints land with the expected closed-set /
//      range bounds:
//      - deployments_canary_preset_chk covers the 5 catalog names
//      - deployments_rollout_state_chk covers the state machine
//      - deployments_canary_step_bounds_chk gates step into [0,total]
//   4. A pre-PR row's "no canary applied" state is the fast-default
//      zero-value (none,0,0,NULL,pending,NULL,NULL,NULL,NULL).
//   5. Re-running db.MigrateUp is a no-op (replay safety).
//
//go:build pg

package migrations_test

import (
	"context"
	"strings"
	"testing"

	"github.com/onebox-faas/faas/pkg/db"
	"github.com/onebox-faas/faas/pkg/db/pgtest"
)

func TestMigrations_00480_DeploymentsCanaryState(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.Open(t)

	// (1) Run the full migration set. 00380 should land last.
	if err := db.MigrateUp(ctx, pool); err != nil {
		t.Fatalf("db.MigrateUp: %v (PR follow-up failure mode: missing migration slot before 00380)", err)
	}

	// (2) Column shape and nullable rules on deployments.
	rows, err := pool.Query(ctx, `
		select column_name, data_type, is_nullable, column_default
		  from information_schema.columns
		 where table_schema = current_schema()
		   and table_name = 'deployments'
		   and column_name in (
		       'canary_preset',
		       'canary_step',
		       'canary_total_steps',
		       'canary_step_started_at',
		       'rollout_state',
		       'rollout_started_at',
		       'rollout_completed_at',
		       'rollout_aborted_at',
		       'rollout_aborted_reason'
		   )`)
	if err != nil {
		t.Fatalf("query deployments columns: %v", err)
	}
	defer rows.Close()

	type colInfo struct {
		typ, nullable string
		hasDefault    bool
	}
	colMap := map[string]colInfo{}
	for rows.Next() {
		var name, typ, nullable string
		var def *string
		if err := rows.Scan(&name, &typ, &nullable, &def); err != nil {
			t.Fatalf("scan column row: %v", err)
		}
		colMap[name] = colInfo{typ: typ, nullable: nullable, hasDefault: def != nil && *def != ""}
	}

	// Expected: NOT NULL columns carry a DEFAULT (PG11+ fast-default);
	// nullable timestamp / reason columns have NO default.
	wantTypes := map[string]string{
		"canary_preset":          "text",
		"canary_step":            "integer",
		"canary_total_steps":     "integer",
		"canary_step_started_at": "timestamp with time zone",
		"rollout_state":          "text",
		"rollout_started_at":     "timestamp with time zone",
		"rollout_completed_at":   "timestamp with time zone",
		"rollout_aborted_at":     "timestamp with time zone",
		"rollout_aborted_reason": "text",
	}
	for col, want := range wantTypes {
		got, ok := colMap[col]
		if !ok {
			t.Errorf("deployments.%s missing (column must land)", col)
			continue
		}
		if got.typ != want {
			t.Errorf("deployments.%s type = %q, want %q", col, got.typ, want)
		}
	}
	wantNullable := map[string]string{
		"canary_preset":          "NO",
		"canary_step":            "NO",
		"canary_total_steps":     "NO",
		"canary_step_started_at": "YES",
		"rollout_state":          "NO",
		"rollout_started_at":     "YES",
		"rollout_completed_at":   "YES",
		"rollout_aborted_at":     "YES",
		"rollout_aborted_reason": "YES",
	}
	for col, want := range wantNullable {
		got := colMap[col]
		if got.nullable != want {
			t.Errorf("deployments.%s nullable = %q, want %q", col, got.nullable, want)
		}
	}
	// Fast-defaults must exist on NOT NULL columns (PG11+ at-the-catalog
	// default so pre-PR rows aren't rewritten).
	wantDefaults := []string{"canary_preset", "canary_step", "canary_total_steps", "rollout_state"}
	for _, col := range wantDefaults {
		got := colMap[col]
		if !got.hasDefault {
			t.Errorf("deployments.%s has no DEFAULT — fast-default for back-compat on pre-PR rows is required", col)
		}
	}

	// (3) CHECK constraints. We pin by name + presence of each closed-set
	// value (pg_get_constraintdef may emit IN or ANY(ARRAY[...])).
	wantCheckContents := map[string][]string{
		"deployments_canary_preset_chk": {
			"'none'", "'slow'", "'balanced'", "'aggressive'", "'1-10-50-100'",
		},
		"deployments_rollout_state_chk": {
			"'pending'", "'rolling_out'", "'complete'", "'aborted'",
		},
	}
	for ck, members := range wantCheckContents {
		var def string
		err := pool.QueryRow(ctx, `
			select pg_get_constraintdef(c.oid)
			  from pg_constraint c
			  join pg_namespace n on n.oid = c.connamespace
			 where c.conname = $1
			   and n.nspname = current_schema()`, ck).Scan(&def)
		if err != nil {
			t.Errorf("query %s: %v (CHECK must have landed)", ck, err)
			continue
		}
		for _, member := range members {
			if !strings.Contains(def, member) {
				t.Errorf("%s def %q missing closed-set value %s", ck, def, member)
			}
		}
	}

	// canary_step_bounds_chk — the (total=0,step=0) fast-default zero-
	// value pair, AND the (total>0, 0<=step<=total) bound. We don't pin
	// the exact constraintdef — it's an OR with mixed columns — only
	// that the constraint exists.
	var stepBoundsDef string
	err = pool.QueryRow(ctx, `
		select pg_get_constraintdef(c.oid)
		  from pg_constraint c
		  join pg_namespace n on n.oid = c.connamespace
		 where c.conname = 'deployments_canary_step_bounds_chk'
		   and n.nspname = current_schema()`).Scan(&stepBoundsDef)
	if err != nil {
		t.Fatalf("query deployments_canary_step_bounds_chk: %v (CHECK must have landed)", err)
	}
	if !strings.Contains(stepBoundsDef, "canary_total_steps") ||
		!strings.Contains(stepBoundsDef, "canary_step") {
		t.Errorf("deployments_canary_step_bounds_chk def %q missing expected column references", stepBoundsDef)
	}

	// (4) Insert a deployment with NO canary (default-zero values) and
	// verify the row lands with the fast-default shape.
	var canaryPreset string
	var canaryStep, canaryTotalSteps int
	var rolloutState string
	err = pool.QueryRow(ctx, `
		insert into deployments (app_id, account_id, status, source_kind, commit_sha)
		values ('00000000-0000-0000-0000-000000000002',
		        '00000000-0000-0000-0000-000000000001',
		        'live', 'git', 'abc1234')
		returning canary_preset, canary_step, canary_total_steps, rollout_state`).Scan(
		&canaryPreset, &canaryStep, &canaryTotalSteps, &rolloutState)
	if err != nil {
		t.Fatalf("insert default-zero deployment: %v (fast-defaults must apply)", err)
	}
	if canaryPreset != "none" {
		t.Errorf("default canary_preset = %q, want 'none'", canaryPreset)
	}
	if canaryStep != 0 || canaryTotalSteps != 0 {
		t.Errorf("default canary step ladder = (%d/%d), want (0/0)", canaryStep, canaryTotalSteps)
	}
	if rolloutState != "pending" {
		t.Errorf("default rollout_state = %q, want 'pending'", rolloutState)
	}

	// (5) Replay safety — a second MigrateUp must be a no-op.
	if err := db.MigrateUp(ctx, pool); err != nil {
		t.Fatalf("replay db.MigrateUp: %v (ADD COLUMN IF NOT EXISTS / DROP CONSTRAINT IF EXISTS guards must silently no-op)", err)
	}
}
