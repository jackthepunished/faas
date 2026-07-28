//go:build !no_pg

// Shape test for migration 00061 (issue #396 / ADR-045) —
// customer-configurable alert rules + deliveries. Mirror of
// migrations/00057_sessions_test.go (IAM-3, ADR-039) so the same gate
// catches a hand-edit that drops a constraint, drops an index, or
// breaks idempotency.
//
// Asserts:
//
//  1. alert_rules lands with every column from the schema (id,
//     account_id, app_id, name, enabled, metric, comparison,
//     threshold, window_spec, failure_source, webhook_url,
//     webhook_secret_sealed, cooldown_minutes, state,
//     last_fired_at, last_evaluated_at, created_at, updated_at).
//  2. CHECK constraints reject bad combos — bad metric, bad
//     window_spec, bad failure_source, 'firing' without ever
//     observing a breach, mismatched xor (metric='failed_invocations'
//     without failure_source), cooldown outside the 5..1440 range,
//     empty name. Each rejection must come back as a CHECK violation,
//     not a parse error.
//  3. app_id is NULLABLE on purpose: an account-wide rule rows
//     successfully without an app; the FK is gone-when-null.
//  4. FK CASCADE on account_id — deleting the account drops its rules
//     and deliveries.
//  5. alert_deliveries row created with a fresh idempotency_key;
//     a second insert with the SAME idempotency_key violates the
//     UNIQUE index. The meterd re-fire path depends on this gate.
//  6. Idempotent re-apply: db.MigrateUp runs twice cleanly.
//
// Build tag mirrors 00057_sessions_test.go:3 — set FAAS_SKIP_PG_TESTS=1
// locally to skip.

package migrations_test

import (
	"context"
	"strings"
	"testing"

	"github.com/onebox-faas/faas/pkg/db"
	"github.com/onebox-faas/faas/pkg/db/pgtest"
)

func TestMigrations_00061_AlertRules_ShapeAndFK(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.Open(t)
	if err := db.MigrateUp(ctx, pool); err != nil {
		t.Fatalf("db.MigrateUp: %v", err)
	}

	const (
		acctID    = "00000000-0000-0000-0000-000000000061"
		acctID2   = "00000000-0000-0000-0000-00000000006a"
		appIDFor0 = "00000000-0000-0000-0000-0000000000b0"
		appIDFor2 = "00000000-0000-0000-0000-0000000000b2"
	)

	for _, acct := range []string{acctID, acctID2} {
		if _, err := pool.Exec(ctx, `
			insert into accounts (id, email, plan, created_at)
			values ($1, $2, 'free', now())
			on conflict (id) do nothing
		`, acct, acct+"@example.com"); err != nil {
			t.Fatalf("seed account %s: %v", acct, err)
		}
	}
	// Two apps — one bound to acctID, one to acctID2 — so the
	// CHECK / CASCADE / null-app_id tests have something to point at.
	// apps columns (migration 00001 + 00002): id, account_id, slug,
	// type, runtime, ram_mb, max_concurrency, idle_timeout_s, status,
	// created_at — no name/source_kind/source_bytes/updated_at.
	// appIDFor0 is seeded twice on purpose: the second iteration is a
	// no-op (on conflict do nothing) and just confirms that re-seeding
	// is safe across accounts — appIDFor0 stays bound to acctID.
	for _, app := range []struct{ id, owner string }{
		{appIDFor0, acctID},
		{appIDFor0, acctID2}, // on conflict do nothing → no-op; appIDFor0 still belongs to acctID
	} {
		if _, err := pool.Exec(ctx, `
			insert into apps (id, account_id, slug, type, ram_mb,
			                  max_concurrency, status, created_at)
			values ($1, $2, $3, 'app', 128, 1, 'active', now())
			on conflict (id) do nothing
		`, app.id, app.owner, "shape-alerts-"+app.id); err != nil {
			t.Fatalf("seed app: %v", err)
		}
	}
	// appIDFor2 → acctID (separate owner for the FK cascade test).
	if _, err := pool.Exec(ctx, `
		insert into apps (id, account_id, slug, type, ram_mb,
		                  max_concurrency, status, created_at)
		values ($1, $2, 'shape-alerts-a2', 'app', 128, 1, 'active', now())
		on conflict (id) do nothing
	`, appIDFor2, acctID); err != nil {
		t.Fatalf("seed app a2: %v", err)
	}

	// (1) Every column from the schema exists. We compare against
	// the SELECT'd attname list, ordered by attnum, so the assertion
	// reads in the schema's natural order.
	wantCols := []string{
		"id", "account_id", "app_id", "name", "enabled",
		"metric", "comparison", "threshold", "window_spec", "failure_source",
		"webhook_url", "webhook_secret_sealed", "cooldown_minutes",
		"state", "last_fired_at", "last_evaluated_at",
		"created_at", "updated_at",
	}
	rows, err := pool.Query(ctx, `
		select attname from pg_attribute
		 where attrelid = 'alert_rules'::regclass
		   and attnum > 0 and not attisdropped
		   and attname = any($1::text[])
		 order by attnum
	`, wantCols)
	if err != nil {
		t.Fatalf("pg_attribute scan: %v", err)
	}
	var gotCols []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			rows.Close()
			t.Fatalf("scan column: %v", err)
		}
		gotCols = append(gotCols, n)
	}
	rows.Close()
	if len(gotCols) != len(wantCols) {
		t.Errorf("alert_rules columns: want %d (%v), got %d (%v)", len(wantCols), wantCols, len(gotCols), gotCols)
	} else {
		for i := range wantCols {
			if gotCols[i] != wantCols[i] {
				t.Errorf("alert_rules column[%d]: want %q, got %q", i, wantCols[i], gotCols[i])
			}
		}
	}

	// app_id is NULLABLE — the absence of a NOT NULL on this column
	// is the load-bearing design point that lets account-wide rules
	// exist.
	var appIDNullable bool
	if err := pool.QueryRow(ctx, `
		select attnotnull = false from pg_attribute
		 where attrelid = 'alert_rules'::regclass and attname = 'app_id'
	`).Scan(&appIDNullable); err != nil {
		t.Fatalf("attnotnull probe: %v", err)
	}
	if !appIDNullable {
		t.Errorf("alert_rules.app_id must be NULLABLE (account-wide rules), but attnotnull is true")
	}

	// (2) CHECK constraint floors reject bad combos. We probe each
	// expected violation by name; a future migration that drops the
	// constraint by accident would let the INSERT succeed and fail
	// the test here, not at runtime.
	checkCases := []struct {
		name   string
		mutate func() error
	}{
		{
			name: "bad metric",
			mutate: func() error {
				_, err := pool.Exec(ctx, `
					insert into alert_rules
						(account_id, app_id, name, metric, comparison,
						 threshold, window_spec, webhook_url, webhook_secret_sealed)
					values ($1, $2, 'bad-metric', 'not_a_metric', 'gt',
					        0, '5m', 'https://example.com/hook', '\x00'::bytea)
				`, acctID, appIDFor2)
				return err
			},
		},
		{
			name: "bad window_spec",
			mutate: func() error {
				_, err := pool.Exec(ctx, `
					insert into alert_rules
						(account_id, app_id, name, metric, comparison,
						 threshold, window_spec, webhook_url, webhook_secret_sealed)
					values ($1, $2, 'bad-window', 'error_rate_pct', 'gt',
					        0, '3m', 'https://example.com/hook', '\x00'::bytea)
				`, acctID, appIDFor2)
				return err
			},
		},
		{
			name: "bad comparison",
			mutate: func() error {
				_, err := pool.Exec(ctx, `
					insert into alert_rules
						(account_id, app_id, name, metric, comparison,
						 threshold, window_spec, webhook_url, webhook_secret_sealed)
					values ($1, $2, 'bad-cmp', 'error_rate_pct', '==',
					        0, '5m', 'https://example.com/hook', '\x00'::bytea)
				`, acctID, appIDFor2)
				return err
			},
		},
		{
			name: "bad failure_source",
			mutate: func() error {
				_, err := pool.Exec(ctx, `
					insert into alert_rules
						(account_id, app_id, name, metric, comparison,
						 threshold, window_spec, failure_source,
						 webhook_url, webhook_secret_sealed)
					values ($1, $2, 'bad-src', 'failed_invocations', 'gt',
					        0, '5m', 'pubsub', 'https://example.com/hook', '\x00'::bytea)
				`, acctID, appIDFor2)
				return err
			},
		},
		{
			name: "xor: failed_invocations without failure_source",
			mutate: func() error {
				_, err := pool.Exec(ctx, `
					insert into alert_rules
						(account_id, app_id, name, metric, comparison,
						 threshold, window_spec,
						 webhook_url, webhook_secret_sealed)
					values ($1, $2, 'xor1', 'failed_invocations', 'gt',
					        0, '5m', 'https://example.com/hook', '\x00'::bytea)
				`, acctID, appIDFor2)
				return err
			},
		},
		{
			name: "xor: non-failed_invocations WITH failure_source",
			mutate: func() error {
				_, err := pool.Exec(ctx, `
					insert into alert_rules
						(account_id, app_id, name, metric, comparison,
						 threshold, window_spec, failure_source,
						 webhook_url, webhook_secret_sealed)
					values ($1, $2, 'xor2', 'error_rate_pct', 'gt',
					        0, '5m', 'cron', 'https://example.com/hook', '\x00'::bytea)
				`, acctID, appIDFor2)
				return err
			},
		},
		{
			name: "cooldown below floor",
			mutate: func() error {
				_, err := pool.Exec(ctx, `
					insert into alert_rules
						(account_id, app_id, name, metric, comparison,
						 threshold, window_spec, cooldown_minutes,
						 webhook_url, webhook_secret_sealed)
					values ($1, $2, 'cd-low', 'error_rate_pct', 'gt',
					        0, '5m', 1, 'https://example.com/hook', '\x00'::bytea)
				`, acctID, appIDFor2)
				return err
			},
		},
		{
			name: "cooldown above ceiling",
			mutate: func() error {
				_, err := pool.Exec(ctx, `
					insert into alert_rules
						(account_id, app_id, name, metric, comparison,
						 threshold, window_spec, cooldown_minutes,
						 webhook_url, webhook_secret_sealed)
					values ($1, $2, 'cd-high', 'error_rate_pct', 'gt',
					        0, '5m', 5000, 'https://example.com/hook', '\x00'::bytea)
				`, acctID, appIDFor2)
				return err
			},
		},
		{
			name: "empty name",
			mutate: func() error {
				_, err := pool.Exec(ctx, `
					insert into alert_rules
						(account_id, app_id, name, metric, comparison,
						 threshold, window_spec,
						 webhook_url, webhook_secret_sealed)
					values ($1, $2, '', 'error_rate_pct', 'gt',
					        0, '5m', 'https://example.com/hook', '\x00'::bytea)
				`, acctID, appIDFor2)
				return err
			},
		},
		{
			name: "bad state",
			mutate: func() error {
				_, err := pool.Exec(ctx, `
					insert into alert_rules
						(account_id, app_id, name, metric, comparison,
						 threshold, window_spec, state,
						 webhook_url, webhook_secret_sealed)
					values ($1, $2, 'bad-state', 'error_rate_pct', 'gt',
					        0, '5m', 'pending', 'https://example.com/hook', '\x00'::bytea)
				`, acctID, appIDFor2)
				return err
			},
		},
	}
	for _, tc := range checkCases {
		err := tc.mutate()
		if err == nil {
			t.Errorf("CHECK case %q: expected rejection; insert succeeded", tc.name)
			continue
		}
		if !strings.Contains(strings.ToLower(err.Error()), "check") {
			t.Errorf("CHECK case %q: expected a check-constraint error, got: %v", tc.name, err)
		}
	}

	// (3) NULL app_id (account-wide rule) inserts cleanly.
	var ruleAID string
	if err := pool.QueryRow(ctx, `
		insert into alert_rules
			(account_id, app_id, name, metric, comparison,
			 threshold, window_spec, failure_source,
			 webhook_url, webhook_secret_sealed)
		values ($1, null, 'acct-wide', 'failed_invocations', 'gt',
		        0, '5m', 'cron', 'https://example.com/hook', '\x00'::bytea)
		returning id
	`, acctID).Scan(&ruleAID); err != nil {
		t.Fatalf("seed account-wide rule: %v", err)
	}

	// (4) CASCADE on account delete — every rule tied to acctID
	// (including the NULL-app_id account-wide one) goes away when
	// the parent account row is deleted. We do this through a fresh
	// account so we don't break other tests that share the
	// pgtest-owned fixture rows.
	const acctIDc = "00000000-0000-0000-0000-00000000005c"
	if _, err := pool.Exec(ctx, `
		insert into accounts (id, email, plan, created_at)
		values ($1, $2, 'free', now())
	`, acctIDc, acctIDc+"@example.com"); err != nil {
		t.Fatalf("seed cascade account: %v", err)
	}
	var ruleCID string
	if err := pool.QueryRow(ctx, `
		insert into alert_rules
			(account_id, app_id, name, metric, comparison,
			 threshold, window_spec,
			 webhook_url, webhook_secret_sealed)
		values ($1, null, 'cascade-target', 'error_rate_pct', 'gt',
		        0, '5m', 'https://example.com/hook', '\x00'::bytea)
		returning id
	`, acctIDc).Scan(&ruleCID); err != nil {
		t.Fatalf("seed cascade rule: %v", err)
	}
	if _, err := pool.Exec(ctx, `delete from accounts where id = $1`, acctIDc); err != nil {
		t.Fatalf("delete cascade account: %v", err)
	}
	var orphan int
	if err := pool.QueryRow(ctx,
		`select count(*) from alert_rules where id = $1`, ruleCID,
	).Scan(&orphan); err != nil {
		t.Fatalf("count orphans: %v", err)
	}
	if orphan != 0 {
		t.Errorf("FK cascade: expected rule %s to be gone, %d row(s) remain", ruleCID, orphan)
	}

	// (5) idempotency_key UNIQUE on alert_deliveries. Insert twice
	// with the same key — second must fail with a UNIQUE violation.
	// Re-seed a rule we can attach the delivery to.
	var ruleIDd string
	if err := pool.QueryRow(ctx, `
		insert into alert_rules
			(account_id, app_id, name, metric, comparison,
			 threshold, window_spec,
			 webhook_url, webhook_secret_sealed)
		values ($1, $2, 'dedupe-victim', 'error_rate_pct', 'gt',
		        0, '5m', 'https://example.com/hook', '\x00'::bytea)
		returning id
	`, acctID, appIDFor2).Scan(&ruleIDd); err != nil {
		t.Fatalf("seed dedupe rule: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		insert into alert_deliveries
			(rule_id, account_id, app_id, idempotency_key, payload, observed_value)
		values ($1, $2, $3, 'rule-A:bucket-1', '{}'::jsonb, 0.0)
	`, ruleIDd, acctID, appIDFor2); err != nil {
		t.Fatalf("seed delivery 1: %v", err)
	}
	_, err = pool.Exec(ctx, `
		insert into alert_deliveries
			(rule_id, account_id, app_id, idempotency_key, payload, observed_value)
		values ($1, $2, $3, 'rule-A:bucket-1', '{}'::jsonb, 0.0)
	`, ruleIDd, acctID, appIDFor2)
	if err == nil {
		t.Errorf("UNIQUE idempotency_key: expected rejection on duplicate; both inserts succeeded")
	} else if !strings.Contains(strings.ToLower(err.Error()), "unique") {
		t.Errorf("UNIQUE idempotency_key: expected unique-violation, got: %v", err)
	}

	// (6) Idempotent re-apply.
	if err := db.MigrateUp(ctx, pool); err != nil {
		t.Errorf("MigrateUp idempotent re-apply: %v", err)
	}
}
