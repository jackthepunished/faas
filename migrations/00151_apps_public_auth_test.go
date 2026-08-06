//go:build !no_pg

// Migration-apply test for 00151 (per-app public_auth mode +
// sealed credential blob, issue #477 / ADR-077). Pins the new
// columns:
//
//  1. The migration set applies cleanly through 00151.
//  2. public_auth_mode accepts the canonical text shape and
//     round-trips.
//  3. Default is 'open' (regression check — pre-PR rows stay
//     public-by-default, no opt-in required, no customer breakage).
//  4. CHECK rejects 'weird' / unknown modes.
//  5. public_auth_basic accepts the canonical bytea shape and
//     round-trips; nullable by default.
//  6. Replay-safe: ADD COLUMN IF NOT EXISTS + DO-block pg_constraint
//     guard makes a second MigrateUp no-op (ADR-041).
//
// Slot note: HEAD was at 00148 (overage cap gate index) at PR
// creation time. 00149 is fenced by PR #673 (issue #554 liveness
// probe) and is also touched by PR #540 (webhook deliveries).
// 00150 is PR #673's real migration. 00151 is the lowest
// uncontested slot. The migration is slot-agnostic — only the
// filename and the test function name carry the literal slot.
// If a sibling PR grabs 00151 first, renumber per
// `migrations/README.md` and update this test's filename + the
// ApplyUp range below.
//
// Build tag matches the rest of the migration tests; set
// FAAS_SKIP_PG_TESTS=1 to skip locally (see migrations/README.md).
package migrations_test

import (
	"context"
	"testing"

	"github.com/onebox-faas/faas/pkg/db"
	"github.com/onebox-faas/faas/pkg/db/pgtest"
)

func TestMigrations_00151_AppsPublicAuth(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.Open(t)

	// Seed UUIDs carry the slot number in the last group (`...000151`,
	// `...000237`) so a reader scanning the test fixtures can pin each
	// row to this migration without grepping the file name. The literal
	// slot value MUST stay in sync with the filename; renumber per
	// `migrations/README.md` if a sibling PR grabs 00151 first.

	// (1) Apply through 00151. A regression that drops a slot between
	// 1 and 150 surfaces here before the per-assertion pins.
	if err := db.MigrateUp(ctx, pool); err != nil {
		t.Fatalf("db.MigrateUp: %v (regression: missing migration slot between 1 and 151)", err)
	}

	// (2) Seed an account + app. The literal UUIDs are fixed across
	// reruns so the seed is idempotent. App fixture uses the seeded
	// pro plan so the plan-gate check (apid returns 402 on Free for
	// bearer/basic) is testable downstream without affecting this
	// migration test.
	if _, err := pool.Exec(ctx, `
		insert into accounts (id, email, plan, created_at)
		values ('00000000-0000-0000-0000-000000000151',
		        'public-auth-test@example.com', 'pro', now())
		on conflict (id) do nothing
	`); err != nil {
		t.Fatalf("seed account: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		insert into apps (id, account_id, slug, type, ram_mb, max_concurrency, idle_timeout_s, status, created_at)
		values ('00000000-0000-0000-0000-000000000237',
		        '00000000-0000-0000-0000-000000000151',
		        'public-auth-test-app', 'function', 512, 5, 300, 'active', now())
		on conflict (id) do nothing
	`); err != nil {
		t.Fatalf("seed app: %v", err)
	}

	// (3) Default round-trip: the freshly-inserted app reads
	// public_auth_mode='open' (NOT NULL DEFAULT 'open'). This is the
	// regression check that pre-PR rows stay public-by-default — the
	// load-bearing invariant for issue #477 (no existing customer
	// breakage, every existing app keeps working).
	var defaultMode string
	if err := pool.QueryRow(ctx, `
		select public_auth_mode from apps where id = '00000000-0000-0000-0000-000000000237'
	`).Scan(&defaultMode); err != nil {
		t.Fatalf("read default public_auth_mode: %v", err)
	}
	if defaultMode != "open" {
		t.Errorf("public_auth_mode default = %q, want %q (regression: pre-PR rows must stay public)", defaultMode, "open")
	}

	// (4) public_auth_basic default round-trip: nullable, defaults
	// to NULL. Regression check that the column is nullable for the
	// open/bearer modes.
	var basic []byte
	if err := pool.QueryRow(ctx, `
		select public_auth_basic from apps where id = '00000000-0000-0000-0000-000000000237'
	`).Scan(&basic); err != nil {
		t.Fatalf("read default public_auth_basic: %v", err)
	}
	if basic != nil {
		t.Errorf("public_auth_basic default = %v, want nil (regression: open mode must not store creds)", basic)
	}

	// (5) Bearer round-trip: PATCH-style UPDATE writes 'bearer' and
	// reads back 'bearer'. Mirrors the apid updateApp handler path so
	// a future regression in the write side surfaces here.
	if _, err := pool.Exec(ctx, `
		update apps set public_auth_mode = 'bearer' where id = '00000000-0000-0000-0000-000000000237'
	`); err != nil {
		t.Fatalf("update public_auth_mode=bearer: %v", err)
	}
	var bearerMode string
	if err := pool.QueryRow(ctx, `
		select public_auth_mode from apps where id = '00000000-0000-0000-0000-000000000237'
	`).Scan(&bearerMode); err != nil {
		t.Fatalf("read bearer public_auth_mode: %v", err)
	}
	if bearerMode != "bearer" {
		t.Errorf("public_auth_mode after update = %q, want %q", bearerMode, "bearer")
	}

	// (6) Basic round-trip with sealed blob: PATCH-style UPDATE writes
	// 'basic' + a non-null sealed blob. Mirrors the apid seal path so
	// a future regression in the bytea storage surfaces here.
	if _, err := pool.Exec(ctx, `
		update apps set public_auth_mode = 'basic',
		                  public_auth_basic = decode('deadbeef', 'hex')
		where id = '00000000-0000-0000-0000-000000000237'
	`); err != nil {
		t.Fatalf("update public_auth_mode=basic + blob: %v", err)
	}
	var basicMode string
	var basicBlob []byte
	if err := pool.QueryRow(ctx, `
		select public_auth_mode, public_auth_basic
		  from apps where id = '00000000-0000-0000-0000-000000000237'
	`).Scan(&basicMode, &basicBlob); err != nil {
		t.Fatalf("read basic mode+blob: %v", err)
	}
	if basicMode != "basic" {
		t.Errorf("public_auth_mode after basic update = %q, want %q", basicMode, "basic")
	}
	if len(basicBlob) != 4 || basicBlob[0] != 0xde || basicBlob[3] != 0xef {
		t.Errorf("public_auth_basic after update = %x, want deadbeef", basicBlob)
	}

	// (7) CHECK rejects unknown modes — the data-integrity backstop.
	// A typo in the apid handler or a future regression that lets
	// through an unrecognised mode surfaces here.
	if _, err := pool.Exec(ctx, `
		update apps set public_auth_mode = 'weird' where id = '00000000-0000-0000-0000-000000000237'
	`); err == nil {
		t.Errorf("CHECK did not reject public_auth_mode='weird'; want constraint violation")
	}

	// (8) Open round-trip with creds cleared: PATCH-style UPDATE flips
	// back to 'open' and clears the sealed blob. Mirrors the
	// PATCH-public-auth=open path so a future regression that
	// leaves stale creds surfaces here.
	if _, err := pool.Exec(ctx, `
		update apps set public_auth_mode = 'open',
		                  public_auth_basic = NULL
		where id = '00000000-0000-0000-0000-000000000237'
	`); err != nil {
		t.Fatalf("update public_auth_mode=open + clear blob: %v", err)
	}
	var openMode string
	var openBlob []byte
	if err := pool.QueryRow(ctx, `
		select public_auth_mode, public_auth_basic
		  from apps where id = '00000000-0000-0000-0000-000000000237'
	`).Scan(&openMode, &openBlob); err != nil {
		t.Fatalf("read open mode+blob: %v", err)
	}
	if openMode != "open" {
		t.Errorf("public_auth_mode after open update = %q, want %q", openMode, "open")
	}
	if openBlob != nil {
		t.Errorf("public_auth_basic after open update = %v, want nil (regression: open mode must not retain creds)", openBlob)
	}

	// (9) Replay safety: a second MigrateUp is a no-op (the migration
	// uses ADD COLUMN IF NOT EXISTS + DO-block pg_constraint guard).
	// ADR-041 contract.
	if err := db.MigrateUp(ctx, pool); err != nil {
		t.Fatalf("replay-safety: second MigrateUp failed: %v", err)
	}
}
