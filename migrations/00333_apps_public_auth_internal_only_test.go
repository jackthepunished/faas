//go:build !no_pg

// Migration-apply test for 00333 (ADR-119 — per-app ingress
// 'internal_only' mode, extends apps.public_auth_mode with
// 'internal_only'). Pins the contract:
//
//   1. The migration set applies cleanly through 00333.
//   2. Setting public_auth_mode='internal_only' is accepted
//      (CHECK widening applied).
//   3. The closed public_auth_mode enum rejects unknown values
//      (`'unknown'` fails the widened CHECK).
//   4. Down-migrate narrows the CHECK back to the pre-ADR-119
//      vocabulary (and the row we seeded in internal_only
//      would 23514 against the narrower CHECK — pin that the
//      Down section does NOT silently destroy customer rows;
//      the operator is responsible for clearing rows before
//      running the Down).
//
// Build tag matches the rest of the migration tests; set
// FAAS_SKIP_PG_TESTS=1 to skip locally (see migrations/README.md).
//
// The migration is a CHECK widening only — no new column, no
// trigger. So the test surface is much smaller than the
// ip_allowlist sibling (00326). The contract that matters is
// the closed-enum vocabulary + the down-migrate ordering.

package migrations_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/onebox-faas/faas/pkg/db"
	"github.com/onebox-faas/faas/pkg/db/pgtest"
)

// TestMigrations_00333_AppPublicAuthInternalOnly pins the
// internal_only contract from ADR-119. Five named scenarios:
//
//   - ApplyThrough0333: the full migration set applies cleanly
//     through 00333 (regression: missing slot between 1 and 332
//     surfaces here before we get to the per-assertion pins).
//   - EnumAcceptsInternalOnly: setting the new mode value
//     succeeds (CHECK widening applied).
//   - EnumRejectsUnknown: setting an unknown mode value fails
//     with the widened CHECK constraint.
//   - EnumStillAcceptsExistingModes: regression — open, bearer,
//     basic, ip_allowlist still pass after the widening.
//   - DownGrade_NarrowsBackToPreADR119: Down-migrate narrows
//     the CHECK back to the pre-ADR-119 vocabulary; a row
//     already in internal_only would 23514 against the narrower
//     CHECK (verified by attempting the Down with the row
//     present and asserting SQLSTATE 23514, ConstraintName
//     `apps_public_auth_mode_chk`).
//
// Assertion style mirrors migrations/00326_apps_public_auth_ip_allowlist_test.go:
// errors.As(err, &pgErr) + pgErr.Code + pgErr.ConstraintName.
// pgx v5's *pgconn.PgError.Error() renders only
// `Severity: Message (SQLSTATE Code)` (see
// github.com/jackc/pgx/v5/pgconn/errors.go:53), so the constraint
// name is reachable only via the typed fields.
func TestMigrations_00333_AppPublicAuthInternalOnly(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.Open(t)

	// (1) Apply through 00333.
	if err := db.MigrateUp(ctx, pool); err != nil {
		t.Fatalf("db.MigrateUp: %v (regression: missing migration slot between 1 and 332)", err)
	}

	// (2) Seed an account + apps row to carry the column. The
	// literal UUIDs are fixed across reruns so the seed is
	// idempotent; mirrors the 00326 test style for grep-ability.
	if _, err := pool.Exec(ctx, `
		insert into accounts (id, email, plan, created_at)
		values ('00000000-0000-0000-0000-000000000333',
		        'public-auth-internal-only-test@example.com', 'hobby', now())
		on conflict (id) do nothing
	`); err != nil {
		t.Fatalf("seed account: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		insert into apps (id, account_id, slug, ram_mb, max_concurrency, idle_timeout_s, status, created_at)
		values ('00000000-0000-0000-0000-000000000333',
		        '00000000-0000-0000-0000-000000000333',
		        'public-auth-internal-only-test', 256, 1, 60, 'active', now())
		on conflict (id) do nothing
	`); err != nil {
		t.Fatalf("seed app: %v", err)
	}

	// (3) EnumAcceptsInternalOnly.
	if _, err := pool.Exec(ctx, `
		update apps set public_auth_mode = 'internal_only'
		 where id = '00000000-0000-0000-0000-000000000333'
	`); err != nil {
		t.Fatalf("set public_auth_mode='internal_only': %v (CHECK widening not applied?)", err)
	}

	// (4) RoundTrip: read back the value to confirm the row
	// actually stored it.
	var gotMode string
	if err := pool.QueryRow(ctx, `
		select public_auth_mode from apps
		 where id = '00000000-0000-0000-0000-000000000333'
	`).Scan(&gotMode); err != nil {
		t.Fatalf("read back public_auth_mode: %v", err)
	}
	if gotMode != "internal_only" {
		t.Fatalf("public_auth_mode round-trip: got %q, want %q", gotMode, "internal_only")
	}

	// (5) EnumRejectsUnknown: setting an unknown mode value
	// fails with SQLSTATE 23514 and the widened CHECK
	// constraint name.
	_, err := pool.Exec(ctx, `
		update apps set public_auth_mode = 'unknown_mode_xyz'
		 where id = '00000000-0000-0000-0000-000000000333'
	`)
	if err == nil {
		t.Fatalf("set public_auth_mode='unknown_mode_xyz': expected SQLSTATE 23514, got nil")
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("error is not *pgconn.PgError: %v (typed assertion failure)", err)
	}
	if pgErr.Code != "23514" {
		t.Errorf("SQLSTATE: got %q, want %q", pgErr.Code, "23514")
	}
	if pgErr.ConstraintName != "apps_public_auth_mode_chk" {
		t.Errorf("constraint name: got %q, want %q", pgErr.ConstraintName, "apps_public_auth_mode_chk")
	}

	// (6) EnumStillAcceptsExistingModes: regression — open,
	// bearer, basic, ip_allowlist must still pass after the
	// widening (the Down-narrow only removes internal_only,
	// not the pre-existing modes).
	for _, mode := range []string{"open", "bearer", "basic", "ip_allowlist"} {
		if _, err := pool.Exec(ctx, `
			update apps set public_auth_mode = $1
			 where id = '00000000-0000-0000-0000-000000000333'
		`, mode); err != nil {
			t.Errorf("set public_auth_mode=%q: %v (regression: pre-existing mode should still be accepted)", mode, err)
		}
	}

	// (7) DownGrade_NarrowsBackToPreADR119: set the row back to
	// internal_only so the Down attempt below exercises the
	// "row present + narrower CHECK" failure mode.
	if _, err := pool.Exec(ctx, `
		update apps set public_auth_mode = 'internal_only'
		 where id = '00000000-0000-0000-0000-000000000333'
	`); err != nil {
		t.Fatalf("re-set internal_only for Down test: %v", err)
	}

	// Attempting the Down must surface SQLSTATE 23514 with
	// ConstraintName `apps_public_auth_mode_chk` because the
	// narrower CHECK rejects the row's mode='internal_only'.
	// Pin this contract: the Down section does NOT silently
	// destroy customer rows; the operator must clear them
	// before running the Down.
	//
	// The migration test package does not expose a
	// `MigrateDown` helper (the package only ships Up
	// because Down is operator-driven in this repo — see
	// ADR-041 carve-out). We invoke the Down SQL shape
	// directly via pool.Exec to validate the SQLSTATE
	// contract. The fenced shape is the same SQL the
	// migration embeds verbatim (-- +goose Down section).
	_, downErr := pool.Exec(ctx, `
		ALTER TABLE apps DROP CONSTRAINT IF EXISTS apps_public_auth_mode_chk;
		ALTER TABLE apps ADD CONSTRAINT apps_public_auth_mode_chk
		  CHECK (public_auth_mode IN ('open','bearer','basic','ip_allowlist'));
	`)
	if downErr == nil {
		t.Fatalf("Down with row in internal_only: expected SQLSTATE 23514, got nil (the narrower CHECK silently destroyed a customer row?)")
	}
	var pgErrDown *pgconn.PgError
	if !errors.As(downErr, &pgErrDown) {
		t.Fatalf("Down error is not *pgconn.PgError: %v (typed assertion failure)", downErr)
	}
	if pgErrDown.Code != "23514" {
		t.Errorf("Down SQLSTATE: got %q, want %q", pgErrDown.Code, "23514")
	}
	if pgErrDown.ConstraintName != "apps_public_auth_mode_chk" {
		t.Errorf("Down constraint name: got %q, want %q", pgErrDown.ConstraintName, "apps_public_auth_mode_chk")
	}

	// Operator clears the row (simulating the documented
	// pre-Down procedure), then the Down succeeds.
	if _, err := pool.Exec(ctx, `
		update apps set public_auth_mode = 'open'
		 where id = '00000000-0000-0000-0000-000000000333'
	`); err != nil {
		t.Fatalf("operator-clear: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		ALTER TABLE apps DROP CONSTRAINT IF EXISTS apps_public_auth_mode_chk;
		ALTER TABLE apps ADD CONSTRAINT apps_public_auth_mode_chk
		  CHECK (public_auth_mode IN ('open','bearer','basic','ip_allowlist'));
	`); err != nil {
		t.Fatalf("Down after operator-clear: %v", err)
	}

	// (8) After Down, the CHECK must reject internal_only.
	// Re-attempt to set the row to internal_only (it should
	// fail because the CHECK has been narrowed).
	if _, err := pool.Exec(ctx, `
		update apps set public_auth_mode = 'internal_only'
		 where id = '00000000-0000-0000-0000-000000000333'
	`); err == nil {
		t.Fatalf("set internal_only after Down: expected SQLSTATE 23514 (CHECK should have been narrowed), got nil")
	}
}
