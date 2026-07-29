//go:build !no_pg

// Migration-apply test for 00065 (ADR-046, per-instance egress
// metering, visibility only).
//
// Asserts the new columns tx_bytes and net_tx_bytes exist on
// usage_minutes, both default to 0 on insert (so existing PR-B /
// PR-346 callers that don't mention the new columns keep working),
// and that the recreated usage_monthly view sums both columns.
//
// Build tag mirrors apply_walk_test.go:4 — set FAAS_SKIP_PG_TESTS=1
// locally to skip.

package migrations_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/onebox-faas/faas/pkg/db"
	"github.com/onebox-faas/faas/pkg/db/pgtest"
)

func TestMigrations_00065_Egress_AddsTxBytesAndNetTxBytes(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.Open(t)
	if err := db.MigrateUp(ctx, pool); err != nil {
		t.Fatalf("db.MigrateUp: %v", err)
	}

	// (1) Both columns exist on usage_minutes, are NOT NULL, and
	//     default to 0. information_schema.columns is the same source
	//     pg_dump uses for introspection — canonical "did the ALTER
	//     TABLE land?" probe.
	for _, col := range []string{"tx_bytes", "net_tx_bytes"} {
		var dataType string
		var nullable string
		var def string
		if err := pool.QueryRow(ctx, `
			select data_type, is_nullable, coalesce(column_default, '')
			  from information_schema.columns
			 where table_schema = current_schema()
			   and table_name   = 'usage_minutes'
			   and column_name  = $1
		`, col).Scan(&dataType, &nullable, &def); err != nil {
			t.Errorf("usage_minutes.%s not present after migrations apply: %v", col, err)
			continue
		}
		if dataType != "bigint" {
			t.Errorf("usage_minutes.%s data_type = %q, want bigint", col, dataType)
		}
		if nullable != "NO" {
			t.Errorf("usage_minutes.%s is_nullable = %q, want NO (NOT NULL DEFAULT 0)", col, nullable)
		}
		if def != "0" {
			t.Errorf("usage_minutes.%s column_default = %q, want 0", col, def)
		}
	}

	// (2) Seed the FK chain (account + app + deployment + instance)
	//     and an insert that OMITS the new columns. The default of 0
	//     must apply — proves the DEFAULT 0 contract is wired so
	//     existing callers (PK-1, M7 hardening) don't break.
	acct := uuid.NewString()
	appID := uuid.NewString()
	depID := uuid.NewString()
	insID := uuid.NewString()
	if _, err := pool.Exec(ctx, `
		insert into accounts (id, email, plan, created_at)
		values ($1, $2, 'hobby', now())
	`, acct, acct+"@example.com"); err != nil {
		t.Fatalf("seed account: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		insert into apps (id, account_id, slug, ram_mb, max_concurrency, idle_timeout_s, status, created_at)
		values ($1, $2, 'egress-default-test', 256, 1, 30, 'active', now())
	`, appID, acct); err != nil {
		t.Fatalf("seed app: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		insert into deployments (id, app_id, kind, image_digest, status, created_at)
		values ($1, $2, 'image', 'sha256:cafe', 'live', now())
	`, depID, appID); err != nil {
		t.Fatalf("seed deployment: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		insert into instances (id, app_id, deployment_id, state, ram_mb, started_at)
		values ($1, $2, $3, 'running', 256, now())
	`, insID, appID, depID); err != nil {
		t.Fatalf("seed instance: %v", err)
	}

	// Insert without tx_bytes / net_tx_bytes — must succeed and the
	// default of 0 must apply. This is the load-bearing acceptance
	// that PK-1 / M7 hardening AppendUsage callers keep working.
	if _, err := pool.Exec(ctx, `
		insert into usage_minutes (account_id, app_id, instance_id, minute, mb_seconds, requests)
		values ($1, $2, $3, date_trunc('minute', now()), 1000, 5)
	`, acct, appID, insID); err != nil {
		t.Fatalf("insert usage_minutes without new columns (must succeed under DEFAULT 0): %v", err)
	}
	var txBytes, netTxBytes int64
	if err := pool.QueryRow(ctx, `
		select tx_bytes, net_tx_bytes
		  from usage_minutes
		 where instance_id = $1
	`, insID).Scan(&txBytes, &netTxBytes); err != nil {
		t.Fatalf("read back default tx_bytes/net_tx_bytes: %v", err)
	}
	if txBytes != 0 || netTxBytes != 0 {
		t.Errorf("default tx_bytes=%d, net_tx_bytes=%d; want 0,0 (DEFAULT 0 contract)", txBytes, netTxBytes)
	}

	// (3) Insert WITH the new columns — values persist. Proves the
	//     columns accept real values and the round-trip shape is
	//     correct end-to-end (used by the future billing PR).
	if _, err := pool.Exec(ctx, `
		insert into usage_minutes (account_id, app_id, instance_id, minute, mb_seconds, requests, tx_bytes, net_tx_bytes)
		values ($1, $2, $3, date_trunc('minute', now()) - interval '1 minute', 2000, 7, 4096, 4500)
	`, acct, appID, insID); err != nil {
		t.Fatalf("insert usage_minutes with tx_bytes/net_tx_bytes: %v", err)
	}
	var got struct{ tx, net int64 }
	if err := pool.QueryRow(ctx, `
		select tx_bytes, net_tx_bytes
		  from usage_minutes
		 where instance_id = $1
		   and mb_seconds = 2000
	`, insID).Scan(&got.tx, &got.net); err != nil {
		t.Fatalf("read back populated tx_bytes/net_tx_bytes: %v", err)
	}
	if got.tx != 4096 || got.net != 4500 {
		t.Errorf("persisted tx_bytes=%d, net_tx_bytes=%d; want 4096, 4500", got.tx, got.net)
	}

	// (4) usage_monthly view sums both columns — proves the recreated
	//     view shape matches usage_minutes and that a SELECT against
	//     the view returns the expected columns (load-bearing for the
	//     future UsageByMonth read path in pgstore).
	rows, err := pool.Query(ctx, `
		select tx_bytes, net_tx_bytes
		  from usage_monthly
		 where account_id = $1 and app_id = $2
	`, acct, appID)
	if err != nil {
		t.Fatalf("select usage_monthly: %v", err)
	}
	defer rows.Close()
	var sumTx, sumNet int64
	for rows.Next() {
		var tx, net int64
		if err := rows.Scan(&tx, &net); err != nil {
			t.Fatalf("scan usage_monthly row: %v", err)
		}
		sumTx += tx
		sumNet += net
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("usage_monthly iteration: %v", err)
	}
	if sumTx != 4096 {
		t.Errorf("usage_monthly SUM(tx_bytes) = %d, want 4096", sumTx)
	}
	if sumNet != 4500 {
		t.Errorf("usage_monthly SUM(net_tx_bytes) = %d, want 4500", sumNet)
	}

	// (5) The ADD COLUMN contract is enforced: NULL inserts into the
	//     new columns are rejected with SQLSTATE 23502 (not_null_violation)
	//     because the column is NOT NULL DEFAULT 0 — proves the
	//     DEFAULT was applied at table-create time, not via a
	//     backfill UPDATE that could leave a window of NULLs.
	_, err = pool.Exec(ctx, `
		insert into usage_minutes (account_id, app_id, instance_id, minute, mb_seconds, requests, tx_bytes, net_tx_bytes)
		values ($1, $2, $3, date_trunc('minute', now()) - interval '2 minute', 3000, 1, null, 10)
	`, acct, appID, insID)
	if err == nil {
		t.Errorf("explicit NULL tx_bytes must be rejected by NOT NULL constraint")
	} else {
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) {
			t.Errorf("explicit-NULL tx_bytes error not a *pgconn.PgError: %v", err)
		} else if pgErr.Code != "23502" {
			t.Errorf("explicit NULL tx_bytes SQLSTATE = %q, want 23502 (not_null_violation); full: %v", pgErr.Code, err)
		}
	}
}
