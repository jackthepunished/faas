//go:build !no_pg

// Migration 00446 — widens operator_intents.kind CHECK to include
// 'force_restart' (P2d follow-on to PR #1099). Pattern mirrors
// 00430_compute_node_heartbeats_builder_tick_test.go exactly.
//
// What this pins:
//
//  1. The CHECK constraint operator_intents_kind_check exists
//     after the migration lands (the DROP + re-ADD shape keeps
//     the canonical auto-name from migration 00445).
//  2. The pg_get_constraintdef text contains all three enum
//     values, including the new 'force_restart' — guards
//     against a future migration dropping one by mistake.
//  3. The constraint name matches the canonical shape so the
//     Down() path of 00446 can drop it cleanly (the canonical
//     name is documented in 00446's `alter table … drop
//     constraint if exists operator_intents_kind_check`).
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

func TestMigration_00446_ForceRestartInKindCheck(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.Open(t)
	defer pool.Close()
	migrateUpOnce(ctx, t, pool)

	body := mustQueryKindCheckConsrc(t, pool)
	for _, want := range []string{"'force_park'", "'force_cold_boot'", "'force_restart'"} {
		if !strings.Contains(body, want) {
			t.Errorf("operator_intents_kind_check body missing %s; full body=%s", want, body)
		}
	}
	for _, banned := range []string{"'foo'", "'bar'", "'baz'"} {
		if strings.Contains(body, banned) {
			t.Errorf("operator_intents_kind_check body contains unexpected token %s; full body=%s", banned, body)
		}
	}
}

func mustQueryKindCheckConsrc(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	var body string
	if err := pool.QueryRow(context.Background(), `
		select pg_get_constraintdef(oid)
		  from pg_constraint
		 where conname = 'operator_intents_kind_check'
		   and conrelid = 'operator_intents'::regclass
	`).Scan(&body); err != nil {
		t.Fatalf("query pg_constraint: %v", err)
	}
	return body
}
