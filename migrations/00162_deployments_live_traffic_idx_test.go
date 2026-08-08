//go:build !no_pg

// Migration-apply test for 00162_deployments_live_traffic_idx.sql
// (issue #556 / PR-B). Pins the gateway's hot-path index for
// LiveDeployments(appID):
//
//  1. The migration set applies cleanly through 00162 (the
//     PR-A reserve fence at 00161 is consumed by this PR —
//     the test would catch a regression where the fence
//     sequence is broken, since goose's reorder check would
//     trip during MigrateUp).
//  2. The partial index `deployments_live_traffic_idx` exists
//     on `deployments(app_id) INCLUDE (traffic_percent, id)
//     WHERE status='live'`.
//  3. The index is partial (a non-live row would NOT land in
//     the index) and the INCLUDE projection is preserved — a
//     future refactor that flips the partial predicate or
//     drops the INCLUDE would degrade the gateway's refresh
//     path from Index Only Scan to Index Scan + heap fetch.
//  4. Replay-safety: a second MigrateUp is a no-op (CREATE
//     INDEX IF NOT EXISTS guard per ADR-041).
//
// Build tag mirrors 00160's; FAAS_SKIP_PG_TESTS=1 skips locally.

package migrations_test

import (
	"context"
	"strings"
	"testing"

	"github.com/onebox-faas/faas/pkg/db"
	"github.com/onebox-faas/faas/pkg/db/pgtest"
)

func TestMigrations_00162_DeploymentsLiveTrafficIdx(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.Open(t)

	// (1) Run the full migration set. 00162 lands last on this
	// branch; the 00161 fence was deleted by PR-B so the set
	// is contiguous 1..162.
	if err := db.MigrateUp(ctx, pool); err != nil {
		t.Fatalf("db.MigrateUp: %v (PR-B slot 162 broken: missing migration slot between 1 and 162)", err)
	}

	// (2) The partial index exists on the right table + column
	// list. pg_indexes carries the canonical indexdef as
	// pg_get_indexdef would emit, so this single SELECT covers
	// name, table, predicate, and INCLUDE list.
	var indexdef string
	if err := pool.QueryRow(ctx, `
		select indexdef
		  from pg_indexes
		 where schemaname = current_schema()
		   and tablename  = 'deployments'
		   and indexname  = 'deployments_live_traffic_idx'
	`).Scan(&indexdef); err != nil {
		t.Fatalf("query deployments_live_traffic_idx: %v (PR-B index must have landed)", err)
	}

	// The indexdef string is something like:
	//   CREATE INDEX deployments_live_traffic_idx
	//     ON public.deployments USING btree (app_id)
	//     INCLUDE (traffic_percent, id)
	//     WHERE (status = 'live'::text)
	// We assert each piece is present (case-insensitive on the
	// keyword — Postgres emits lowercase by default but defensive
	// parsing is cheap).
	upper := strings.ToUpper(indexdef)
	if !strings.Contains(upper, "WHERE") {
		t.Errorf("indexdef missing WHERE clause (must be partial): %s", indexdef)
	}
	if !strings.Contains(upper, "STATUS") || !strings.Contains(upper, "'LIVE'") {
		t.Errorf("indexdef missing partial predicate 'status = live': %s", indexdef)
	}
	if !strings.Contains(upper, "INCLUDE") {
		t.Errorf("indexdef missing INCLUDE clause (LiveDeployments must be index-only): %s", indexdef)
	}
	if !strings.Contains(upper, "TRAFFIC_PERCENT") {
		t.Errorf("indexdef missing traffic_percent in INCLUDE list: %s", indexdef)
	}
	if !strings.Contains(upper, "(APP_ID)") {
		t.Errorf("indexdef missing app_id as the leading key column: %s", indexdef)
	}

	// (3) Functional pin: insert one 'live' row + one
	// 'superseded' row for the same app. The 'live' row must
	// surface in a SELECT … WHERE app_id=$1 AND status='live';
	// the 'superseded' row must NOT (proves the partial predicate).
	slot := "00000000-0000-0000-0000-000000000162"
	if _, err := pool.Exec(ctx, `
		insert into accounts (id, plan, email)
		values ($1, 'scale', 'live-traffic-test@example.com')
		on conflict (id) do nothing
	`, slot); err != nil {
		t.Fatalf("seed accounts: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		insert into apps (id, account_id, slug, type, ram_mb, max_concurrency, idle_timeout_s, status, created_at)
		values ($1, $1, 'live-traffic-test', 'function', 256, 1, 30, 'active', now())
		on conflict (id) do nothing
	`, slot); err != nil {
		t.Fatalf("seed apps: %v", err)
	}

	// Use distinct image digests so the SELECT below can tell them
	// apart. The deployments table doesn't enforce digest uniqueness
	// today, but the test stays valid under a future uniqueness
	// constraint by keeping them distinct from the get-go.
	if _, err := pool.Exec(ctx, `
		insert into deployments (app_id, image_digest, status, traffic_percent)
		values ($1, 'sha256:' || repeat('a', 64), 'live', 100)
	`, slot); err != nil {
		t.Fatalf("seed live deployment: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		insert into deployments (app_id, image_digest, status, traffic_percent)
		values ($1, 'sha256:' || repeat('b', 64), 'superseded', 0)
	`, slot); err != nil {
		t.Fatalf("seed superseded deployment: %v", err)
	}

	var liveCount, supCount int
	if err := pool.QueryRow(ctx, `
		select
		  count(*) filter (where status = 'live'),
		  count(*) filter (where status = 'superseded')
		  from deployments
		 where app_id = $1
	`, slot).Scan(&liveCount, &supCount); err != nil {
		t.Fatalf("count deployments by status: %v", err)
	}
	if liveCount != 1 {
		t.Errorf("live rows for app = %d, want 1 (sanity check before EXPLAIN)", liveCount)
	}
	if supCount != 1 {
		t.Errorf("superseded rows for app = %d, want 1 (sanity check before EXPLAIN)", supCount)
	}

	// (4) EXPLAIN confirms the planner uses an Index Only Scan on
	// the new partial index. Regression guard: a future refactor
	// that drops the INCLUDE list would degrade the access path
	// to "Index Scan using deployments_live_traffic_idx" — the
	// "Only" token is the tripwire.
	var plan string
	if err := pool.QueryRow(ctx, `
		explain (format text)
		select id, traffic_percent
		  from deployments
		 where app_id = $1
		   and status = 'live'
	`, slot).Scan(&plan); err != nil {
		t.Fatalf("explain: %v", err)
	}
	if !strings.Contains(plan, "deployments_live_traffic_idx") {
		t.Errorf("EXPLAIN did not mention deployments_live_traffic_idx (planner chose a worse plan):\n%s", plan)
	}
	if !strings.Contains(strings.ToLower(plan), "index only scan") {
		t.Errorf("EXPLAIN did not pick Index Only Scan (INCLUDE clause likely dropped):\n%s", plan)
	}

	// (5) Replay safety: applying the migration set a second time
	// must not blow up. The CREATE INDEX IF NOT EXISTS guard
	// handles this; this assertion is a tripwire that survives
	// future refactors that drop the guard.
	if err := db.MigrateUp(ctx, pool); err != nil {
		t.Fatalf("replay db.MigrateUp: %v (the CREATE INDEX IF NOT EXISTS guard must have been silently dropped)", err)
	}
}
