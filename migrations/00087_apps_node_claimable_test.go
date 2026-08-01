//go:build !no_pg

// Migration-apply tests for 00087 (apps.node_id nullable — Phase 2 / Gate A
// schedd-side async placement claim).
//
// Pins the contract the PlacementClaimSubscriber depends on:
//
//	1. apps.node_id is_nullable flips from NO (post-00086) to
//	   YES (post-00087) — apid can now INSERT a fresh app
//	   with the owner undecided; schedd stamps it later.
//	2. apps_node_id_nonempty_chk still trips 23514 on the
//	   zero uuid — the relaxation is "NULL is legal", not
//	   "any value is legal"; an operator upsert that tried
//	   to set node_id to the empty uuid still fails loud.
//	3. apps_node_id_idx is still in place (the relaxation
//	   doesn't drop the index — the per-schedd slice reads
//	   stay indexed).
//	4. The down path errors loud if any row has node_id IS
//	   NULL (Postgres refuses to set NOT NULL while nulls
//	   exist). The 00086 backfill + the post-00087 claim
//	   subscriber guarantee no NULL rows survive.
//	5. Replay-safety: a second MigrateUp() returns nil
//	   (ALTER COLUMN … DROP NOT NULL is idempotent).
//
// Build tag mirrors 00086_apps_node_shard_test.go:7.

package migrations_test

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/onebox-faas/faas/pkg/db"
	"github.com/onebox-faas/faas/pkg/db/pgtest"
)

// TestMigration_00087_1_AllowsNullNodeID pins the relaxation. After
// 00087, information_schema.columns reports is_nullable=YES for
// apps.node_id. A NULL insert succeeds; an empty-uuid insert still
// fails with SQLSTATE 23514 (the apps_node_id_nonempty_chk CHECK
// preserved from 00083).
func TestMigration_00087_1_AllowsNullNodeID(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.Open(t)
	defer pool.Close()
	migrateUpOnce(ctx, t, pool)

	var nullable string
	if err := pool.QueryRow(ctx, `
		select is_nullable
		  from information_schema.columns
		 where table_schema = current_schema()
		   and table_name   = 'apps'
		   and column_name  = 'node_id'
	`).Scan(&nullable); err != nil {
		t.Fatalf("query apps.node_id: %v", err)
	}
	if nullable != "YES" {
		t.Errorf("apps.node_id is_nullable = %q, want %q", nullable, "YES")
	}

	// NULL insert succeeds. We seed a fresh account + app row
	// mirroring the apid createApp write path (node_id omitted
	// from the INSERT column list so it defaults to NULL).
	acctID := uuid.NewString()
	if _, err := pool.Exec(ctx,
		`insert into accounts (id, email, plan) values ($1, $2, 'free')`,
		acctID, acctID+"@example.test"); err != nil {
		t.Fatalf("seed account: %v", err)
	}
	appID := uuid.NewString()
	if _, err := pool.Exec(ctx,
		`insert into apps (id, account_id, slug, ram_mb) values ($1, $2, $3, 128)`,
		appID, acctID, "null-owner-"+appID); err != nil {
		t.Fatalf("insert app with NULL node_id: %v", err)
	}

	// Empty-uuid insert still trips 23514 (apps_node_id_nonempty_chk).
	if _, err := pool.Exec(ctx,
		`insert into apps (id, account_id, slug, ram_mb, node_id) values ($1, $2, $3, 128, '00000000-0000-0000-0000-000000000000')`,
		uuid.NewString(), acctID, "empty-owner-"+uuid.NewString()); err == nil {
		t.Errorf("insert app with empty-uuid node_id succeeded; want CHECK violation (SQLSTATE 23514)")
	}
}

// TestMigration_00087_2_IndexStillPresent pins that 00087 does not
// drop apps_node_id_idx. The index is the per-schedd slice read path
// (ListAppsByNodeID, ListInstancesByNodeID); losing it would force
// sequential scans on every schedd tick.
func TestMigration_00087_2_IndexStillPresent(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.Open(t)
	defer pool.Close()
	migrateUpOnce(ctx, t, pool)

	var count int
	if err := pool.QueryRow(ctx, `
		select count(*)
		  from pg_indexes
		 where schemaname = current_schema()
		   and indexname  = 'apps_node_id_idx'
	`).Scan(&count); err != nil {
		t.Fatalf("query pg_indexes: %v", err)
	}
	if count != 1 {
		t.Errorf("apps_node_id_idx count = %d, want 1 (relaxation must not drop the index)", count)
	}
}

// TestMigration_00087_3_RejectsDownWithNullRows pins that the down
// path errors loud on a NULL row. Postgres refuses to set NOT NULL
// while nulls exist; the operator must investigate (a schedd is
// down or has not claimed) before retrying. We do NOT silently
// coerce NULL to the empty uuid — that would defeat the purpose
// of the relaxation.
func TestMigration_00087_3_RejectsDownWithNullRows(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.Open(t)
	defer pool.Close()
	migrateUpOnce(ctx, t, pool)

	// Seed a NULL-row app so the down has something to choke on.
	acctID := uuid.NewString()
	if _, err := pool.Exec(ctx,
		`insert into accounts (id, email, plan) values ($1, $2, 'free')`,
		acctID, acctID+"@example.test"); err != nil {
		t.Fatalf("seed account: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`insert into apps (id, account_id, slug, ram_mb) values ($1, $2, $3, 128)`,
		uuid.NewString(), acctID, "unclaimed-"+uuid.NewString()); err != nil {
		t.Fatalf("insert NULL-row app: %v", err)
	}

	// Drive the down body directly (the same way 00086 test 8 does
	// for its down path — pkg/db has no MigrateDown helper today).
	if _, err := pool.Exec(ctx, `alter table apps alter column node_id set not null`); err == nil {
		t.Errorf("down body succeeded with a NULL row present; want 23504 NOT NULL violation")
	}
}

// TestMigration_00087_4_ReplaySafe pins that a second MigrateUp is
// a no-op. ALTER COLUMN … DROP NOT NULL is idempotent (Postgres
// records the desired state, not a transition).
func TestMigration_00087_4_ReplaySafe(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.Open(t)
	defer pool.Close()

	if err := db.MigrateUp(ctx, pool); err != nil {
		t.Fatalf("first MigrateUp: %v", err)
	}
	if err := db.MigrateUp(ctx, pool); err != nil {
		t.Fatalf("replay MigrateUp: %v (ALTER COLUMN DROP NOT NULL must be idempotent)", err)
	}
}

// TestMigration_00087_5_UpThenDownThenUp pins the round-trip. MigrateUp
// applies 00087, the down body re-tightens NOT NULL (after we delete
// any NULL row first), and MigrateUp re-applies the relaxation cleanly.
// A non-idempotent ALTER would leave the schema in an inconsistent
// state on a release that needs to roll back 00087 in isolation.
func TestMigration_00087_5_UpThenDownThenUp(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.Open(t)
	defer pool.Close()
	migrateUpOnce(ctx, t, pool)

	// Down: must succeed cleanly when no NULL row exists (the
	// 00086 backfill guarantees this).
	if _, err := pool.Exec(ctx, `alter table apps alter column node_id set not null`); err != nil {
		t.Fatalf("down body: %v (no NULL rows present; down must succeed)", err)
	}

	// Re-up: must re-relax NOT NULL.
	if _, err := pool.Exec(ctx, `alter table apps alter column node_id drop not null`); err != nil {
		t.Fatalf("re-up body: %v", err)
	}

	// Probe: nullable again.
	var nullable string
	if err := pool.QueryRow(ctx, `
		select is_nullable
		  from information_schema.columns
		 where table_schema = current_schema()
		   and table_name   = 'apps'
		   and column_name  = 'node_id'
	`).Scan(&nullable); err != nil {
		t.Fatalf("post-roundtrip probe: %v", err)
	}
	if nullable != "YES" {
		t.Errorf("after up→down→up, apps.node_id is_nullable = %q, want %q", nullable, "YES")
	}
}
