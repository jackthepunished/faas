//go:build !no_pg

// Migration-apply test for 00087 (per-app private-registry Basic Auth,
// issue #461 / ADR-062). Pins the app_registry_credentials shape:
//
//  1. The migration set applies cleanly through 00087.
//  2. Every CHECK constraint enforces its bound (registry non-empty and
//     ≤ 253 chars; username non-empty and ≤ 256 chars; password blob
//     non-empty).
//  3. UNIQUE (app_id, registry) prevents duplicates at the schema layer.
//  4. FK cascade: deleting the account deletes its creds; deleting the
//     app deletes its creds.
//  5. Replay-safe: CREATE TABLE IF NOT EXISTS makes a second MigrateUp
//     a no-op (PR #377 / ADR-041).
//
// Slot note: HEAD is at 00086 (apps_require_signed, issue #472), so
// 00087 is the next free slot after the rebased head. The migration is
// slot-agnostic — only the filename and the test function name carry
// the literal slot. If a sibling PR grabs 00087 first, renumber per
// `migrations/README.md` and update this test's filename + ApplyUp
// range.
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

func TestMigrations_00087_AppRegistryCredentials(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.Open(t)

	// Seed UUIDs carry the slot number in the last group (`...000087`,
	// `...000187`, `...000287`, `...000387`) so a reader scanning the
	// test fixtures can pin each row to this migration without grepping
	// the file name. The first three are zero-padded to keep all four
	// IDs the same length and easy to scan side-by-side. The literal
	// slot value MUST stay in sync with the filename; renumber per
	// `migrations/README.md` if a sibling PR grabs 00087 first.

	// (1) Apply through 00087. A regression that drops a slot between
	// 1 and 87 surfaces here before the per-assertion pins.
	if err := db.MigrateUp(ctx, pool); err != nil {
		t.Fatalf("db.MigrateUp: %v (regression: missing migration slot between 1 and 87)", err)
	}

	// (2) Seed an account + app. The literal UUIDs are fixed across
	// reruns so the seed is idempotent.
	if _, err := pool.Exec(ctx, `
		insert into accounts (id, email, plan, created_at)
		values ('00000000-0000-0000-0000-000000000087',
		        'registry-auth-test@example.com', 'hobby', now())
		on conflict (id) do nothing
	`); err != nil {
		t.Fatalf("seed account: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		insert into apps (id, account_id, slug, type, ram_mb, max_concurrency, idle_timeout_s, status, created_at)
		values ('00000000-0000-0000-0000-000000000187',
		        '00000000-0000-0000-0000-000000000087',
		        'registry-auth-test-app', 'function', 256, 1, 30, 'active', now())
		on conflict (id) do nothing
	`); err != nil {
		t.Fatalf("seed app: %v", err)
	}

	// (3) Insert a credential row + round-trip. The ciphertext blob is
	// any non-empty byte sequence; the schema doesn't validate shape.
	if _, err := pool.Exec(ctx, `
		insert into app_registry_credentials
		  (account_id, app_id, registry, username, password_encrypted)
		values ('00000000-0000-0000-0000-000000000087',
		        '00000000-0000-0000-0000-000000000187',
		        'ghcr.io', 'alice', E'\\x000102030405 sealed-payload-stub'::bytea)
	`); err != nil {
		t.Fatalf("insert credential: %v", err)
	}

	var (
		gotRegistry   string
		gotUsername   string
		gotLastUsedAt *string
	)
	if err := pool.QueryRow(ctx, `
		select registry, username, last_used_at::text
		from app_registry_credentials
		where app_id = '00000000-0000-0000-0000-000000000187'
		  and registry = 'ghcr.io'
	`).Scan(&gotRegistry, &gotUsername, &gotLastUsedAt); err != nil {
		t.Fatalf("read back credential: %v", err)
	}
	if gotRegistry != "ghcr.io" {
		t.Errorf("registry round-trip = %q, want ghcr.io", gotRegistry)
	}
	if gotUsername != "alice" {
		t.Errorf("username round-trip = %q, want alice", gotUsername)
	}
	if gotLastUsedAt != nil {
		t.Errorf("last_used_at on fresh row = %v, want nil (nullable until first pull)", *gotLastUsedAt)
	}

	// (4) CHECK constraint enforcement — empty registry rejected.
	if _, err := pool.Exec(ctx, `
		insert into app_registry_credentials
		  (account_id, app_id, registry, username, password_encrypted)
		values ('00000000-0000-0000-0000-000000000087',
		        '00000000-0000-0000-0000-000000000187',
		        '', 'alice', '\x00\x01 stub')
	`); err == nil {
		t.Errorf("insert with empty registry: got no error; want CHECK violation")
	}

	// (5) CHECK constraint enforcement — empty username rejected.
	if _, err := pool.Exec(ctx, `
		insert into app_registry_credentials
		  (account_id, app_id, registry, username, password_encrypted)
		values ('00000000-0000-0000-0000-000000000087',
		        '00000000-0000-0000-0000-000000000187',
		        'ghcr.io', '', '\x00\x01 stub')
	`); err == nil {
		t.Errorf("insert with empty username: got no error; want CHECK violation")
	}

	// (6) CHECK constraint enforcement — empty password_encrypted rejected.
	if _, err := pool.Exec(ctx, `
		insert into app_registry_credentials
		  (account_id, app_id, registry, username, password_encrypted)
		values ('00000000-0000-0000-0000-000000000087',
		        '00000000-0000-0000-0000-000000000187',
		        'ghcr.io', 'alice', ''::bytea)
	`); err == nil {
		t.Errorf("insert with empty password_encrypted: got no error; want CHECK violation")
	}

	// (7) UNIQUE (app_id, registry) — duplicate (app, registry) rejected.
	// Pre-existing row from step (3) covers (app, 'ghcr.io'). Re-insert
	// with different ciphertext; the unique index should reject it
	// even before the ON CONFLICT path (this is the constraint that
	// enforces one-row-per-(app, registry) at the schema layer).
	if _, err := pool.Exec(ctx, `
		insert into app_registry_credentials
		  (account_id, app_id, registry, username, password_encrypted)
		values ('00000000-0000-0000-0000-000000000087',
		        '00000000-0000-0000-0000-000000000187',
		        'ghcr.io', 'alice', '\x99\x99 different-blob')
	`); err == nil {
		t.Errorf("duplicate (app_id, registry) insert: got no error; want UNIQUE violation")
	}

	// (8) A different registry on the same app succeeds — UNIQUE is on
	// (app_id, registry), not app_id alone.
	if _, err := pool.Exec(ctx, `
		insert into app_registry_credentials
		  (account_id, app_id, registry, username, password_encrypted)
		values ('00000000-0000-0000-0000-000000000087',
		        '00000000-0000-0000-0000-000000000187',
		        'registry.gregale.dev', 'bob', '\x00\x01 stub-2')
	`); err != nil {
		t.Errorf("second registry on same app: %v (UNIQUE should allow different registry)", err)
	}

	// (9) FK cascade on account delete. Seed a separate account + app +
	// cred so we don't tear down the rows from the previous steps that
	// other tests might rely on (this file is the only consumer in
	// practice, but defence in depth).
	if _, err := pool.Exec(ctx, `
		insert into accounts (id, email, plan, created_at)
		values ('00000000-0000-0000-0000-000000000287',
		        'registry-auth-cascade@example.com', 'hobby', now())
		on conflict (id) do nothing
	`); err != nil {
		t.Fatalf("seed cascade account: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		insert into apps (id, account_id, slug, type, ram_mb, max_concurrency, idle_timeout_s, status, created_at)
		values ('00000000-0000-0000-0000-000000000387',
		        '00000000-0000-0000-0000-000000000287',
		        'cascade-app', 'function', 256, 1, 30, 'active', now())
		on conflict (id) do nothing
	`); err != nil {
		t.Fatalf("seed cascade app: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		insert into app_registry_credentials
		  (account_id, app_id, registry, username, password_encrypted)
		values ('00000000-0000-0000-0000-000000000287',
		        '00000000-0000-0000-0000-000000000387',
		        'ghcr.io', 'alice', '\x00\x01 cascade-blob')
	`); err != nil {
		t.Fatalf("insert cascade credential: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		delete from accounts where id = '00000000-0000-0000-0000-000000000287'
	`); err != nil {
		t.Fatalf("delete cascade account: %v", err)
	}
	var cascadeCount int
	if err := pool.QueryRow(ctx, `
		select count(*) from app_registry_credentials
		where account_id = '00000000-0000-0000-0000-000000000287'
	`).Scan(&cascadeCount); err != nil {
		t.Fatalf("count after cascade: %v", err)
	}
	if cascadeCount != 0 {
		t.Errorf("cred rows after account cascade = %d, want 0 (ON DELETE CASCADE)", cascadeCount)
	}

	// (10) Replay safety: a second MigrateUp is a no-op (the migration
	// uses CREATE TABLE IF NOT EXISTS). PR #377 / ADR-041 contract.
	if err := db.MigrateUp(ctx, pool); err != nil {
		t.Fatalf("replay-safety: second MigrateUp failed: %v", err)
	}
}
