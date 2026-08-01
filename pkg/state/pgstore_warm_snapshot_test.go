//go:build !no_pg

// PgStore coverage tests for the snapshot-tier CRUD (issue #470 /
// ADR-055). Mirrors pkg/state/memstore_warm_snapshot_test.go so the
// in-memory and PG paths stay semantically equal — every test name
// has a 1:1 counterpart in the MemStore file.
//
// Build tag matches the rest of the pgstore tests; set
// FAAS_SKIP_PG_TESTS=1 to skip locally (see migrations/README.md).
package state_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/onebox-faas/faas/pkg/db"
	"github.com/onebox-faas/faas/pkg/state"
)

// seedTierAppAndDeploymentPg inserts the minimum app + deployment
// rows the snapshot queries need, plus an account so the foreign-key
// chain is satisfied. UUIDs are fixed so a failure message can name
// them directly; the test doesn't depend on the shape beyond that.
func seedTierAppAndDeploymentPg(t *testing.T, pool *pgxpool.Pool, deploymentID string) {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `
		insert into accounts (id, email, plan, created_at)
		values ('00000000-0000-0000-0000-0000000000d0',
		        'tier-pg@example.com', 'pro', now())
		on conflict (id) do nothing
	`); err != nil {
		t.Fatalf("seed account: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		insert into apps (id, account_id, slug, type, ram_mb, max_concurrency, idle_timeout_s, status, created_at)
		values ('00000000-0000-0000-0000-0000000000d1',
		        '00000000-0000-0000-0000-0000000000d0',
		        'tier-pg-app', 'function', 256, 1, 30, 'active', now())
		on conflict (id) do nothing
	`); err != nil {
		t.Fatalf("seed app: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		insert into deployments (id, app_id, image_digest, status, created_at)
		values ($1, '00000000-0000-0000-0000-0000000000d1',
		        'sha256:warm', 'live', now())
		on conflict (id) do nothing
	`, deploymentID); err != nil {
		t.Fatalf("seed deployment: %v", err)
	}
}

// TestPg_CreateSnapshot_DefaultsTierToInit confirms the SQL DEFAULT
// 'init' on snapshots.tier (migration 00102) covers legacy callers
// that don't pass a tier.
func TestPg_CreateSnapshot_DefaultsTierToInit(t *testing.T) {
	s, pool, ctx := pgStoreWithPool(t)
	if err := db.MigrateUp(ctx, pool); err != nil {
		t.Fatalf("db.MigrateUp: %v (regression: missing migration slot between 1 and 102)", err)
	}
	depID := "00000000-0000-0000-0000-0000000000e0"
	seedTierAppAndDeploymentPg(t, pool, depID)

	created, err := s.CreateSnapshot(ctx, state.Snapshot{
		DeploymentID: depID, FCVersion: "fc-1.0",
		MemBytes: 1024, DiskBytes: 512,
		StorageKey: "snap/" + depID + "/mem",
	})
	if err != nil {
		t.Fatalf("CreateSnapshot: %v", err)
	}
	if created.Tier != state.SnapshotTierInit {
		t.Errorf("default Tier = %q, want %q", created.Tier, state.SnapshotTierInit)
	}
}

// TestPg_CreateSnapshot_TwoTierCoexist pins the
// (deployment_id, tier) unique-index behaviour from migration 00102:
// init + warm can both exist for one deployment; a duplicate init
// insert hits the unique index and surfaces as ErrConflict (the
// pgstore UniqueViolation branch from R6 — now reachable again).
func TestPg_CreateSnapshot_TwoTierCoexist(t *testing.T) {
	s, pool, ctx := pgStoreWithPool(t)
	if err := db.MigrateUp(ctx, pool); err != nil {
		t.Fatalf("db.MigrateUp: %v", err)
	}
	depID := "00000000-0000-0000-0000-0000000000e1"
	seedTierAppAndDeploymentPg(t, pool, depID)

	if _, err := s.CreateSnapshot(ctx, state.Snapshot{
		DeploymentID: depID, FCVersion: "fc-1.0",
		StorageKey: "snap/" + depID + "/mem", Tier: state.SnapshotTierInit,
	}); err != nil {
		t.Fatalf("seed init: %v", err)
	}
	if _, err := s.CreateSnapshot(ctx, state.Snapshot{
		DeploymentID: depID, FCVersion: "fc-1.0",
		StorageKey: "snap/" + depID + "/warm/mem", Tier: state.SnapshotTierWarm,
	}); err != nil {
		t.Fatalf("seed warm: %v", err)
	}
	_, err := s.CreateSnapshot(ctx, state.Snapshot{
		DeploymentID: depID, FCVersion: "fc-1.0",
		StorageKey: "snap/" + depID + "/init-dup/mem", Tier: state.SnapshotTierInit,
	})
	if !errors.Is(err, state.ErrConflict) {
		t.Errorf("dup init insert err = %v, want ErrConflict", err)
	}
}

// TestPg_LatestSnapshotForTier confirms the per-tier lookup.
// LatestSnapshotForTier(warm) returns the warm row; (init) returns
// the init row; an unknown deployment returns ErrNotFound.
func TestPg_LatestSnapshotForTier(t *testing.T) {
	s, pool, ctx := pgStoreWithPool(t)
	if err := db.MigrateUp(ctx, pool); err != nil {
		t.Fatalf("db.MigrateUp: %v", err)
	}
	depID := "00000000-0000-0000-0000-0000000000e2"
	seedTierAppAndDeploymentPg(t, pool, depID)

	if _, err := s.CreateSnapshot(ctx, state.Snapshot{
		DeploymentID: depID, FCVersion: "fc-1.0",
		StorageKey: "snap/" + depID + "/mem", Tier: state.SnapshotTierInit,
	}); err != nil {
		t.Fatalf("seed init: %v", err)
	}
	if _, err := s.CreateSnapshot(ctx, state.Snapshot{
		DeploymentID: depID, FCVersion: "fc-1.0",
		StorageKey: "snap/" + depID + "/warm/mem", Tier: state.SnapshotTierWarm,
	}); err != nil {
		t.Fatalf("seed warm: %v", err)
	}

	gotWarm, err := s.LatestSnapshotForTier(ctx, depID, state.SnapshotTierWarm)
	if err != nil {
		t.Fatalf("LatestSnapshotForTier(warm): %v", err)
	}
	if gotWarm.Tier != state.SnapshotTierWarm {
		t.Errorf("warm lookup Tier = %q, want warm", gotWarm.Tier)
	}
	if gotWarm.StorageKey != "snap/"+depID+"/warm/mem" {
		t.Errorf("warm lookup key = %q, want snap/%s/warm/mem", gotWarm.StorageKey, depID)
	}

	gotInit, err := s.LatestSnapshotForTier(ctx, depID, state.SnapshotTierInit)
	if err != nil {
		t.Fatalf("LatestSnapshotForTier(init): %v", err)
	}
	if gotInit.Tier != state.SnapshotTierInit {
		t.Errorf("init lookup Tier = %q, want init", gotInit.Tier)
	}

	if _, err := s.LatestSnapshotForTier(ctx, "00000000-0000-0000-0000-0000000000e9", state.SnapshotTierWarm); !errors.Is(err, state.ErrNotFound) {
		t.Errorf("missing dep warm lookup err = %v, want ErrNotFound", err)
	}
}

// TestPg_LatestSnapshot_WarmBeatsInitOnTie confirms the
// ORDER BY (tier='warm') DESC clause: on a created_at tie, the warm
// row wins.
func TestPg_LatestSnapshot_WarmBeatsInitOnTie(t *testing.T) {
	s, pool, ctx := pgStoreWithPool(t)
	if err := db.MigrateUp(ctx, pool); err != nil {
		t.Fatalf("db.MigrateUp: %v", err)
	}
	depID := "00000000-0000-0000-0000-0000000000e3"
	seedTierAppAndDeploymentPg(t, pool, depID)

	// Insert both rows at the exact same created_at by using a
	// fixed timestamp via pool.Exec — bypasses the per-row clock
	// jitter that would otherwise randomise the tie-breaker.
	if _, err := pool.Exec(ctx, `
		insert into snapshots (deployment_id, fc_version, mem_bytes, disk_bytes, storage_key, stale, tier, created_at)
		values ($1, 'fc-1.0', 1000, 500, 'snap/`+depID+`/mem', false, 'init', '2026-08-01 00:00:00+00'),
		       ($1, 'fc-1.0', 1000, 500, 'snap/`+depID+`/warm/mem', false, 'warm', '2026-08-01 00:00:00+00')
	`, depID); err != nil {
		t.Fatalf("seed tie rows: %v", err)
	}

	got, err := s.LatestSnapshot(ctx, depID)
	if err != nil {
		t.Fatalf("LatestSnapshot: %v", err)
	}
	if got.Tier != state.SnapshotTierWarm {
		t.Errorf("LatestSnapshot Tier = %q, want warm (tie-break wins)", got.Tier)
	}
}

// TestPg_ListSnapshotsForGC_ProjectsTier confirms the GC projection
// now carries the tier so the perAppKeepCurrentPrevious policy can
// branch on it.
func TestPg_ListSnapshotsForGC_ProjectsTier(t *testing.T) {
	s, pool, ctx := pgStoreWithPool(t)
	if err := db.MigrateUp(ctx, pool); err != nil {
		t.Fatalf("db.MigrateUp: %v", err)
	}
	depID := "00000000-0000-0000-0000-0000000000e4"
	seedTierAppAndDeploymentPg(t, pool, depID)

	if _, err := s.CreateSnapshot(ctx, state.Snapshot{
		DeploymentID: depID, FCVersion: "fc-1.0",
		StorageKey: "snap/" + depID + "/mem", Tier: state.SnapshotTierInit,
	}); err != nil {
		t.Fatalf("seed init: %v", err)
	}
	if _, err := s.CreateSnapshot(ctx, state.Snapshot{
		DeploymentID: depID, FCVersion: "fc-1.0",
		StorageKey: "snap/" + depID + "/warm/mem", Tier: state.SnapshotTierWarm,
	}); err != nil {
		t.Fatalf("seed warm: %v", err)
	}

	got, err := s.ListSnapshotsForGC(ctx)
	if err != nil {
		t.Fatalf("ListSnapshotsForGC: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	tiers := map[string]bool{}
	for _, r := range got {
		if r.ID == "" {
			continue
		}
		tiers[r.Tier] = true
	}
	if !tiers[state.SnapshotTierInit] || !tiers[state.SnapshotTierWarm] {
		t.Errorf("GC projection tiers = %v, want both init + warm", tiers)
	}
}
