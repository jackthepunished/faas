//go:build !no_pg

// PgStore coverage tests for the framework_ready_at column CRUD
// (issue #470 / PR #470-FU-B). Mirrors
// pkg/state/memstore_framework_ready_test.go so the in-memory and
// PG paths stay semantically equal — every test name has a 1:1
// counterpart in the MemStore file.
//
// Build tag matches the rest of the pgstore tests; set
// FAAS_SKIP_PG_TESTS=1 to skip locally (see migrations/README.md).
package state_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/onebox-faas/faas/pkg/db"
	"github.com/onebox-faas/faas/pkg/db/pgtest"
	"github.com/onebox-faas/faas/pkg/state"
)

// seedFrameworkReadyInstancePg inserts the minimum app + deployment
// + instance rows the framework_ready_at queries need. UUIDs are
// fixed so a failure message can name them directly. Mirrors the
// memstore seed helper's structure with its own app+deployment pair
// to avoid colliding with the warm-snapshot test fixtures.
func seedFrameworkReadyInstancePg(t *testing.T, pool *pgxpool.Pool) (appID, deploymentID, instanceID string) {
	t.Helper()
	ctx := context.Background()
	IDs := struct{ acct, app, dep, ins string }{
		acct: "00000000-0000-0000-0000-0000000000f0",
		app:  "00000000-0000-0000-0000-0000000000f1",
		dep:  "00000000-0000-0000-0000-0000000000f2",
		ins:  "00000000-0000-0000-0000-0000000000f3",
	}
	if _, err := pool.Exec(ctx, `
		insert into accounts (id, email, plan, created_at)
		values ($1, 'framework-ready-pg@example.com', 'pro', now())
		on conflict (id) do nothing
	`, IDs.acct); err != nil {
		t.Fatalf("seed account: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		insert into apps (id, account_id, slug, type, ram_mb, max_concurrency, idle_timeout_s, status, created_at)
		values ($1, $2, 'framework-ready-pg-app', 'function', 256, 1, 30, 'active', now())
		on conflict (id) do nothing
	`, IDs.app, IDs.acct); err != nil {
		t.Fatalf("seed app: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		insert into deployments (id, app_id, image_digest, status, created_at)
		values ($1, $2, 'sha256:frameworkready', 'live', now())
		on conflict (id) do nothing
	`, IDs.dep, IDs.app); err != nil {
		t.Fatalf("seed deployment: %v", err)
	}
	// node_id is a UUID FK referencing compute_nodes(id). The 00024
	// migration seeds a default-local row; look it up rather than
	// hand-rolling a UUID (the schema rejects text). Mirrors the
	// pattern in migrations/00066_usage_minutes_egress_test.go.
	var nodeID string
	if err := pool.QueryRow(ctx, `
		select id from compute_nodes where name = 'default-local' limit 1
	`).Scan(&nodeID); err != nil {
		t.Fatalf("lookup default-local compute_node id: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		insert into instances (id, deployment_id, node_id, state, ram_mb, started_at)
		values ($1, $2, $3, 'running', 256, now())
		on conflict (id) do nothing
	`, IDs.ins, IDs.dep, nodeID); err != nil {
		t.Fatalf("seed instance: %v", err)
	}
	return IDs.app, IDs.dep, IDs.ins
}

// TestPg_SetInstanceFrameworkReadyAt_StampsAndClears confirms the
// SetInstanceFrameworkReadyAt + ClearInstanceFrameworkReadyAt pair
// round-trip on Postgres. Fresh row has framework_ready_at = NULL;
// set stamps a non-NULL timestamp; clear resets to NULL. The
// MemStore equivalent is in memstore_framework_ready_test.go.
func TestPg_SetInstanceFrameworkReadyAt_StampsAndClears(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.Open(t)
	if err := db.MigrateUp(ctx, pool); err != nil {
		t.Fatalf("MigrateUp: %v", err)
	}
	_, _, insID := seedFrameworkReadyInstancePg(t, pool)
	store := state.NewPgStore(pool)

	// Fresh row has no framework-ready stamp.
	before, err := store.InstanceByID(ctx, insID)
	if err != nil {
		t.Fatalf("InstanceByID on fresh row: %v", err)
	}
	if before.FrameworkReadyAt != nil {
		t.Errorf("fresh instance FrameworkReadyAt = %v, want nil", before.FrameworkReadyAt)
	}

	// Set stamps the timestamp.
	stamp := time.Now().UTC().Truncate(time.Microsecond)
	if err := store.SetInstanceFrameworkReadyAt(ctx, insID, stamp); err != nil {
		t.Fatalf("SetInstanceFrameworkReadyAt: %v", err)
	}
	after, err := store.InstanceByID(ctx, insID)
	if err != nil {
		t.Fatalf("InstanceByID after set: %v", err)
	}
	if after.FrameworkReadyAt == nil {
		t.Fatalf("SetInstanceFrameworkReadyAt did not stamp; got nil")
	}
	if !after.FrameworkReadyAt.Equal(stamp) {
		t.Errorf("FrameworkReadyAt = %v, want %v", after.FrameworkReadyAt, stamp)
	}

	// Clear resets to NULL.
	if err := store.ClearInstanceFrameworkReadyAt(ctx, insID); err != nil {
		t.Fatalf("ClearInstanceFrameworkReadyAt: %v", err)
	}
	cleared, err := store.InstanceByID(ctx, insID)
	if err != nil {
		t.Fatalf("InstanceByID after clear: %v", err)
	}
	if cleared.FrameworkReadyAt != nil {
		t.Errorf("post-clear FrameworkReadyAt = %v, want nil", cleared.FrameworkReadyAt)
	}
}

// TestPg_SetInstanceFrameworkReadyAt_MissingRowReturnsErrNotFound
// confirms the missing-row contract on Postgres. The function
// returns ErrNotFound (matching the convention of
// UpdateInstanceState) so callers can distinguish "instance gone"
// from transient DB errors.
func TestPg_SetInstanceFrameworkReadyAt_MissingRowReturnsErrNotFound(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.Open(t)
	if err := db.MigrateUp(ctx, pool); err != nil {
		t.Fatalf("MigrateUp: %v", err)
	}
	store := state.NewPgStore(pool)

	err := store.SetInstanceFrameworkReadyAt(ctx, "00000000-0000-0000-0000-deadbeefdead", time.Now())
	if !errors.Is(err, state.ErrNotFound) {
		t.Errorf("SetInstanceFrameworkReadyAt on missing row = %v, want ErrNotFound", err)
	}
	err = store.ClearInstanceFrameworkReadyAt(ctx, "00000000-0000-0000-0000-deadbeefdead")
	if !errors.Is(err, state.ErrNotFound) {
		t.Errorf("ClearInstanceFrameworkReadyAt on missing row = %v, want ErrNotFound", err)
	}
}
