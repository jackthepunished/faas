//go:build !no_pg

// TailCount PgStore tests (issue #667 / ADR-078). Mirrors the
// framework_ready PgStore test pattern (pgstore_framework_ready_test.go):
// skip-friendly via pgtest.Open when DATABASE_URL is not set, so the
// tests run on any machine with PG available and skip cleanly
// otherwise.
//
// Black-box package (`package state_test`) — same convention as the
// framework_ready pgstore test. The MemStore equivalents live in
// memstore_tail_count_test.go.

package state_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/onebox-faas/faas/pkg/db"
	"github.com/onebox-faas/faas/pkg/db/pgtest"
	"github.com/onebox-faas/faas/pkg/state"
)

// seedTailCountInstancePg inserts the minimum app + deployment +
// instance rows the BumpInstanceTailCount / DecrementInstanceTailCount
// queries need. UUIDs are fixed so a failure message can name them
// directly. Mirrors the framework_ready seed helper's structure with
// its own app+deployment pair to avoid colliding with the warm-
// snapshot test fixtures.
func seedTailCountInstancePg(t *testing.T, pool *pgxpool.Pool) (appID, deploymentID, instanceID string) {
	t.Helper()
	ctx := context.Background()
	IDs := struct{ acct, app, dep, ins string }{
		acct: "00000000-0000-0000-0000-0000000000c0",
		app:  "00000000-0000-0000-0000-0000000000c1",
		dep:  "00000000-0000-0000-0000-0000000000c2",
		ins:  "00000000-0000-0000-0000-0000000000c3",
	}
	if _, err := pool.Exec(ctx, `
		insert into accounts (id, email, plan, created_at)
		values ($1, 'tail-count-pg@example.com', 'pro', now())
		on conflict (id) do nothing
	`, IDs.acct); err != nil {
		t.Fatalf("seed account: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		insert into apps (id, account_id, slug, type, ram_mb, max_concurrency, idle_timeout_s, status, created_at)
		values ($1, $2, 'tail-count-pg-app', 'function', 256, 1, 30, 'active', now())
		on conflict (id) do nothing
	`, IDs.app, IDs.acct); err != nil {
		t.Fatalf("seed app: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		insert into deployments (id, app_id, image_digest, status, created_at)
		values ($1, $2, 'sha256:tailcountpg', 'live', now())
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
		insert into instances (id, app_id, deployment_id, node_id, state, ram_mb, started_at)
		values ($1, $2, $3, $4, 'running', 256, now())
		on conflict (id) do nothing
	`, IDs.ins, IDs.app, IDs.dep, nodeID); err != nil {
		t.Fatalf("seed instance: %v", err)
	}
	return IDs.app, IDs.dep, IDs.ins
}

// TestPg_BumpInstanceTailCount_AddsAndReturnsPostValue pins
// the SQL RETURNING clause — the function must observe the
// post-update value via the row scan, not a follow-up SELECT.
// Mirrors the MemStore equivalent.
func TestPg_BumpInstanceTailCount_AddsAndReturnsPostValue(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.OpenMigrated(t)
	if err := db.MigrateUp(ctx, pool); err != nil {
		t.Fatalf("MigrateUp: %v", err)
	}
	_, _, insID := seedTailCountInstancePg(t, pool)
	store := state.NewPgStore(pool)

	// Reset the column absolutely so the test is deterministic
	// regardless of which other tests in this package ran first
	// against the same per-test isolated schema.
	if _, err := pool.Exec(ctx, `update instances set tail_count = 0 where id = $1`, insID); err != nil {
		t.Fatalf("reset tail_count: %v", err)
	}

	post, err := store.BumpInstanceTailCount(ctx, insID, 1)
	if err != nil {
		t.Fatalf("bump +1: %v", err)
	}
	if post != 1 {
		t.Errorf("BumpInstanceTailCount(+1) post = %d, want 1", post)
	}

	post, err = store.BumpInstanceTailCount(ctx, insID, 2)
	if err != nil {
		t.Fatalf("bump +2: %v", err)
	}
	if post != 3 {
		t.Errorf("BumpInstanceTailCount(+2) post = %d, want 3", post)
	}

	// Round-trip read confirms the column was written.
	got, err := store.InstanceByID(ctx, insID)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got.TailCount != 3 {
		t.Errorf("read-back TailCount = %d, want 3", got.TailCount)
	}
}

// TestPg_DecrementInstanceTailCount_DecrementsAndFloors pins
// the canonical decrement path on the SQL side. Mirrors the
// MemStore equivalent — the floor at 0 is the load-bearing
// property (a stale receipt from a parked instance is the
// expected source of underflow pressure; the floor prevents
// negative tail_count).
func TestPg_DecrementInstanceTailCount_DecrementsAndFloors(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.OpenMigrated(t)
	if err := db.MigrateUp(ctx, pool); err != nil {
		t.Fatalf("MigrateUp: %v", err)
	}
	_, _, insID := seedTailCountInstancePg(t, pool)
	store := state.NewPgStore(pool)

	// Seed at exactly 3 — reset the column absolutely first so this
	// test is deterministic regardless of which other tests ran
	// against the same isolated schema (the schema is per-test but
	// tests in the same package share the cluster).
	if _, err := pool.Exec(ctx, `update instances set tail_count = 0 where id = $1`, insID); err != nil {
		t.Fatalf("reset tail_count: %v", err)
	}
	if _, err := store.BumpInstanceTailCount(ctx, insID, 3); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// 3 → 2 → 1 → 0.
	for want := int32(2); want >= 0; want-- {
		if err := store.DecrementInstanceTailCount(ctx, insID, 1); err != nil {
			t.Fatalf("decrement: %v", err)
		}
		got, err := store.InstanceByID(ctx, insID)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if got.TailCount != int(want) {
			t.Errorf("post-decrement TailCount = %d, want %d", got.TailCount, want)
		}
	}

	// Stray decrement at 0 — must floor, not underflow.
	if err := store.DecrementInstanceTailCount(ctx, insID, 1); err != nil {
		t.Fatalf("stray decrement: %v", err)
	}
	got, _ := store.InstanceByID(ctx, insID)
	if got.TailCount != 0 {
		t.Errorf("stray decrement underflowed: TailCount = %d, want 0", got.TailCount)
	}
}

// TestPg_BumpInstanceTailCount_NegativeDeltaFloorsAtZero
// mirrors the MemStore test — the SQL GREATEST(…, 0) guard
// applies symmetrically to BumpInstanceTailCount with a
// negative delta. Pinning both sides (positive + negative)
// guards against a future PR that splits the SQL paths.
func TestPg_BumpInstanceTailCount_NegativeDeltaFloorsAtZero(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.OpenMigrated(t)
	if err := db.MigrateUp(ctx, pool); err != nil {
		t.Fatalf("MigrateUp: %v", err)
	}
	_, _, insID := seedTailCountInstancePg(t, pool)
	store := state.NewPgStore(pool)

	// Reset to 0 absolutely — same determinism rationale as the
	// additive tests above.
	if _, err := pool.Exec(ctx, `update instances set tail_count = 0 where id = $1`, insID); err != nil {
		t.Fatalf("reset tail_count: %v", err)
	}

	post, err := store.BumpInstanceTailCount(ctx, insID, -5)
	if err != nil {
		t.Fatalf("bump -5: %v", err)
	}
	if post != 0 {
		t.Errorf("BumpInstanceTailCount(-5) on 0 = %d, want 0 (floor)", post)
	}
	got, _ := store.InstanceByID(ctx, insID)
	if got.TailCount != 0 {
		t.Errorf("read-back TailCount = %d, want 0", got.TailCount)
	}
}

// TestPg_BumpDecrement_MissingRowReturnsErrNotFound pins the
// missing-row contract on the SQL side — both methods return
// ErrNotFound for unknown ids. The pgx.ErrNoRows error is
// translated to ErrNotFound in the implementations so callers
// (vmmd's MarkInstanceTailTerminal receipt path) can branch
// on a single error value.
func TestPg_BumpDecrement_MissingRowReturnsErrNotFound(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.OpenMigrated(t)
	if err := db.MigrateUp(ctx, pool); err != nil {
		t.Fatalf("MigrateUp: %v", err)
	}
	store := state.NewPgStore(pool)
	const missing = "00000000-0000-0000-0000-deadbeefdead"

	if _, err := store.BumpInstanceTailCount(ctx, missing, 1); !errors.Is(err, state.ErrNotFound) {
		t.Errorf("BumpInstanceTailCount on missing = %v, want state.ErrNotFound", err)
	}
	if err := store.DecrementInstanceTailCount(ctx, missing, 1); !errors.Is(err, state.ErrNotFound) {
		t.Errorf("DecrementInstanceTailCount on missing = %v, want state.ErrNotFound", err)
	}
}
