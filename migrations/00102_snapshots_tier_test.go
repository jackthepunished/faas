//go:build !no_pg

// Migration-apply test for 00102_snapshots_tier.sql (issue #470,
// ADR-055). Pins the tier column, the CHECK constraint, and the
// (deployment_id, tier) unique index for non-stale rows.
//
// Pins:
//
//  1. The migration set applies cleanly through 00102 (covered by
//     00101_apps_warm_snapshot_test.go too; we run again here to
//     make a missing migration file between the two obvious).
//  2. Default tier is 'init' (NOT NULL DEFAULT 'init').
//  3. Tier CHECK rejects values outside ('init','warm').
//  4. Two-tier unique index: (deployment_id='init') and
//     (deployment_id='warm') can both exist for one deployment.
//  5. Same-tier second insert on a non-stale row hits the unique
//     index (ErrConflict path that pgstore.CreateSnapshot surfaces).
//  6. Same-tier second insert on a STALE row bypasses the unique
//     index (WHERE stale=false filter) — the recovery path.
//  7. Replay safety: a second MigrateUp is a no-op. The migration
//     uses ADD COLUMN IF NOT EXISTS + CREATE UNIQUE INDEX IF NOT
//     EXISTS + DROP CONSTRAINT IF EXISTS / ADD CONSTRAINT (the
//     constraint is idempotent via the prior drop). Same shape as
//     migrations/00088_apps_warm_snapshot.sql.
//
// Slot note: see 00101_apps_warm_snapshot_test.go for the renumber
// protocol. The seed UUIDs in this file carry 089 / 189 / 289
// (account / app / deployment).
//
// Build tag matches the rest of the migration tests; set
// FAAS_SKIP_PG_TESTS=1 to skip locally (see migrations/README.md).
package migrations_test

import (
	"context"
	"testing"

	"github.com/onebox-faas/faas/pkg/db"
	"github.com/onebox-faas/faas/pkg/db/pgtest"
)

func TestMigrations_00102_SnapshotsTier(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.Open(t)

	// (1) Apply through 00102.
	if err := db.MigrateUp(ctx, pool); err != nil {
		t.Fatalf("db.MigrateUp: %v (regression: missing migration slot between 1 and 89)", err)
	}

	// Seed an account, app, and one deployment. Seed UUIDs carry the
	// slot number in the last group (`...000087` etc.) so a reader can
	// pin each row to this migration without grepping the file name.
	if _, err := pool.Exec(ctx, `
		insert into accounts (id, email, plan, created_at)
		values ('00000000-0000-0000-0000-000000000102',
		        'snapshots-tier-test@example.com', 'pro', now())
		on conflict (id) do nothing
	`); err != nil {
		t.Fatalf("seed account: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		insert into apps (id, account_id, slug, type, ram_mb, max_concurrency, idle_timeout_s, status, created_at)
		values ('00000000-0000-0000-0000-000000000202',
		        '00000000-0000-0000-0000-000000000102',
		        'snapshots-tier-test-app', 'function', 256, 1, 30, 'active', now())
		on conflict (id) do nothing
	`); err != nil {
		t.Fatalf("seed app: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		insert into deployments (id, app_id, image_digest, status, created_at)
		values ('00000000-0000-0000-0000-000000000302',
		        '00000000-0000-0000-0000-000000000202',
		        'sha256:deadbeef', 'live', now())
		on conflict (id) do nothing
	`); err != nil {
		t.Fatalf("seed deployment: %v", err)
	}

	// (2) Default tier: insert without explicit tier reads back 'init'.
	if _, err := pool.Exec(ctx, `
		insert into snapshots (deployment_id, fc_version, mem_bytes, disk_bytes, storage_key, stale)
		values ('00000000-0000-0000-0000-000000000302', 'fc-1.0', 1000, 500, 'snap/seed/mem', false)
	`); err != nil {
		t.Fatalf("seed init snapshot: %v", err)
	}
	var tier string
	if err := pool.QueryRow(ctx, `
		select tier from snapshots
		 where deployment_id = '00000000-0000-0000-0000-000000000302' and stale = false
		 order by created_at desc limit 1
	`).Scan(&tier); err != nil {
		t.Fatalf("read default tier: %v", err)
	}
	if tier != "init" {
		t.Errorf("default tier = %q, want \"init\"", tier)
	}

	// (3) Tier CHECK rejects values outside the allowed set.
	if _, err := pool.Exec(ctx, `
		insert into snapshots (deployment_id, fc_version, mem_bytes, disk_bytes, storage_key, stale, tier)
		values ('00000000-0000-0000-0000-000000000302', 'fc-1.0', 1000, 500, 'snap/seed/mem', false, 'bogus')
	`); err == nil {
		t.Errorf("tier='bogus' should be rejected by CHECK")
	}

	// (4) Two-tier coexistence: a 'warm' row on the same deployment is
	// permitted (different (deployment_id, tier) pair). This is the
	// load-bearing acceptance for the warm-tier capture flow.
	if _, err := pool.Exec(ctx, `
		insert into snapshots (deployment_id, fc_version, mem_bytes, disk_bytes, storage_key, stale, tier)
		values ('00000000-0000-0000-0000-000000000302', 'fc-1.0', 1000, 500, 'snap/seed/warm/mem', false, 'warm')
	`); err != nil {
		t.Fatalf("seed warm snapshot: %v", err)
	}
	var initCount, warmCount int
	if err := pool.QueryRow(ctx, `
		select
		    count(*) filter (where tier = 'init'),
		    count(*) filter (where tier = 'warm')
		from snapshots
		where deployment_id = '00000000-0000-0000-0000-000000000302' and stale = false
	`).Scan(&initCount, &warmCount); err != nil {
		t.Fatalf("count tiers: %v", err)
	}
	if initCount != 1 {
		t.Errorf("init rows = %d, want 1", initCount)
	}
	if warmCount != 1 {
		t.Errorf("warm rows = %d, want 1", warmCount)
	}

	// (5) Same-tier second insert on a non-stale row hits the
	// snapshots_deployment_tier_key unique index. This is the path
	// pgstore.CreateSnapshot collapses to ErrConflict on (so imaged
	// can ignore duplicate emissions).
	if _, err := pool.Exec(ctx, `
		insert into snapshots (deployment_id, fc_version, mem_bytes, disk_bytes, storage_key, stale, tier)
		values ('00000000-0000-0000-0000-000000000302', 'fc-1.0', 1000, 500, 'snap/seed/init-dup/mem', false, 'init')
	`); err == nil {
		t.Errorf("second init-tier row on same deployment should hit unique index")
	}

	// (6) Same-tier recovery path: mark the existing init row stale,
	// then insert a fresh init row. The unique index excludes stale
	// rows (WHERE stale=false), so the recovery insert succeeds.
	if _, err := pool.Exec(ctx, `
		update snapshots set stale = true
		 where deployment_id = '00000000-0000-0000-0000-000000000302' and tier = 'init'
	`); err != nil {
		t.Fatalf("stale init row: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		insert into snapshots (deployment_id, fc_version, mem_bytes, disk_bytes, storage_key, stale, tier)
		values ('00000000-0000-0000-0000-000000000302', 'fc-1.1', 1100, 550, 'snap/seed/init-recovered/mem', false, 'init')
	`); err != nil {
		t.Errorf("fresh init row after stale: %v (recovery path should NOT hit unique index)", err)
	}
	var recoveredTier string
	var stale bool
	if err := pool.QueryRow(ctx, `
		select tier, stale from snapshots
		 where deployment_id = '00000000-0000-0000-0000-000000000302'
		   and storage_key = 'snap/seed/init-recovered/mem'
	`).Scan(&recoveredTier, &stale); err != nil {
		t.Fatalf("read recovered row: %v", err)
	}
	if recoveredTier != "init" {
		t.Errorf("recovered row tier = %q, want \"init\"", recoveredTier)
	}
	if stale {
		t.Errorf("recovered row stale = true, want false")
	}

	// (7) Replay safety: a second MigrateUp is a no-op. The migration
	// uses ADD COLUMN IF NOT EXISTS + CREATE UNIQUE INDEX IF NOT
	// EXISTS + DROP CONSTRAINT IF EXISTS / ADD CONSTRAINT (the
	// constraint is idempotent via the prior drop). Same shape as
	// migrations/00088_apps_warm_snapshot.sql.
	if err := db.MigrateUp(ctx, pool); err != nil {
		t.Fatalf("replay-safety: second MigrateUp failed: %v", err)
	}
}
