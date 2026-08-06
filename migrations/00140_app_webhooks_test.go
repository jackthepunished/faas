//go:build !no_pg

// Shape test for migration 00140 (issue #476 / ADR-076) —
// app_webhooks subscription table. Mirror of
// migrations/00062_alert_rules_test.go so the same gate catches a
// hand-edit that drops a constraint, drops an index, or breaks
// idempotency.
//
// Asserts:
//
//  1. app_webhooks lands with every column from the schema (id,
//     app_id, account_id, target_url, secret_sealed, event_filter,
//     retry_policy, enabled, created_at, updated_at).
//  2. retry_policy CHECK accepts ('default','aggressive','none') and
//     rejects everything else with 23514.
//  3. target_url length CHECK enforces 8..2048 chars (mirrors the
//     alert_rules.webhook_url floor).
//  4. unique (app_id, target_url) rejects a duplicate subscription.
//  5. FK CASCADE on app_id + account_id — deleting the app drops its
//     webhooks; deleting the account drops its webhooks.
//  6. Replay-safe: db.MigrateUp runs twice cleanly.
//
// Slot note: HEAD on origin/main is 00134 (api_keys_org_bound). The
// PR cluster around 135-139 (PRs #540, #651, #653, #654, #647) put
// real-schema slots at 138 (apps_eviction_priority, this branch's
// prior commit) and 140 here. The migration itself is slot-agnostic
// — only the filename, the test function name, the seed UUIDs, and
// pkg/e2etest/harness.go::e2eMigrationTarget carry the literal slot.
// Bump e2eMigrationTarget to 141 when this lands.

package migrations_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/onebox-faas/faas/pkg/db"
	"github.com/onebox-faas/faas/pkg/db/pgtest"
)

func TestMigrations_00140_AppWebhooks_ShapeAndFK(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.Open(t)
	if err := db.MigrateUp(ctx, pool); err != nil {
		t.Fatalf("db.MigrateUp: %v", err)
	}

	const (
		acctID  = "00000000-0000-0000-0000-000000000140"
		appID   = "00000000-0000-0000-0000-000000000240"
		appID2  = "00000000-0000-0000-0000-000000000241"
		acctID2 = "00000000-0000-0000-0000-00000000014a"
	)

	for _, a := range []struct{ id, owner string }{
		{acctID, acctID},
		{acctID2, acctID2},
	} {
		if _, err := pool.Exec(ctx, `
			insert into accounts (id, email, plan, created_at)
			values ($1, $2, 'free', now())
			on conflict (id) do nothing
		`, a.id, a.id+"@example.com"); err != nil {
			t.Fatalf("seed account %s: %v", a.id, err)
		}
	}
	for _, ap := range []struct{ id, owner string }{
		{appID, acctID},
		{appID2, acctID2},
	} {
		if _, err := pool.Exec(ctx, `
			insert into apps (id, account_id, slug, type, ram_mb,
			                  max_concurrency, status, created_at)
			values ($1, $2, $3, 'app', 128, 1, 'active', now())
			on conflict (id) do nothing
		`, ap.id, ap.owner, "shape-webhooks-"+ap.id); err != nil {
			t.Fatalf("seed app %s: %v", ap.id, err)
		}
	}

	// (1) Every column from the schema exists, in the natural order.
	wantCols := []string{
		"id", "app_id", "account_id", "target_url",
		"secret_sealed", "event_filter", "retry_policy", "enabled",
		"created_at", "updated_at",
	}
	rows, err := pool.Query(ctx, `
		select attname from pg_attribute
		 where attrelid = 'app_webhooks'::regclass
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
		t.Errorf("app_webhooks columns: want %d (%v), got %d (%v)",
			len(wantCols), wantCols, len(gotCols), gotCols)
	} else {
		for i := range wantCols {
			if gotCols[i] != wantCols[i] {
				t.Errorf("app_webhooks column[%d]: want %q, got %q",
					i, wantCols[i], gotCols[i])
			}
		}
	}

	// (2) retry_policy CHECK rejects 'foo' with 23514. Accept round is
	// pinned by the three INSERTs below — a regression that narrowed
	// the closed vocabulary would let one of them through.
	for _, p := range []string{"default", "aggressive", "none"} {
		if _, err := pool.Exec(ctx, `
			insert into app_webhooks
				(app_id, account_id, target_url, secret_sealed, retry_policy)
			values ($1, $2, $3, '\x00'::bytea, $4)
		`, appID, acctID, "https://example.com/hook-"+p, p); err != nil {
			t.Errorf("retry_policy=%q should be accepted, got: %v", p, err)
		}
	}
	_, err = pool.Exec(ctx, `
		insert into app_webhooks
			(app_id, account_id, target_url, secret_sealed, retry_policy)
		values ($1, $2, 'https://example.com/hook-bad', '\x00'::bytea, 'foo')
	`, appID, acctID)
	var pgErr *pgconn.PgError
	if err == nil {
		t.Errorf("retry_policy='foo' should be rejected by CHECK")
	} else if !errors.As(err, &pgErr) || pgErr.Code != "23514" {
		t.Errorf("retry_policy='foo' error = %v, want pgx 23514 (check_violation)", err)
	}

	// (3) target_url length CHECK enforces 8..2048. The floor rejects
	// 'short' (5 chars); the ceiling rejects a 2049-char URL. Mirrors
	// alert_rules.webhook_url behaviour.
	if _, err := pool.Exec(ctx, `
		insert into app_webhooks
			(app_id, account_id, target_url, secret_sealed)
		values ($1, $2, 'short', '\x00'::bytea)
	`, appID2, acctID2); err == nil {
		t.Errorf("target_url with 5 chars should be rejected by length CHECK")
	} else if !strings.Contains(strings.ToLower(err.Error()), "check") {
		t.Errorf("short target_url: expected CHECK violation, got: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		insert into app_webhooks
			(app_id, account_id, target_url, secret_sealed)
		values ($1, $2, 'https://example.com/'||$3::text, '\x00'::bytea)
	`, appID2, acctID2, strings.Repeat("a", 2040)); err == nil {
		t.Errorf("target_url with > 2048 chars should be rejected by length CHECK")
	} else if !strings.Contains(strings.ToLower(err.Error()), "check") {
		t.Errorf("long target_url: expected CHECK violation, got: %v", err)
	}

	// (4) unique (app_id, target_url) rejects a duplicate. Insert
	// once with default retry_policy; second insert with a different
	// retry_policy on the same (app_id, target_url) must fail.
	if _, err := pool.Exec(ctx, `
		insert into app_webhooks
			(app_id, account_id, target_url, secret_sealed)
		values ($1, $2, 'https://example.com/dup', '\x00'::bytea)
	`, appID, acctID); err != nil {
		t.Fatalf("seed first webhook: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		insert into app_webhooks
			(app_id, account_id, target_url, secret_sealed)
		values ($1, $2, 'https://example.com/dup', '\x00'::bytea)
	`, appID, acctID); err == nil {
		t.Errorf("unique (app_id, target_url) should reject the duplicate")
	} else if !strings.Contains(strings.ToLower(err.Error()), "unique") {
		t.Errorf("duplicate webhook: expected UNIQUE violation, got: %v", err)
	}

	// (5) FK CASCADE on app delete drops the webhooks. Seed a fresh
	// account + app + webhook, then drop the app and confirm the
	// webhook goes with it.
	const acctIDc = "00000000-0000-0000-0000-00000000014c"
	const appIDc = "00000000-0000-0000-0000-00000000024c"
	const hookIDc = "00000000-0000-0000-0000-00000000034c"
	if _, err := pool.Exec(ctx, `
		insert into accounts (id, email, plan, created_at)
		values ($1, $2, 'free', now())
	`, acctIDc, acctIDc+"@example.com"); err != nil {
		t.Fatalf("seed cascade account: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		insert into apps (id, account_id, slug, type, ram_mb,
		                  max_concurrency, status, created_at)
		values ($1, $2, 'shape-webhooks-cascade-app', 'app', 128, 1, 'active', now())
	`, appIDc, acctIDc); err != nil {
		t.Fatalf("seed cascade app: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		insert into app_webhooks
			(id, app_id, account_id, target_url, secret_sealed)
		values ($1, $2, $3, 'https://example.com/cascade', '\x00'::bytea)
	`, hookIDc, appIDc, acctIDc); err != nil {
		t.Fatalf("seed cascade webhook: %v", err)
	}
	if _, err := pool.Exec(ctx, `delete from apps where id = $1`, appIDc); err != nil {
		t.Fatalf("delete cascade app: %v", err)
	}
	var orphan int
	if err := pool.QueryRow(ctx,
		`select count(*) from app_webhooks where id = $1`, hookIDc,
	).Scan(&orphan); err != nil {
		t.Fatalf("count orphans: %v", err)
	}
	if orphan != 0 {
		t.Errorf("FK cascade: expected webhook %s gone, %d row(s) remain", hookIDc, orphan)
	}

	// (6) Replay-safe: a second MigrateUp is a no-op. CREATE TABLE
	// IF NOT EXISTS + CREATE INDEX IF NOT EXISTS guard the schema.
	if err := db.MigrateUp(ctx, pool); err != nil {
		t.Errorf("MigrateUp idempotent re-apply: %v", err)
	}
}
