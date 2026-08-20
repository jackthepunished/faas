//go:build !no_pg

// Migration-apply test for 00309_consumer_keys.sql
// (ADR-120 / issue #975 item #5).
//
// Pins:
//
//  1. Migration set applies cleanly through 00309 (no goose
//     duplicate-version panic). Slot 00309 was picked as the next
//     free slot on origin/main past the open-PR reservations:
//     - PR #988 (00304 cors_presets — MERGED 2026-08-20)
//     - PR #984 (00305 deployments_annotation — open)
//     - PR #990 (00305 app_secret_value_hash — open)
//     - PR #991 (00306 + 00307 — open)
//     - PR #992 (00305 + 00306 — open)
//     - PR #997 (00303-00307 — open)
//     This PR pushes to 00309 (Strategy A — high end, robust
//     against most merge orders). Fallback renumber 00309 → 00309
//     if PR #984 merges first; re-verify with
//     scripts/ci/check_migration_slots.sh immediately before push.
//  2. The table is present with the 12 expected columns (positive
//     shape — ADR-120 §D1).
//  3. All 6 CHECK constraints landed with the expected names
//     (defense-in-depth — apid write path is the canonical gate,
//     the DB CHECKs are the floor).
//  4. The composite (app_id, prefix) hot-path index landed
//     (gateway-side lookup per ADR-120 §D1).
//  5. The UNIQUE (account_id, app_id, name) index landed
//     (the user-visible identity per ADR-120 §D1).
//  6. Closed-vocab scope rejection: insert with scope='superadmin'
//     must fail with SQLSTATE 23514 (defends the closed-set
//     contract — a typo at the apid handler is rejected by the DB
//     if it slips past the apid validator).
//  7. Replay safety: re-running db.MigrateUp is a no-op. The
//     IF NOT EXISTS / DROP TRIGGER IF EXISTS / CREATE OR REPLACE
//     FUNCTION carve-outs are the load-bearing pieces.

package migrations_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/onebox-faas/faas/pkg/db"
	"github.com/onebox-faas/faas/pkg/db/pgtest"
)

// consumerKeyExpectedColumns are the 12 columns the migration must
// add. Adding a column to 00309 without updating this list is a
// load-bearing failure mode — downstream consumers (PgStore,
// MemStore, OpenAPI, SDKs) all key off this shape.
var consumerKeyExpectedColumns = []string{
	"id",
	"account_id",
	"app_id",
	"name",
	"prefix",
	"hashed_secret",
	"scopes",
	"expires_at",
	"last_used_at",
	"revoked_at",
	"created_at",
	"updated_at",
}

// consumerKeyExpectedConstraints is the floor the migration must
// leave behind. The CHECK names are pinned because PR #5-B's apid
// write path relies on a typo here failing fast (and the
// pgstore_test cases rely on the SQLSTATE on a CHECK violation).
var consumerKeyExpectedConstraints = []string{
	"consumer_keys_name_len_chk",
	"consumer_keys_prefix_len_chk",
	"consumer_keys_hashed_secret_len_chk",
	"consumer_keys_scopes_vocab_chk",
	"consumer_keys_expires_after_created_chk",
	"consumer_keys_revoked_state_chk",
}

// consumerKeyExpectedIndexes pins the indexes PR #5-C's gateway
// middleware reads against. (app_id, prefix) is the hot-path
// composite — every inbound request with a `ck_<prefix>_<secret>`
// header narrows via this index before the hash compare.
var consumerKeyExpectedIndexes = []string{
	"consumer_keys_unique_name",
	"consumer_keys_app_prefix_idx",
	"consumer_keys_app_idx",
}

func TestMigrations_00309_ConsumerKeys(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.Open(t)

	// (1) Apply through 00309.
	if err := db.MigrateUp(ctx, pool); err != nil {
		t.Fatalf("db.MigrateUp: %v (regression: missing migration slot between 00304 cors_presets and 00309 consumer_keys)", err)
	}

	// (2) Positive shape — table present with 12 expected columns.
	for _, col := range consumerKeyExpectedColumns {
		var n int
		err := pool.QueryRow(ctx, `
			SELECT count(*)
			  FROM information_schema.columns
			 WHERE table_schema = 'public'
			   AND table_name = 'consumer_keys'
			   AND column_name = $1`, col).Scan(&n)
		if err != nil {
			t.Fatalf("query column %s: %v", col, err)
		}
		if n == 0 {
			t.Errorf("consumer_keys.%s missing (regression: column was renamed/dropped from the migration)", col)
		}
	}

	// (3) All CHECK constraints landed.
	for _, c := range consumerKeyExpectedConstraints {
		var n int
		err := pool.QueryRow(ctx, `
			SELECT count(*)
			  FROM pg_constraint c
			  JOIN pg_namespace n ON n.oid = c.connamespace
			 WHERE c.conname = $1
			   AND n.nspname = current_schema()`, c).Scan(&n)
		if err != nil {
			t.Fatalf("query constraint %s: %v", c, err)
		}
		if n == 0 {
			t.Errorf("consumer_keys constraint %s missing (migration must define all 6 named CHECKs)", c)
		}
	}

	// (4) + (5) Indexes landed.
	for _, idx := range consumerKeyExpectedIndexes {
		var n int
		err := pool.QueryRow(ctx, `
			SELECT count(*)
			  FROM pg_indexes
			 WHERE schemaname = 'public'
			   AND indexname = $1`, idx).Scan(&n)
		if err != nil {
			t.Fatalf("query index %s: %v", idx, err)
		}
		if n == 0 {
			t.Errorf("consumer_keys index %s missing (migration must create all 3 indexes — UNIQUE name + composite (app_id, prefix) + app_id)", idx)
		}
	}

	// (6) Closed-vocab scope rejection. INSERT with scope='superadmin'
	// must fail with SQLSTATE 23514 (check_violation). The CHECK
	// consumer_keys_scopes_vocab_chk is the floor — apid handlers
	// validate at the write boundary, but a hand-rolled INSERT must
	// also be rejected.
	var accountID = "00000000-0000-0000-0000-000000003308a"
	var appID = "00000000-0000-0000-0000-000000003308b"
	dummyErr := error(nil)
	if _, err := pool.Exec(ctx, `
		INSERT INTO consumer_keys (account_id, app_id, name, prefix, hashed_secret, scopes)
		VALUES (
		  $1::uuid,
		  $2::uuid,
		  'bad-scope-test',
		  'deadbeef',
		  decode('0000000000000000000000000000000000000000000000000000000000000000', 'hex'),
		  ARRAY['superadmin']::text[]
		)`, accountID, appID); err != nil {
		dummyErr = err
	}
	if dummyErr == nil {
		t.Fatal("expected closed-vocab CHECK to reject scope='superadmin' (regression: the CHECK was widened to admit non-vocab scopes)")
	}
	var pgErr *pgconn.PgError
	if !errors.As(dummyErr, &pgErr) {
		t.Fatalf("expected pgconn.PgError, got %T: %v", dummyErr, dummyErr)
	}
	if pgErr.Code != "23514" {
		t.Errorf("expected SQLSTATE 23514 check_violation, got %s (closed vocabulary contract)", pgErr.Code)
	}
	if !strings.Contains(pgErr.ConstraintName, "consumer_keys_scopes_vocab_chk") {
		t.Errorf("expected violation of consumer_keys_scopes_vocab_chk, got %q", pgErr.ConstraintName)
	}

	// (7) Replay safety: re-running db.MigrateUp is a no-op.
	// The IF NOT EXISTS / DROP TRIGGER IF EXISTS / CREATE OR REPLACE
	// FUNCTION carve-outs make the up idempotent on an already-applied
	// schema. Without them, the second MigrateUp would 42P07 on the
	// CREATE TABLE.
	if err := db.MigrateUp(ctx, pool); err != nil {
		t.Fatalf("replay db.MigrateUp: %v (migration must be replay-safe — IF NOT EXISTS + DROP TRIGGER IF EXISTS are the load-bearing carve-outs)", err)
	}
}
