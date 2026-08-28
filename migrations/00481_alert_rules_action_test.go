// migrations/00481_alert_rules_action_test.go — pins the shape
// of the action column on alert_rules (issue #976 / ADR-122 /
// SAFE-RELEASES-B). Build tag mirrors the precedent at
// migrations/00410_app_secret_value_hash_test.go; set
// FAAS_SKIP_PG_TESTS=1 locally to skip.
//
// Asserts:
//   1. The migration set applies cleanly through 00381.
//   2. The action column lands with type text, NOT NULL, default
//      'webhook', and a closed-set CHECK covering {webhook,
//      rollback, demote, promote}.
//   3. A pre-PR row's default action is 'webhook' (back-compat).
//   4. Inserting an out-of-set value is rejected with 23514.
//   5. Re-running db.MigrateUp is a no-op (replay safety).

//go:build pg

package migrations_test

import (
	"context"
	"strings"
	"testing"

	"github.com/onebox-faas/faas/pkg/db"
	"github.com/onebox-faas/faas/pkg/db/pgtest"
)

func TestMigrations_00481_AlertRulesAction(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.Open(t)

	// (1) Run the full migration set. 00381 should land last.
	if err := db.MigrateUp(ctx, pool); err != nil {
		t.Fatalf("db.MigrateUp: %v (PR follow-up failure mode: missing migration slot before 00381)", err)
	}

	// (2) Column shape on alert_rules.
	var typ, nullable string
	var def *string
	err := pool.QueryRow(ctx, `
		select data_type, is_nullable, column_default
		  from information_schema.columns
		 where table_schema = current_schema()
		   and table_name = 'alert_rules'
		   and column_name = 'action'`).Scan(&typ, &nullable, &def)
	if err != nil {
		t.Fatalf("query alert_rules.action: %v (column must land)", err)
	}
	if typ != "text" {
		t.Errorf("alert_rules.action type = %q, want 'text'", typ)
	}
	if nullable != "NO" {
		t.Errorf("alert_rules.action nullable = %q, want 'NO'", nullable)
	}
	if def == nil || *def == "" {
		t.Error("alert_rules.action has no DEFAULT (PG11+ fast-default required for back-compat on pre-PR rows)")
	}

	// Closed-set CHECK. pg_get_constraintdef emits IN or ANY(ARRAY[...])
	// — we assert each closed-set value is referenced.
	var ckDef string
	err = pool.QueryRow(ctx, `
		select pg_get_constraintdef(c.oid)
		  from pg_constraint c
		  join pg_namespace n on n.oid = c.connamespace
		 where c.conname = 'alert_rules_action_chk'
		   and n.nspname = current_schema()`).Scan(&ckDef)
	if err != nil {
		t.Fatalf("query alert_rules_action_chk: %v (CHECK must have landed)", err)
	}
	for _, want := range []string{"'webhook'", "'rollback'", "'demote'", "'promote'"} {
		if !strings.Contains(ckDef, want) {
			t.Errorf("alert_rules_action_chk def %q missing closed-set value %s", ckDef, want)
		}
	}

	// (3) Default back-compat: a row inserted WITHOUT action lands with
	// 'webhook' (the legacy Dispatcher fan-out).
	var action string
	err = pool.QueryRow(ctx, `
		insert into alert_rules (account_id, name, metric, comparison, threshold, window_spec, webhook_url, webhook_secret_sealed)
		values ('00000000-0000-0000-0000-000000000001',
		        'PR-481-DEFAULT',
		        'error_rate_pct', 'gt', 5.0, '5m',
		        'https://example.test/hook',
		        E'\\x00'::bytea)
		returning action`).Scan(&action)
	if err != nil {
		t.Fatalf("insert default-zero alert_rule: %v (the column MUST default to 'webhook' for back-compat)", err)
	}
	if action != "webhook" {
		t.Errorf("default action = %q, want 'webhook'", action)
	}

	// (4) Closed-set enforcement — out-of-set value is rejected with
	// 23514 check_violation.
	_, err = pool.Exec(ctx, `
		insert into alert_rules (account_id, name, metric, comparison, threshold, window_spec, webhook_url, webhook_secret_sealed, action)
		values ('00000000-0000-0000-0000-000000000001',
		        'PR-481-BAD',
		        'error_rate_pct', 'gt', 5.0, '5m',
		        'https://example.test/hook',
		        E'\\x00'::bytea,
		        'evict')`)
	if err == nil {
		t.Fatal("inserting action='evict' accepted; MUST be rejected by closed-set CHECK")
	}
	if !strings.Contains(err.Error(), "23514") {
		t.Errorf("expected 23514 check_violation; got %v", err)
	}

	// (5) Replay safety — second MigrateUp must be a no-op.
	if err := db.MigrateUp(ctx, pool); err != nil {
		t.Fatalf("replay db.MigrateUp: %v (ADD COLUMN IF NOT EXISTS / DROP CONSTRAINT IF EXISTS guards must silently no-op)", err)
	}
}
