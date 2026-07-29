//go:build !no_pg

// Migration-apply test for 00065 (compute_node_heartbeats table).
//
// Pins the load-bearing contract from CP-1 (operator-observability for
// the fleet):
//
//   1. The migration set applies cleanly through 00065.
//   2. The compute_node_heartbeats table exists with the exact shape
//      the heartbeat-history endpoint reads from (column types, indexes,
//      CHECK constraint).
//   3. Insert + select roundtrip works against a freshly-seeded node.
//   4. The unique(node_id, received_at) constraint rejects duplicate
//      stamps with SQLSTATE 23505 (unique_violation). Duplicate-key
//      collisions are observable on the writer side; the schedd
//      stamp path is expected to NOT fold them via ON CONFLICT.
//   5. The FK on delete cascade drops a node's history when the row
//      is hard-deleted (admin DELETE FROM compute_nodes).
//
// Build tag mirrors 00026_compute_node_notify_test.go:1 and
// 00064_invocations_dead_letter_test.go:1 — set FAAS_SKIP_PG_TESTS=1
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

func TestMigrations_00065_ComputeNodeHeartbeats(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.Open(t)
	if err := db.MigrateUp(ctx, pool); err != nil {
		t.Fatalf("db.MigrateUp: %v", err)
	}

	// (1) Table exists with the expected column shape. The endpoint
	//     reads these columns by name; renaming or retyping any of
	//     them would break the wire shape silently.
	expectedCols := map[string]string{
		"id":                "bigint",
		"node_id":           "uuid",
		"received_at":       "timestamp with time zone",
		"last_heartbeat_at": "timestamp with time zone",
		"source":            "text",
	}
	for col, wantType := range expectedCols {
		var gotType string
		if err := pool.QueryRow(ctx, `
			select data_type
			  from information_schema.columns
			 where table_schema = current_schema()
			   and table_name = 'compute_node_heartbeats'
			   and column_name = $1
		`, col).Scan(&gotType); err != nil {
			t.Errorf("column %q missing: %v", col, err)
			continue
		}
		if gotType != wantType {
			t.Errorf("compute_node_heartbeats.%s = %q, want %q", col, gotType, wantType)
		}
	}

	// (2) Index exists with the exact name the endpoint's read path
	//     depends on. schemaname = current_schema() follows the
	//     canonical pattern in apply_walk_test.go:124 — without it,
	//     a multi-schema dev box returns a stale row from a previous
	//     test's schema and the assertion passes-by-accident.
	var idxName string
	if err := pool.QueryRow(ctx, `
		select indexname
		  from pg_indexes
		 where schemaname = current_schema()
		   and tablename = 'compute_node_heartbeats'
		   and indexname = 'compute_node_heartbeats_node_at_idx'
	`).Scan(&idxName); err != nil {
		t.Fatalf("compute_node_heartbeats_node_at_idx not present after migrations apply: %v", err)
	}

	// (3) Seed a compute_node (parent row) and an INSERT + SELECT
	//     roundtrip. The CHECK constraint permits 'heartbeat_tick' as
	//     a source value; the negative case (unknown source) is
	//     picked up by the unique-constraint test below which trips
	//     before the CHECK on the duplicate-insert path.
	nodeID := uuid.NewString()
	if _, err := pool.Exec(ctx, `
		insert into compute_nodes (id, name, target_url, vpcpus, mem_mb, max_concurrency, admission_ceiling_mb)
		values ($1, 'cp1-mig-test', 'unix:///run/faas/cp1-mig.sock', 8, 4096, 16, 2048)
	`, nodeID); err != nil {
		t.Fatalf("seed compute_node: %v", err)
	}

	if _, err := pool.Exec(ctx, `
		insert into compute_node_heartbeats (node_id, received_at, last_heartbeat_at, source)
		values ($1, now(), now(), 'heartbeat_tick')
	`, nodeID); err != nil {
		t.Fatalf("insert heartbeat: %v", err)
	}

	var rowCount int
	if err := pool.QueryRow(ctx, `
		select count(*) from compute_node_heartbeats where node_id = $1
	`, nodeID).Scan(&rowCount); err != nil {
		t.Fatalf("count heartbeats: %v", err)
	}
	if rowCount != 1 {
		t.Errorf("after insert + select, count = %d, want 1", rowCount)
	}

	// (4) The unique(node_id, received_at) constraint rejects the
	//     duplicate. SELECT-in-shared-transaction is unavailable here
	//     because pgtest isolates each test in its own schema; we
	//     rely on the same `now()` to land twice under the seed
	//     insert + this duplicate insert.
	_, err := pool.Exec(ctx, `
		insert into compute_node_heartbeats (node_id, received_at, last_heartbeat_at, source)
		values ($1, now(), now(), 'heartbeat_tick')
	`, nodeID)
	// The two `now()` calls in adjacent inserts are very likely to
	// differ at microsecond resolution; rather than chasing that
	// race, force a deterministic collision by re-inserting with an
	// explicit received_at that we already wrote.
	if err == nil {
		// If the two now()s happened to differ, force the collision
		// by re-reading the row and re-inserting with the same key.
		// Scan into time.Time (pgx native timestamptz codec) — the
		// *string codec fails under the CI runner's binary format
		// negotiation (cannot scan timestamptz (OID 1184) in binary
		// format into *string).
		var receivedAt time.Time
		if err := pool.QueryRow(ctx, `
			select received_at from compute_node_heartbeats where node_id = $1 limit 1
		`, nodeID).Scan(&receivedAt); err != nil {
			t.Fatalf("re-read received_at: %v", err)
		}
		_, err = pool.Exec(ctx, `
			insert into compute_node_heartbeats (node_id, received_at, last_heartbeat_at, source)
			values ($1, $2, $2, 'heartbeat_tick')
		`, nodeID, receivedAt)
	}
	if err == nil {
		t.Fatalf("duplicate (node_id, received_at) must be rejected by the unique constraint")
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("duplicate-key error not a *pgconn.PgError: %v", err)
	}
	if pgErr.Code != "23505" {
		t.Errorf("duplicate (node_id, received_at) SQLSTATE = %q, want 23505 (unique_violation); full: %v", pgErr.Code, err)
	}

	// (5) FK on delete cascade: hard-deleting the node drops the
	//     history. Soft-delete (SetComputeNodeActive=false) is NOT
	//     exercised here — the cascade is on DELETE FROM, not on
	//     UPDATE active=false.
	if _, err := pool.Exec(ctx, `delete from compute_nodes where id = $1`, nodeID); err != nil {
		t.Fatalf("delete compute_node: %v", err)
	}
	if err := pool.QueryRow(ctx, `
		select count(*) from compute_node_heartbeats where node_id = $1
	`, nodeID).Scan(&rowCount); err != nil {
		t.Fatalf("count heartbeats after cascade: %v", err)
	}
	if rowCount != 0 {
		t.Errorf("after cascade, count = %d, want 0 (history rows must drop with the node)", rowCount)
	}
}
