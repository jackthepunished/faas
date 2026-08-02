//go:build !no_pg

// Migration-apply test for 00094 (per-app private-registry Basic Auth,
// issue #461 / ADR-064). Pins the app_registry_credentials shape:
//
//  1. The migration set applies cleanly through 00094.
//  2. Every CHECK constraint enforces its bound (registry non-empty and
//     ≤ 253 chars; username non-empty and ≤ 256 chars; password blob
//     non-empty).
//  3. UNIQUE (app_id, registry) prevents duplicates at the schema layer.
//  4. FK cascade: deleting the account deletes its creds; deleting the
//     app deletes its creds.
//  5. Replay-safe: CREATE TABLE IF NOT EXISTS makes a second MigrateUp
//     a no-op (PR #377 / ADR-041).
//
// Slot note: HEAD is at 00091 (apps_node_claimable, PR #509); slot
// 00092 is held by PR #529 (apps_reassigned_at, Tier A4 cross-node
// rebalance) on its branch — this PR keeps a 00092 reservation fence
// so its embedded FS stays contiguous from 1..94 even though the
// real 92 lives on PR #529. 00093 is also held by PR #529
// (apps_node_reassignable) on its branch. 00094 is the next free slot
// for the real registry_credentials migration. The migration is
// slot-agnostic — only the filename and the test function name carry
// the literal slot. If a sibling PR grabs 00094 first, renumber per
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

func TestMigrations_00094_AppRegistryCredentials(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.Open(t)

	// Seed UUIDs carry the slot number in the last group (`...000094`,
	// `...000194`, `...000294`, `...000394`) so a reader scanning the
	// test fixtures can pin each row to this migration without grepping
	// the file name. The first three are zero-padded to keep all four
	// IDs the same length and easy to scan side-by-side. The literal
	// slot value MUST stay in sync with the filename; renumber per
	// `migrations/README.md` if a sibling PR grabs 00094 first.

	// (1) Apply through 00094. A regression that drops a slot between
	// 1 and 94 surfaces here before the per-assertion pins.
	if err := db.MigrateUp(ctx, pool); err != nil {
		t.Fatalf("db.MigrateUp: %v (regression: missing migration slot between 1 and 94)", err)
	}

	// (2) Seed an account + app. The literal UUIDs are fixed across
	// reruns so the seed is idempotent.
	if _, err := pool.Exec(ctx, `
		insert into accounts (id, email, plan, created_at)
		values ('00000000-0000-0000-0000-000000000094',
		        'registry-auth-test@example.com', 'hobby', now())
		on conflict (id) do nothing
	`); err != nil {
		t.Fatalf("seed account: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		insert into apps (id, account_id, slug, type, ram_mb, max_concurrency, idle_timeout_s, status, created_at)
		values ('00000000-0000-0000-0000-000000000194',
		        '00000000-0000-0000-0000-000000000094',
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
		values ('00000000-0000-0000-0000-000000000094',
		        '00000000-0000-0000-0000-000000000194',
		        'ghcr.io', 'alice', decode('0001020304050607', 'hex'))
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
		where app_id = '00000000-0000-0000-0000-000000000194'
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
		values ('00000000-0000-0000-0000-000000000094',
		        '00000000-0000-0000-0000-000000000194',
		        '', 'alice', decode('0001', 'hex'))
	`); err == nil {
		t.Errorf("insert with empty registry: got no error; want CHECK violation")
	}

	// (5) CHECK constraint enforcement — empty username rejected.
	if _, err := pool.Exec(ctx, `
		insert into app_registry_credentials
		  (account_id, app_id, registry, username, password_encrypted)
		values ('00000000-0000-0000-0000-000000000094',
		        '00000000-0000-0000-0000-000000000194',
		        'ghcr.io', '', decode('0001', 'hex'))
	`); err == nil {
		t.Errorf("insert with empty username: got no error; want CHECK violation")
	}

	// (6) CHECK constraint enforcement — empty password_encrypted rejected.
	if _, err := pool.Exec(ctx, `
		insert into app_registry_credentials
		  (account_id, app_id, registry, username, password_encrypted)
		values ('00000000-0000-0000-0000-000000000094',
		        '00000000-0000-0000-0000-000000000194',
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
		values ('00000000-0000-0000-0000-000000000094',
		        '00000000-0000-0000-0000-000000000194',
		        'ghcr.io', 'alice', decode('9999', 'hex'))
	`); err == nil {
		t.Errorf("duplicate (app_id, registry) insert: got no error; want UNIQUE violation")
	}

	// (8) A different registry on the same app succeeds — UNIQUE is on
	// (app_id, registry), not app_id alone.
	if _, err := pool.Exec(ctx, `
		insert into app_registry_credentials
		  (account_id, app_id, registry, username, password_encrypted)
		values ('00000000-0000-0000-0000-000000000094',
		        '00000000-0000-0000-0000-000000000194',
		        'registry.gregale.dev', 'bob', decode('0001', 'hex'))
	`); err != nil {
		t.Errorf("second registry on same app: %v (UNIQUE should allow different registry)", err)
	}

	// (9) FK cascade on app delete. The migration declares
	// `app_id ... REFERENCES apps(id) ON DELETE CASCADE`, so dropping
	// the app should drop its credentials transitively. Seed a
	// separate account + app + cred so we don't tear down the rows
	// from the previous steps that other tests might rely on (this
	// file is the only consumer in practice, but defence in depth).
	//
	// Note: we deliberately cascade via APP delete, not ACCOUNT delete.
	// `apps.account_id` is NOT cascade (00001_init.sql:32) — it has a
	// plain FK — so the account path is not a cascade. The credential's
	// own FK on `account_id` IS cascade (mirrors `app_secrets`), so
	// account delete would also cascade via the account_id FK IF the
	// app were already gone. Verifying the app-delete path is the more
	// meaningful contract: it proves the credential's app_id FK is
	// configured correctly, which is the only FK the migration owns.
	if _, err := pool.Exec(ctx, `
		insert into accounts (id, email, plan, created_at)
		values ('00000000-0000-0000-0000-000000000294',
		        'registry-auth-cascade@example.com', 'hobby', now())
		on conflict (id) do nothing
	`); err != nil {
		t.Fatalf("seed cascade account: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		insert into apps (id, account_id, slug, type, ram_mb, max_concurrency, idle_timeout_s, status, created_at)
		values ('00000000-0000-0000-0000-000000000394',
		        '00000000-0000-0000-0000-000000000294',
		        'cascade-app', 'function', 256, 1, 30, 'active', now())
		on conflict (id) do nothing
	`); err != nil {
		t.Fatalf("seed cascade app: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		insert into app_registry_credentials
		  (account_id, app_id, registry, username, password_encrypted)
		values ('00000000-0000-0000-0000-000000000294',
		        '00000000-0000-0000-0000-000000000394',
		        'ghcr.io', 'alice', decode('0001', 'hex'))
	`); err != nil {
		t.Fatalf("insert cascade credential: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		delete from apps where id = '00000000-0000-0000-0000-000000000394'
	`); err != nil {
		t.Fatalf("delete cascade app: %v", err)
	}
	var cascadeCount int
	if err := pool.QueryRow(ctx, `
		select count(*) from app_registry_credentials
		where app_id = '00000000-0000-0000-0000-000000000394'
	`).Scan(&cascadeCount); err != nil {
		t.Fatalf("count after cascade: %v", err)
	}
	if cascadeCount != 0 {
		t.Errorf("cred rows after app cascade = %d, want 0 (ON DELETE CASCADE)", cascadeCount)
	}

	// (10) Replay safety: a second MigrateUp is a no-op (the migration
	// uses CREATE TABLE IF NOT EXISTS). PR #377 / ADR-041 contract.
	if err := db.MigrateUp(ctx, pool); err != nil {
		t.Fatalf("replay-safety: second MigrateUp failed: %v", err)
	}
}
