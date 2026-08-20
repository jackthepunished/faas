//go:build !no_pg

// Migration-apply test for 00308 (ADR-118 — per-app ingress IP
// allowlist, extends apps.public_auth_mode with 'ip_allowlist').
// Pins the contract:
//
//   1. The migration set applies cleanly through 00308.
//   2. Mixed v4 + v6 CIDRs round-trip (UPDATE / read-back).
//   3. The closed public_auth_mode enum rejects unknown values
//      (`'unknown'` fails the widened CHECK).
//   4. `0.0.0.0/0` is rejected (SQLSTATE 23514, constraint
//      `apps_public_auth_ip_allowlist_cidr`).
//   5. `::/0` is rejected with the same shape.
//   6. Setting public_auth_mode='ip_allowlist' is accepted (CHECK
//      widening is in the same transaction as the trigger).
//
// Build tag matches the rest of the migration tests; set
// FAAS_SKIP_PG_TESTS=1 to skip locally (see migrations/README.md).

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

// TestMigrations_00308_AppPublicAuthIPAllowlist pins the
// ingress-IP-allowlist contract from ADR-118. Six named scenarios:
//
//   - ApplyThrough0308: the full migration set applies cleanly
//     through 00308 (regression: missing slot between 1 and 308
//     surfaces here before we get to the per-assertion pins).
//   - RoundTripMixed: an UPDATE with v4 + v6 in one UPDATE
//     reads both back.
//   - EnumAcceptsIPAllowlist: setting the new mode value
//     succeeds (CHECK widening applied).
//   - EnumRejectsUnknown: setting an unknown mode value fails
//     with the widened CHECK constraint.
//   - RejectsSlashZeroV4: `0.0.0.0/0` fails with SQLSTATE 23514,
//     ConstraintName `apps_public_auth_ip_allowlist_cidr`.
//   - RejectsSlashZeroV6: `::/0` fails with the same shape.
//
// Assertion style mirrors migrations/00033_app_egress_allowlist_v6_test.go:
// errors.As(err, &pgErr) + pgErr.Code + pgErr.ConstraintName.
// pgx v5's *pgconn.PgError.Error() renders only
// `Severity: Message (SQLSTATE Code)` (see
// github.com/jackc/pgx/v5/pgconn/errors.go:53), so the constraint
// name is reachable only via the typed fields.
func TestMigrations_00308_AppPublicAuthIPAllowlist(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.Open(t)

	// (1) Apply through 00308.
	if err := db.MigrateUp(ctx, pool); err != nil {
		t.Fatalf("db.MigrateUp: %v (regression: missing migration slot between 1 and 308)", err)
	}

	// (2) Seed an account + apps row to carry the column. The
	// literal UUIDs are fixed across reruns so the seed is
	// idempotent; mirrors the 00033 test style for grep-ability.
	if _, err := pool.Exec(ctx, `
		insert into accounts (id, email, plan, created_at)
		values ('00000000-0000-0000-0000-000000000308',
		        'public-auth-ip-allowlist-test@example.com', 'pro', now())
		on conflict (id) do nothing
	`); err != nil {
		t.Fatalf("seed account: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		insert into apps (id, account_id, slug, ram_mb, max_concurrency, idle_timeout_s, status, created_at)
		values ('00000000-0000-0000-0000-000000000308',
		        '00000000-0000-0000-0000-000000000308',
		        'public-auth-ip-allowlist-test', 512, 1, 30, 'active', now())
		on conflict (id) do nothing
	`); err != nil {
		t.Fatalf("seed app: %v", err)
	}

	// (3) RoundTripMixed — v4 + v6 in one UPDATE reads both back.
	// Single column carries both families; the gate is what walks
	// them in the request hot path.
	if _, err := pool.Exec(ctx, `
		update apps
		   set public_auth_ip_allowlist = array['10.0.0.0/8'::cidr, '2001:db8::/32'::cidr]
		 where id = '00000000-0000-0000-0000-000000000308'
	`); err != nil {
		t.Fatalf("update mixed public_auth_ip_allowlist: %v", err)
	}
	var asText string
	if err := pool.QueryRow(ctx, `
		select public_auth_ip_allowlist::text
		  from apps
		 where id = '00000000-0000-0000-0000-000000000308'
	`).Scan(&asText); err != nil {
		t.Fatalf("read mixed public_auth_ip_allowlist: %v", err)
	}
	if !strings.Contains(asText, "10.0.0.0/8") {
		t.Errorf("mixed round-trip missing 10.0.0.0/8: %q", asText)
	}
	if !strings.Contains(asText, "2001:db8::/32") {
		t.Errorf("mixed round-trip missing 2001:db8::/32: %q", asText)
	}

	// (4) EnumAcceptsIPAllowlist — the CHECK widening applied.
	// This is the load-bearing assertion: without the DROP+ADD
	// CHECK in the same transaction as the trigger, the new
	// enum value would be rejected.
	if _, err := pool.Exec(ctx, `
		update apps
		   set public_auth_mode = 'ip_allowlist'
		 where id = '00000000-0000-0000-0000-000000000308'
	`); err != nil {
		t.Fatalf("set public_auth_mode='ip_allowlist': %v (CHECK widening did not apply)", err)
	}
	var mode string
	if err := pool.QueryRow(ctx, `
		select public_auth_mode
		  from apps
		 where id = '00000000-0000-0000-0000-000000000308'
	`).Scan(&mode); err != nil {
		t.Fatalf("read public_auth_mode: %v", err)
	}
	if mode != "ip_allowlist" {
		t.Errorf("public_auth_mode = %q, want %q", mode, "ip_allowlist")
	}

	// (5) EnumRejectsUnknown — the CHECK widening did not widen
	// past the closed vocabulary. Set mode back to 'open' first
	// so the failed UPDATE doesn't leave the row in a state that
	// affects later assertions.
	if _, err := pool.Exec(ctx, `
		update apps
		   set public_auth_mode = 'open'
		 where id = '00000000-0000-0000-0000-000000000308'
	`); err != nil {
		t.Fatalf("reset public_auth_mode to 'open': %v", err)
	}
	_, err := pool.Exec(ctx, `
		update apps
		   set public_auth_mode = 'unknown-mode'
		 where id = '00000000-0000-0000-0000-000000000308'
	`)
	if err == nil {
		t.Fatalf("UPDATE with public_auth_mode='unknown-mode' unexpectedly succeeded; CHECK widening was too permissive")
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("unknown mode error not a *pgconn.PgError: %v", err)
	}
	// Postgres reports a CHECK violation as 23514; the
	// constraint name may be the explicit name OR fall back to
	// apps_public_auth_mode_chk (whichever Postgres surfaces).
	if pgErr.Code != "23514" {
		t.Errorf("unknown mode SQLSTATE = %q, want %q; full: %v", pgErr.Code, "23514", err)
	}
	if pgErr.ConstraintName != "apps_public_auth_mode_chk" {
		t.Errorf("unknown mode constraint name = %q, want %q; full: %v",
			pgErr.ConstraintName, "apps_public_auth_mode_chk", err)
	}

	// (6) RejectsSlashZeroV4 — the non-/0 contract fires. The
	// trigger raises SQLSTATE 23514 (check_violation) with
	// constraint name `apps_public_auth_ip_allowlist_cidr` via
	// `using constraint =`. Belt and suspenders: assert on the
	// structured fields AND the message text.
	_, err = pool.Exec(ctx, `
		update apps
		   set public_auth_ip_allowlist = array['0.0.0.0/0'::cidr]
		 where id = '00000000-0000-0000-0000-000000000308'
	`)
	if err == nil {
		t.Fatalf("UPDATE with 0.0.0.0/0 unexpectedly succeeded; apps_public_auth_ip_allowlist_cidr TRIGGER did not fire")
	}
	pgErr = nil
	if !errors.As(err, &pgErr) {
		t.Fatalf("/0 v4 update error not a *pgconn.PgError: %v", err)
	}
	if pgErr.Code != "23514" {
		t.Errorf("/0 v4 SQLSTATE = %q, want %q (check_violation); full: %v", pgErr.Code, "23514", err)
	}
	if pgErr.ConstraintName != "apps_public_auth_ip_allowlist_cidr" {
		t.Errorf("/0 v4 constraint name = %q, want %q; full: %v",
			pgErr.ConstraintName, "apps_public_auth_ip_allowlist_cidr", err)
	}
	if !strings.Contains(pgErr.Message, "/0") && !strings.Contains(pgErr.Message, "masklen") {
		t.Errorf("/0 v4 message = %q, want substring %q or %q", pgErr.Message, "/0", "masklen")
	}

	// (7) RejectsSlashZeroV6 — same shape for v6.
	_, err = pool.Exec(ctx, `
		update apps
		   set public_auth_ip_allowlist = array['::/0'::cidr]
		 where id = '00000000-0000-0000-0000-000000000308'
	`)
	if err == nil {
		t.Fatalf("UPDATE with ::/0 unexpectedly succeeded; apps_public_auth_ip_allowlist_cidr TRIGGER did not fire")
	}
	pgErr = nil
	if !errors.As(err, &pgErr) {
		t.Fatalf("/0 v6 update error not a *pgconn.PgError: %v", err)
	}
	if pgErr.Code != "23514" {
		t.Errorf("/0 v6 SQLSTATE = %q, want %q (check_violation); full: %v", pgErr.Code, "23514", err)
	}
	if pgErr.ConstraintName != "apps_public_auth_ip_allowlist_cidr" {
		t.Errorf("/0 v6 constraint name = %q, want %q; full: %v",
			pgErr.ConstraintName, "apps_public_auth_ip_allowlist_cidr", err)
	}
	if !strings.Contains(pgErr.Message, "/0") && !strings.Contains(pgErr.Message, "masklen") {
		t.Errorf("/0 v6 message = %q, want substring %q or %q", pgErr.Message, "/0", "masklen")
	}
}
