//go:build !no_pg

// Migration-apply test for 00067 (ADR-048, extend metering
// telemetry: ingress bytes, WakeMethod, builder-time, usage_daily
// rollup).
//
// Asserts the four new columns (net_rx_bytes, cold_boot_count,
// builder_seconds, builder_kind) exist on usage_minutes, all
// default to 0 / 'none' on insert (so existing PR-046 / PR-279
// callers that don't mention the new columns keep working), that
// the recreated usage_monthly view sums them, and that the new
// usage_daily rollup table has the expected shape and primary key.
//
// Build tag mirrors apply_walk_test.go:4 — set FAAS_SKIP_PG_TESTS=1
// locally to skip.

package migrations_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/onebox-faas/faas/pkg/db"
	"github.com/onebox-faas/faas/pkg/db/pgtest"
)

func TestMigrations_00067_ExtendMeteringTelemetry(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.Open(t)
	if err := db.MigrateUp(ctx, pool); err != nil {
		t.Fatalf("db.MigrateUp: %v", err)
	}

	// (1) All four columns exist on usage_minutes with the right
	//     shape. net_rx_bytes / cold_boot_count / builder_seconds
	//     are bigint; cold_boot_count is integer (smaller cardinality,
	//     same shape as cpu_usec on the per-row side); builder_kind
	//     is text with default 'none'. information_schema.columns is
	//     the same source pg_dump uses — canonical probe.
	type colExpect struct {
		name     string
		wantType string
		wantNull string
		wantDef  string
	}
	cases := []colExpect{
		{"net_rx_bytes", "bigint", "NO", "0"},
		{"cold_boot_count", "integer", "NO", "0"},
		{"builder_seconds", "bigint", "NO", "0"},
		{"builder_kind", "text", "NO", "'none'::text"},
	}
	for _, c := range cases {
		var dataType, nullable, def string
		if err := pool.QueryRow(ctx, `
			select data_type, is_nullable, coalesce(column_default, '')
			  from information_schema.columns
			 where table_schema = current_schema()
			   and table_name   = 'usage_minutes'
			   and column_name  = $1
		`, c.name).Scan(&dataType, &nullable, &def); err != nil {
			t.Errorf("usage_minutes.%s not present after migrations apply: %v", c.name, err)
			continue
		}
		if dataType != c.wantType {
			t.Errorf("usage_minutes.%s data_type = %q, want %q", c.name, dataType, c.wantType)
		}
		if nullable != c.wantNull {
			t.Errorf("usage_minutes.%s is_nullable = %q, want %q", c.name, nullable, c.wantNull)
		}
		if def != c.wantDef {
			t.Errorf("usage_minutes.%s column_default = %q, want %q", c.name, def, c.wantDef)
		}
	}

	// (2) Seed the FK chain (account + app + deployment + instance)
	//     and an insert that OMITS the new columns. The defaults of
	//     0 / 'none' must apply — proves the DEFAULT contract is
	//     wired so existing ADR-039 / ADR-046 callers keep working.
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
		values ($1, $2, 'extend-metering-default-test', 256, 1, 30, 'active', now())
	`, appID, acct); err != nil {
		t.Fatalf("seed app: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		insert into deployments (id, app_id, kind, image_digest, status, created_at)
		values ($1, $2, 'image', 'sha256:cafe', 'live', now())
	`, depID, appID); err != nil {
		t.Fatalf("seed deployment: %v", err)
	}
	var nodeID string
	if err := pool.QueryRow(ctx, `
		select id from compute_nodes where name = 'default-local' limit 1
	`).Scan(&nodeID); err != nil {
		t.Fatalf("lookup default-local compute_node id: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		insert into instances (id, app_id, deployment_id, node_id, state, ram_mb, started_at)
		values ($1, $2, $3, $4, 'running', 256, now())
	`, insID, appID, depID, nodeID); err != nil {
		t.Fatalf("seed instance: %v", err)
	}

	// Insert without the new columns — must succeed and defaults apply.
	if _, err := pool.Exec(ctx, `
		insert into usage_minutes (account_id, app_id, instance_id, minute, mb_seconds, requests)
		values ($1, $2, $3, date_trunc('minute', now()), 1000, 5)
	`, acct, appID, insID); err != nil {
		t.Fatalf("insert usage_minutes without new columns (must succeed under DEFAULT 0 / 'none'): %v", err)
	}
	var netRx int64
	var coldBoots int32
	var builderSec int64
	var builderKind string
	if err := pool.QueryRow(ctx, `
		select net_rx_bytes, cold_boot_count, builder_seconds, builder_kind
		  from usage_minutes
		 where instance_id = $1
	`, insID).Scan(&netRx, &coldBoots, &builderSec, &builderKind); err != nil {
		t.Fatalf("read back default net_rx_bytes/cold_boot_count/builder_seconds/builder_kind: %v", err)
	}
	if netRx != 0 || coldBoots != 0 || builderSec != 0 || builderKind != "none" {
		t.Errorf("default net_rx_bytes=%d, cold_boot_count=%d, builder_seconds=%d, builder_kind=%q; want 0,0,0,'none'",
			netRx, coldBoots, builderSec, builderKind)
	}

	// (3) Insert WITH the new columns — values persist. Proves the
	//     columns accept real values and the round-trip shape is
	//     correct end-to-end (used by the producer wiring in
	//     PR-A tasks A.3 / A.3b / A.3c).
	if _, err := pool.Exec(ctx, `
		insert into usage_minutes (account_id, app_id, instance_id, minute, mb_seconds, requests,
		                          net_rx_bytes, cold_boot_count, builder_seconds, builder_kind)
		values ($1, $2, $3, date_trunc('minute', now()) - interval '1 minute',
		        2000, 7, 8192, 3, 120, 'railpack')
	`, acct, appID, insID); err != nil {
		t.Fatalf("insert usage_minutes with new columns: %v", err)
	}
	var got struct {
		rx    int64
		cold  int32
		bSec  int64
		bKind string
	}
	if err := pool.QueryRow(ctx, `
		select net_rx_bytes, cold_boot_count, builder_seconds, builder_kind
		  from usage_minutes
		 where instance_id = $1
		   and mb_seconds = 2000
	`, insID).Scan(&got.rx, &got.cold, &got.bSec, &got.bKind); err != nil {
		t.Fatalf("read back populated new columns: %v", err)
	}
	if got.rx != 8192 || got.cold != 3 || got.bSec != 120 || got.bKind != "railpack" {
		t.Errorf("persisted net_rx_bytes=%d, cold_boot_count=%d, builder_seconds=%d, builder_kind=%q; want 8192, 3, 120, 'railpack'",
			got.rx, got.cold, got.bSec, got.bKind)
	}

	// (4) usage_monthly view sums all four new columns — proves the
	//     recreated view shape matches usage_minutes and that a
	//     SELECT against the view returns the expected columns
	//     (load-bearing for UsageByMonth in pgstore).
	rows, err := pool.Query(ctx, `
		select net_rx_bytes, cold_boot_count, builder_seconds
		  from usage_monthly
		 where account_id = $1 and app_id = $2
	`, acct, appID)
	if err != nil {
		t.Fatalf("select usage_monthly: %v", err)
	}
	defer rows.Close()
	var sumRx, sumCold, sumBuilder int64
	for rows.Next() {
		var rx, cold, b int64
		if err := rows.Scan(&rx, &cold, &b); err != nil {
			t.Fatalf("scan usage_monthly row: %v", err)
		}
		sumRx += rx
		sumCold += cold
		sumBuilder += b
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("usage_monthly iteration: %v", err)
	}
	if sumRx != 8192 {
		t.Errorf("usage_monthly SUM(net_rx_bytes) = %d, want 8192", sumRx)
	}
	if sumCold != 3 {
		t.Errorf("usage_monthly SUM(cold_boot_count) = %d, want 3", sumCold)
	}
	if sumBuilder != 120 {
		t.Errorf("usage_monthly SUM(builder_seconds) = %d, want 120", sumBuilder)
	}

	// (5) The ADD COLUMN contract is enforced: NULL inserts into
	//     the new bigint columns are rejected with SQLSTATE 23502
	//     (not_null_violation) because the column is NOT NULL
	//     DEFAULT 0 — proves the DEFAULT was applied at table-
	//     create time, not via a backfill UPDATE that could leave
	//     a window of NULLs. Same probe pattern as 00066 test.
	_, err = pool.Exec(ctx, `
		insert into usage_minutes (account_id, app_id, instance_id, minute, mb_seconds, requests, net_rx_bytes)
		values ($1, $2, $3, date_trunc('minute', now()) - interval '2 minute', 3000, 1, null)
	`, acct, appID, insID)
	if err == nil {
		t.Errorf("explicit NULL net_rx_bytes must be rejected by NOT NULL constraint")
	} else {
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) {
			t.Errorf("explicit-NULL net_rx_bytes error not a *pgconn.PgError: %v", err)
		} else if pgErr.Code != "23502" {
			t.Errorf("explicit NULL net_rx_bytes SQLSTATE = %q, want 23502 (not_null_violation); full: %v", pgErr.Code, err)
		}
	}

	// (6) usage_daily table exists with the expected columns + PK.
	//     information_schema.tables + .columns are the canonical
	//     probes (same source pg_dump uses). The PK is on
	//     (account_id, app_id, day) — load-bearing for the meterd
	//     cron's ON CONFLICT additive merge.
	var tableName string
	if err := pool.QueryRow(ctx, `
		select table_name
		  from information_schema.tables
		 where table_schema = current_schema()
		   and table_name   = 'usage_daily'
	`).Scan(&tableName); err != nil {
		t.Fatalf("usage_daily table not present after migrations apply: %v", err)
	}
	dayCols := []string{
		"account_id", "app_id", "day",
		"mb_seconds", "requests", "cpu_usec",
		"tx_bytes", "net_tx_bytes", "net_rx_bytes",
		"cold_boot_count", "builder_seconds", "rolled_up_at",
	}
	for _, col := range dayCols {
		var n string
		if err := pool.QueryRow(ctx, `
			select column_name
			  from information_schema.columns
			 where table_schema = current_schema()
			   and table_name   = 'usage_daily'
			   and column_name  = $1
		`, col).Scan(&n); err != nil {
			t.Errorf("usage_daily.%s not present: %v", col, err)
		}
	}

	// (7) Insert into usage_daily directly (the meterd cron would
	//     do this; the test pins the table accepts writes) and
	//     verify the additive-merge contract under ON CONFLICT
	//     (the cron's idempotency mechanism).
	day := time.Now().UTC().Truncate(24*time.Hour).AddDate(0, 0, -7) // pinned past day
	if _, err := pool.Exec(ctx, `
		insert into usage_daily (account_id, app_id, day, mb_seconds, requests, net_rx_bytes, cold_boot_count, builder_seconds)
		values ($1, $2, $3, 5000, 9, 4096, 1, 60)
	`, acct, appID, day); err != nil {
		t.Fatalf("insert usage_daily: %v", err)
	}
	// Second insert with ON CONFLICT additive merge — proves the
	// cron's idempotency mechanism. Hand-written here because the
	// cron code is not yet wired (PR-A A.5).
	if _, err := pool.Exec(ctx, `
		insert into usage_daily (account_id, app_id, day, mb_seconds, requests, net_rx_bytes, cold_boot_count, builder_seconds)
		values ($1, $2, $3, 1000, 1, 2048, 0, 30)
		on conflict (account_id, app_id, day) do update set
		    mb_seconds      = usage_daily.mb_seconds      + excluded.mb_seconds,
		    requests        = usage_daily.requests        + excluded.requests,
		    net_rx_bytes    = usage_daily.net_rx_bytes    + excluded.net_rx_bytes,
		    cold_boot_count = usage_daily.cold_boot_count + excluded.cold_boot_count,
		    builder_seconds = usage_daily.builder_seconds + excluded.builder_seconds,
		    rolled_up_at    = excluded.rolled_up_at
	`, acct, appID, day); err != nil {
		t.Fatalf("usage_daily ON CONFLICT additive merge: %v", err)
	}
	var dayRow struct {
		mb, req, rx, cold, b int64
	}
	if err := pool.QueryRow(ctx, `
		select mb_seconds, requests, net_rx_bytes, cold_boot_count, builder_seconds
		  from usage_daily
		 where account_id = $1 and app_id = $2 and day = $3
	`, acct, appID, day).Scan(&dayRow.mb, &dayRow.req, &dayRow.rx, &dayRow.cold, &dayRow.b); err != nil {
		t.Fatalf("read back usage_daily after merge: %v", err)
	}
	if dayRow.mb != 6000 || dayRow.req != 10 || dayRow.rx != 6144 || dayRow.cold != 1 || dayRow.b != 90 {
		t.Errorf("usage_daily additive-merge result mb=%d req=%d rx=%d cold=%d b=%d; want 6000, 10, 6144, 1, 90",
			dayRow.mb, dayRow.req, dayRow.rx, dayRow.cold, dayRow.b)
	}

	// (8) The usage_daily_account_day_idx exists. Same probe as
	//     00066 will check pg_indexes for the migration-defined
	//     index. Load-bearing for /v1/usage/daily?day=.
	var idxCount int
	if err := pool.QueryRow(ctx, `
		select count(*) from pg_indexes
		 where schemaname = current_schema()
		   and tablename  = 'usage_daily'
		   and indexname  = 'usage_daily_account_day_idx'
	`).Scan(&idxCount); err != nil {
		t.Fatalf("pg_indexes lookup: %v", err)
	}
	if idxCount != 1 {
		t.Errorf("usage_daily_account_day_idx count = %d, want 1", idxCount)
	}
}
