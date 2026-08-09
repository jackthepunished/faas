//go:build !no_pg

// Migration-apply test for 00122_instances_framework_ready_at.sql
// (issue #470, PR #470-FU-B). Pins the framework_ready_at column on
// the instances table that PR #470-FU-A's engine waits on, and
// PR #470-FU-B's vmmd gRPC `FrameworkReady` RPC writes to.
//
// Pins:
//
//  1. The migration set applies cleanly through 00122 (covered by
//     00109_apps_warm_snapshot_test.go and 00110_snapshots_tier_test.go
//     too; we run again here to make a missing migration file
//     between any of them obvious).
//  2. Schema-only: column exists, is nullable, default NULL.
//  3. Workflow round-trip: a NULL value can be inserted, then
//     updated to a non-NULL timestamp via the UPDATE path that
//     PR #470-FU-B's vmmd gRPC handler calls (preview-shape; the
//     real handler lands in cmd/vmmd/manager.go).
//  4. The column is nullable on every existing test row (no
//     requirement that the migration back-fill anything — that's
//     by design; legacy rows never had a framework-ready signal).
//  5. Replay safety: a second MigrateUp is a no-op. The migration
//     uses ADD COLUMN IF NOT EXISTS (idempotent at the column
//     level).
//
// Slot note: 00122 was picked on the second rebase onto main
// after PR #547 (Tier A7 edge split) claimed slots
// 00119/00120/00121. Originally the migration was 00112
// (PR-creation slot per ADR-041); the renumber chain 112 → 116
// → 117 → 118 → 119 → 120 collapsed into 120 on the first
// rebase onto main (1768ed4b), then PR #547 racing the gate
// to higher numbers forced a 120 → 122 jump on this rebase.
// Slot 121 is a partner fence at
// migrations/00121_reserve_slot.sql that bridges 120 → 122.
// The seed UUIDs in this file carry 122 (account), 232 (app),
// 332 (deployment), 432 (instance).
//
// Build tag matches the rest of the migration tests; set
// FAAS_SKIP_PG_TESTS=1 to skip locally (see migrations/README.md).
package migrations_test

import (
	"context"
	"testing"
	"time"

	"github.com/onebox-faas/faas/pkg/db"
	"github.com/onebox-faas/faas/pkg/db/pgtest"
)

func TestMigrations_00122_InstancesFrameworkReadyAt(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.Open(t)

	// (1) Apply through 00122.
	if err := db.MigrateUp(ctx, pool); err != nil {
		t.Fatalf("db.MigrateUp: %v (regression: missing migration slot between 1 and 112)", err)
	}

	// Seed an account, app, deployment, and one instance. Seed UUIDs
	// carry the slot number in the last group (`...000122` etc.) so
	// a reader can pin each row to this migration without grepping
	// the file name.
	if _, err := pool.Exec(ctx, `
		insert into accounts (id, email, plan, created_at)
		values ('00000000-0000-0000-0000-000000000122',
		        'framework-ready-test@example.com', 'pro', now())
		on conflict (id) do nothing
	`); err != nil {
		t.Fatalf("seed account: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		insert into apps (id, account_id, slug, type, ram_mb, max_concurrency, idle_timeout_s, status, created_at)
		values ('00000000-0000-0000-0000-000000000232',
		        '00000000-0000-0000-0000-000000000122',
		        'framework-ready-test-app', 'function', 256, 1, 30, 'active', now())
		on conflict (id) do nothing
	`); err != nil {
		t.Fatalf("seed app: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		insert into deployments (id, app_id, image_digest, status, created_at)
		values ('00000000-0000-0000-0000-000000000332',
		        '00000000-0000-0000-0000-000000000232',
		        'sha256:deadbeef', 'live', now())
		on conflict (id) do nothing
	`); err != nil {
		t.Fatalf("seed deployment: %v", err)
	}
	// node_id is a UUID FK referencing compute_nodes(id); the 00024
	// migration seeds a default-local row during apply. Look it up
	// rather than hand-rolling a UUID (the schema rejects text).
	var nodeID string
	if err := pool.QueryRow(ctx, `
		select id from compute_nodes where name = 'default-local' limit 1
	`).Scan(&nodeID); err != nil {
		t.Fatalf("lookup default-local compute_node id: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		insert into instances (id, app_id, deployment_id, node_id, state, ram_mb, started_at)
		values ('00000000-0000-0000-0000-000000000432',
		        '00000000-0000-0000-0000-000000000232',
		        '00000000-0000-0000-0000-000000000332',
		        $1, 'running', 256, now())
		on conflict (id) do nothing
	`, nodeID); err != nil {
		t.Fatalf("seed instance: %v", err)
	}

	// (2) Column is present and nullable with default NULL.
	var frameworkReadyAt *time.Time
	if err := pool.QueryRow(ctx, `
		select framework_ready_at from instances
		 where id = '00000000-0000-0000-0000-000000000432'
	`).Scan(&frameworkReadyAt); err != nil {
		t.Fatalf("read framework_ready_at on freshly-seeded row: %v", err)
	}
	if frameworkReadyAt != nil {
		t.Errorf("freshly-seeded instance has framework_ready_at = %v, want NULL", frameworkReadyAt)
	}

	// (3) Workflow round-trip: simulate the vmmd gRPC
	// `FrameworkReady` handler by writing a timestamp, then reading
	// it back. This is the same UPDATE statement
	// pkg/state/pgstore.go::SetInstanceFrameworkReadyAt will issue
	// (PR #470-FU-B). Confirms the column is writable from the
	// runtime path, not just from the schema.
	stamp := time.Now().UTC().Truncate(time.Microsecond)
	if _, err := pool.Exec(ctx, `
		update instances set framework_ready_at = $1
		 where id = '00000000-0000-0000-0000-000000000432'
	`, stamp); err != nil {
		t.Fatalf("update framework_ready_at: %v", err)
	}
	var roundTrip *time.Time
	if err := pool.QueryRow(ctx, `
		select framework_ready_at from instances
		 where id = '00000000-0000-0000-0000-000000000432'
	`).Scan(&roundTrip); err != nil {
		t.Fatalf("read round-trip framework_ready_at: %v", err)
	}
	if roundTrip == nil {
		t.Fatalf("round-trip read returned NULL; want %v", stamp)
	}
	if !roundTrip.Equal(stamp) {
		t.Errorf("round-trip timestamp = %v, want %v (timezone drift?)", roundTrip, stamp)
	}

	// (4) Re-assert nullable: writing NULL is accepted (the engine
	// might re-clear the column on a fresh warm-capture cycle).
	if _, err := pool.Exec(ctx, `
		update instances set framework_ready_at = NULL
		 where id = '00000000-0000-0000-0000-000000000432'
	`); err != nil {
		t.Fatalf("reset framework_ready_at to NULL: %v", err)
	}
	if err := pool.QueryRow(ctx, `
		select framework_ready_at from instances
		 where id = '00000000-0000-0000-0000-000000000432'
	`).Scan(&frameworkReadyAt); err != nil {
		t.Fatalf("read post-reset framework_ready_at: %v", err)
	}
	if frameworkReadyAt != nil {
		t.Errorf("post-reset framework_ready_at = %v, want NULL", frameworkReadyAt)
	}

	// (5) Replay safety: a second MigrateUp is a no-op. The migration
	// uses ADD COLUMN IF NOT EXISTS (idempotent at the column level).
	if err := db.MigrateUp(ctx, pool); err != nil {
		t.Fatalf("replay-safety: second MigrateUp failed: %v", err)
	}
}
