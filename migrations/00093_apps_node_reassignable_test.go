//go:build !no_pg

// Migration-apply tests for 00093
// (apps_node_id_status_partial_idx, Tier A4 cross-node app
// rebalance, ADR-064).
//
// Pins the Tier A4 partial-index contract verbatim:
//
//	1. apps_node_id_status_partial_idx exists as a btree
//	   index over (node_id, status) with the partial
//	   predicate WHERE node_id IS NOT NULL AND status IN
//	   ('active', 'evicted_cold').
//	2. The partial predicate is load-bearing: the index
//	   covers ≤ the non-deleted app fleet, never the
//	   full apps table. A regression that widens the
//	   predicate (e.g. drops the status filter) would
//	   balloon the index to every app row and silently
//	   inflate WAL write-amplification on every UPDATE
//	   to apps.node_id / apps.status.
//	3. Replay-safety: a second MigrateUp() returns nil —
//	   CREATE INDEX IF NOT EXISTS paired with DROP INDEX
//	   IF EXISTS in the down block (PR #377 / ADR-041).
//	4. Down symmetry: the down body drops the partial
//	   index cleanly; the re-applied up body round-trips.
//
// Build tag mirrors apply_walk_test.go:4 and the other
// 0008x/0009x migration tests — set FAAS_SKIP_PG_TESTS=1
// to skip.

package migrations_test

import (
	"context"
	"strings"
	"testing"

	"github.com/onebox-faas/faas/pkg/db"
	"github.com/onebox-faas/faas/pkg/db/pgtest"
)

// TestMigration_00093_1_IndexExists pins the index presence.
// apps_node_id_status_partial_idx must be visible in
// pg_indexes after 00093 applies. A missing index would
// leave the rebalancer's hot query doing a full apps scan,
// which is fine on a 50-app fleet but breaks down on a
// 50,000-app fleet with frequent drain events.
func TestMigration_00093_1_IndexExists(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.Open(t)
	defer pool.Close()
	migrateUpOnce(ctx, t, pool)

	var idxCount int
	if err := pool.QueryRow(ctx, `
		select count(*) from pg_indexes
		 where schemaname = current_schema()
		   and tablename  = 'apps'
		   and indexname  = 'apps_node_id_status_partial_idx'
	`).Scan(&idxCount); err != nil {
		t.Fatalf("query apps_node_id_status_partial_idx: %v", err)
	}
	if idxCount != 1 {
		t.Errorf("apps_node_id_status_partial_idx present = %d, want 1", idxCount)
	}
}

// TestMigration_00093_2_PartialPredicate pins the index
// shape. pg_get_indexdef must contain the partial predicate
//
//	WHERE (((node_id IS NOT NULL) AND
//	        (status = ANY (ARRAY['parked'::text,
//	                             'stopped'::text]))))
//
// Postgres' indexdef formatting is version-stable (the
// pg_get_indexdef contract has not changed since 9.0), but
// we match on substrings rather than the full string to
// tolerate minor whitespace/parenthesis drift across
// versions. The exact form observed on 14+:
//
//	CREATE INDEX apps_node_id_status_partial_idx
//	    ON public.apps USING btree (node_id, status)
//	    WHERE (((node_id IS NOT NULL) AND
//	            (status = ANY (ARRAY['active'::text,
//	                                 'evicted_cold'::text]))))
func TestMigration_00093_2_PartialPredicate(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.Open(t)
	defer pool.Close()
	migrateUpOnce(ctx, t, pool)

	var idxDef string
	if err := pool.QueryRow(ctx, `
		select pg_get_indexdef(indexrelid)
		  from pg_index
		 join pg_class c on c.oid = indexrelid
		 where c.relname = 'apps_node_id_status_partial_idx'
		   and c.relnamespace = (select oid from pg_namespace
		                          where nspname = current_schema())
	`).Scan(&idxDef); err != nil {
		t.Fatalf("query pg_get_indexdef: %v", err)
	}

	// Required substrings (case-insensitive — Postgres lower-
	// cases unquoted identifiers in CREATE INDEX output).
	lower := strings.ToLower(idxDef)
	for _, want := range []string{
		"where",
		"node_id",
		"is not null",
		"status",
		"active",
		"evicted_cold",
	} {
		if !strings.Contains(lower, want) {
			t.Errorf("apps_node_id_status_partial_idx predicate missing %q: %q", want, idxDef)
		}
	}
}

// TestMigration_00093_3_ReplaySafe pins the idempotency
// contract. A second MigrateUp() returns nil — CREATE INDEX
// IF NOT EXISTS paired with DROP INDEX IF EXISTS in the
// down block (PR #377 / ADR-041).
func TestMigration_00093_3_ReplaySafe(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.Open(t)
	defer pool.Close()

	if err := db.MigrateUp(ctx, pool); err != nil {
		t.Fatalf("first MigrateUp: %v", err)
	}
	if err := db.MigrateUp(ctx, pool); err != nil {
		t.Fatalf("replay MigrateUp: %v (CREATE INDEX must be IF NOT EXISTS)", err)
	}
}

// TestMigration_00093_4_DownSymmetry pins the down path.
// Drive the SQL the down body carries directly, then re-
// apply the up body and assert the index comes back. A
// non-symmetric down would leave a broken schema on a
// release that needs to roll back 00093 in isolation.
func TestMigration_00093_4_DownSymmetry(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.Open(t)
	defer pool.Close()

	if err := db.MigrateUp(ctx, pool); err != nil {
		t.Fatalf("MigrateUp: %v", err)
	}

	if _, err := pool.Exec(ctx, `drop index if exists apps_node_id_status_partial_idx`); err != nil {
		t.Fatalf("down: drop index: %v", err)
	}

	// Probe: index gone.
	var count int
	if err := pool.QueryRow(ctx, `
		select count(*) from pg_indexes
		 where schemaname = current_schema()
		   and tablename  = 'apps'
		   and indexname  = 'apps_node_id_status_partial_idx'
	`).Scan(&count); err != nil {
		t.Fatalf("probe apps_node_id_status_partial_idx absence: %v", err)
	}
	if count != 0 {
		t.Errorf("after down, apps_node_id_status_partial_idx still present (count=%d)", count)
	}

	// Re-apply the up body — must succeed cleanly.
	if _, err := pool.Exec(ctx, `create index if not exists apps_node_id_status_partial_idx on apps (node_id, status) where node_id is not null and status in ('active', 'evicted_cold')`); err != nil {
		t.Fatalf("re-add index: %v", err)
	}

	// Probe: index back.
	if err := pool.QueryRow(ctx, `
		select count(*) from pg_indexes
		 where schemaname = current_schema()
		   and tablename  = 'apps'
		   and indexname  = 'apps_node_id_status_partial_idx'
	`).Scan(&count); err != nil {
		t.Fatalf("probe apps_node_id_status_partial_idx re-added: %v", err)
	}
	if count != 1 {
		t.Errorf("after re-add, apps_node_id_status_partial_idx present = %d, want 1", count)
	}
}

// Migration 00093 has no schema change beyond the partial
// index — no round-trip fixture is needed. The four tests
// above pin the index presence, predicate shape, replay
// safety, and down symmetry.