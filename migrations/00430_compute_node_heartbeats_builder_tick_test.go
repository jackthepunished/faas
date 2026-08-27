//go:build !no_pg

// Migration 00430 — widens compute_node_heartbeats.source CHECK
// to include 'builder_tick' for the (deferred) pkg/builderd/
// heartbeat.go writer (operator-side observability mega-PR /
// Commit 7). Pattern mirrors 00091_apps_node_claimable_test.go.
//
// What this pins:
//
//   1. The CHECK constraint compute_node_heartbeats_source_check
//      exists after the migration lands (the DROP + re-ADD shape
//      keeps the canonical constraint name from migration 00065).
//   2. The pg_constraint.consrc text contains all four enum
//      values, including the new 'builder_tick' — guards
//      against a future migration dropping one by mistake.
//   3. The constraint name matches the canonical shape so the
//      Down() path of 00430 can drop it cleanly (the canonical
//      name is documented in 00430's `alter table … drop
//      constraint if exists compute_node_heartbeats_source_check`).
//
// Replay-safety is exercised at the goose level (MigrateUp is
// idempotent on this migration's IF EXISTS guards).

package migrations_test

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/onebox-faas/faas/pkg/db/pgtest"
)

func TestMigration_00430_BuilderTickInSourceCheck(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.Open(t)
	defer pool.Close()
	migrateUpOnce(ctx, t, pool)

	consrc := mustQueryConsrc(t, pool)
	for _, want := range []string{"'heartbeat_tick'", "'deactivation'", "'reactivation'", "'builder_tick'"} {
		if !strings.Contains(consrc, want) {
			t.Errorf("compute_node_heartbeats_source_check body missing %s; full body=%s", want, consrc)
		}
	}
	for _, banned := range []string{"'foo'", "'bar'", "'baz'"} {
		if strings.Contains(consrc, banned) {
			t.Errorf("compute_node_heartbeats_source_check body contains unexpected token %s; full body=%s", banned, consrc)
		}
	}
}

func mustQueryConsrc(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	var body string
	if err := pool.QueryRow(context.Background(), `
		select pg_get_constraintdef(oid)
		  from pg_constraint
		 where conname = 'compute_node_heartbeats_source_check'
		   and conrelid = 'compute_node_heartbeats'::regclass
	`).Scan(&body); err != nil {
		t.Fatalf("query pg_constraint: %v", err)
	}
	return body
}
