//go:build !no_pg

// Migration-apply test for 00112_instances_framework_ready_at.sql
// (issue #470, PR #470-FU-B). Pins the framework_ready_at column on
// the instances table that PR #470-FU-A's engine waits on, and
// PR #470-FU-B's vmmd gRPC `FrameworkReady` RPC writes to.
//
// Pins:
//
//  1. The migration set applies cleanly through 00112 (covered by
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
// Slot note: 00112 was picked at PR-creation time per ADR-041;
// slot 111 was already owned by open PR #540
// (00111_webhook_deliveries.sql). The seed UUIDs in this file
// carry 112 (account), 212 (app), 312 (deployment), 412 (instance).
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

func TestMigrations_00112_InstancesFrameworkReadyAt(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.Open(t)

	// (1) Apply through 00112.
	if err := db.MigrateUp(ctx, pool); err != nil {
		t.Fatalf("db.MigrateUp: %v (regression: missing migration slot between 1 and 112)", err)
	}

	// Seed an account, app, deployment, and one instance. Seed UUIDs
	// carry the slot number in the last group (`...000112` etc.) so
	// a reader can pin each row to this migration without grepping
	// the file name.
	if _, err := pool.Exec(ctx, `
		insert into accounts (id, email, plan, created_at)
		values ('00000000-0000-0000-0000-000000000112',
		        'framework-ready-test@example.com', 'pro', now())
		on conflict (id) do nothing
	`); err != nil {
		t.Fatalf("seed account: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		insert into apps (id, account_id, slug, type, ram_mb, max_concurrency, idle_timeout_s, status, created_at)
		values ('00000000-0000-0000-0000-000000000212',
		        '00000000-0000-0000-0000-000000000112',
		        'framework-ready-test-app', 'function', 256, 1, 30, 'active', now())
		on conflict (id) do nothing
	`); err != nil {
		t.Fatalf("seed app: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		insert into deployments (id, app_id, image_digest, status, created_at)
		values ('00000000-0000-0000-0000-000000000312',
		        '00000000-0000-0000-0000-000000000212',
		        'sha256:deadbeef', 'live', now())
		on conflict (id) do nothing
	`); err != nil {
		t.Fatalf("seed deployment: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		insert into instances (id, deployment_id, node_id, state, ram_mb, started_at)
		values ('00000000-0000-0000-0000-000000000412',
		        '00000000-0000-0000-0000-000000000312',
		        'local-0', 'running', 256, now())
		on conflict (id) do nothing
	`); err != nil {
		t.Fatalf("seed instance: %v", err)
	}

	// (2) Column is present and nullable with default NULL.
	var frameworkReadyAt *time.Time
	if err := pool.QueryRow(ctx, `
		select framework_ready_at from instances
		 where id = '00000000-0000-0000-0000-000000000412'
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
		 where id = '00000000-0000-0000-0000-000000000412'
	`, stamp); err != nil {
		t.Fatalf("update framework_ready_at: %v", err)
	}
	var roundTrip *time.Time
	if err := pool.QueryRow(ctx, `
		select framework_ready_at from instances
		 where id = '00000000-0000-0000-0000-000000000412'
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
		 where id = '00000000-0000-0000-0000-000000000412'
	`); err != nil {
		t.Fatalf("reset framework_ready_at to NULL: %v", err)
	}
	if err := pool.QueryRow(ctx, `
		select framework_ready_at from instances
		 where id = '00000000-0000-0000-0000-000000000412'
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
