//go:build !no_pg

// Migration-apply test for 00084_apps_require_signed.sql (issue #472,
// ADR-058). Pins the new column + replay-safety contract.
//
// Pins:
//
//  1. The migration set applies cleanly through 00084.
//  2. The column accepts the canonical boolean shape and round-trips.
//  3. Default is false (regression check — pre-PR rows stay on the
//     open-deploy path; require_signed is operator opt-in).
//  4. Replay-safe: ADD COLUMN IF NOT EXISTS makes a second MigrateUp
//     no-op (PR #377 / ADR-041).
//
// Slot note: HEAD on origin/main was at 00082 (apps_scaling_policy) when
// this PR was opened. Slot 83 was claimed in parallel by the
// `phase 2 / gate a — per-node schedd` PR (#509); the slot-collision
// CI gate caught the race (run 30703774418) and we renumbered to 84
// before the second merge could panic goose with "duplicate version
// 83". The migration is slot-agnostic — only the filename, the test
// function name, the seed UUIDs, and the e2eMigrationTarget carry
// the literal slot. Bump pkg/e2etest/harness.go::e2eMigrationTarget
// to match the filename when renumbering.
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

func TestMigrations_00084_AppsRequireSigned(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.Open(t)

	// Seed UUIDs carry the slot number in the last group (`...000084`,
	// `...000184`) so a reader scanning the test fixtures can pin each
	// row to this migration without grepping the file name. The literal
	// slot value MUST stay in sync with the filename; renumber per
	// `migrations/README.md` if a sibling PR grabs 00084 first.

	// (1) Apply through 00084. A regression that drops a slot between
	// 1 and 84 surfaces here before the per-assertion pins.
	if err := db.MigrateUp(ctx, pool); err != nil {
		t.Fatalf("db.MigrateUp: %v (regression: missing migration slot between 1 and 84)", err)
	}

	// (2) Seed an account + app. The literal UUIDs are fixed across
	// reruns so the seed is idempotent.
	if _, err := pool.Exec(ctx, `
		insert into accounts (id, email, plan, created_at)
		values ('00000000-0000-0000-0000-000000000084',
		        'require-signed-test@example.com', 'hobby', now())
		on conflict (id) do nothing
	`); err != nil {
		t.Fatalf("seed account: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		insert into apps (id, account_id, slug, type, ram_mb, max_concurrency, idle_timeout_s, status, created_at)
		values ('00000000-0000-0000-0000-000000000184',
		        '00000000-0000-0000-0000-000000000084',
		        'require-signed-test-app', 'function', 256, 1, 30, 'active', now())
		on conflict (id) do nothing
	`); err != nil {
		t.Fatalf("seed app: %v", err)
	}

	// (3) Default round-trip: the freshly-inserted app reads
	// require_signed=false (NOT NULL DEFAULT false). This is the
	// regression check that pre-PR rows stay on the open-deploy path.
	var defaultVal bool
	if err := pool.QueryRow(ctx, `
		select require_signed from apps where id = '00000000-0000-0000-0000-000000000184'
	`).Scan(&defaultVal); err != nil {
		t.Fatalf("read default require_signed: %v", err)
	}
	if defaultVal {
		t.Errorf("require_signed default = true, want false (regression: pre-PR rows must stay open-deploy)")
	}

	// (4) Opt-in round-trip: PATCH-style UPDATE writes true and reads
	// back true. Mirrors the apid updateApp handler path so a future
	// regression in the write side surfaces here.
	if _, err := pool.Exec(ctx, `
		update apps set require_signed = true where id = '00000000-0000-0000-0000-000000000184'
	`); err != nil {
		t.Fatalf("update require_signed: %v", err)
	}
	var optedIn bool
	if err := pool.QueryRow(ctx, `
		select require_signed from apps where id = '00000000-0000-0000-0000-000000000184'
	`).Scan(&optedIn); err != nil {
		t.Fatalf("read opted-in require_signed: %v", err)
	}
	if !optedIn {
		t.Errorf("require_signed after update = false, want true")
	}

	// (5) Replay safety: a second MigrateUp is a no-op (the migration
	// uses ADD COLUMN IF NOT EXISTS). PR #377 / ADR-041 contract.
	if err := db.MigrateUp(ctx, pool); err != nil {
		t.Fatalf("replay-safety: second MigrateUp failed: %v", err)
	}

	// (6) Trusted-signer name-shape CHECK: lowercase DNS-1123-label
	// shape; a Capitalised label fails the INSERT.
	if _, err := pool.Exec(ctx, `
		insert into app_trusted_signers
		    (account_id, app_id, signer_name, cosign_public_key, added_by_account_id)
		values ('00000000-0000-0000-0000-000000000084',
		        '00000000-0000-0000-0000-000000000184',
		        'Capitalised-Label', decode(repeat('aa', 64), 'hex'),
		        '00000000-0000-0000-0000-000000000084')
	`); err == nil {
		t.Errorf("trusted-signer name-shape: Capitalised-Label should be rejected by CHECK")
	}

	// (7) Trusted-signer PEM-shape CHECK: bytes < 64 or > 1024 must
	// fail the INSERT.
	if _, err := pool.Exec(ctx, `
		insert into app_trusted_signers
		    (account_id, app_id, signer_name, cosign_public_key, added_by_account_id)
		values ('00000000-0000-0000-0000-000000000084',
		        '00000000-0000-0000-0000-000000000184',
		        'short-blob', repeat('\xAA', 32)::bytea,
		        '00000000-0000-0000-0000-000000000084')
	`); err == nil {
		t.Errorf("trusted-signer pem-shape: 32-byte blob should be rejected by CHECK")
	}
	if _, err := pool.Exec(ctx, `
		insert into app_trusted_signers
		    (account_id, app_id, signer_name, cosign_public_key, added_by_account_id)
		values ('00000000-0000-0000-0000-000000000084',
		        '00000000-0000-0000-0000-000000000184',
		        'oversized-blob', repeat('\xAA', 2048)::bytea,
		        '00000000-0000-0000-0000-000000000084')
	`); err == nil {
		t.Errorf("trusted-signer pem-shape: 2048-byte blob should be rejected by CHECK")
	}

	// (8) Happy-path trusted-signer round-trip + replay safety.
	if _, err := pool.Exec(ctx, `
		insert into app_trusted_signers
		    (account_id, app_id, signer_name, cosign_public_key, added_by_account_id)
		values ('00000000-0000-0000-0000-000000000084',
		        '00000000-0000-0000-0000-000000000184',
		        'ci-bot', decode(repeat('aa', 64), 'hex'),
		        '00000000-0000-0000-0000-000000000084')
		on conflict (app_id, signer_name) do nothing
	`); err != nil {
		t.Fatalf("seed trusted signer: %v", err)
	}
	var n int
	if err := pool.QueryRow(ctx, `
		select count(*) from app_trusted_signers
		where app_id = '00000000-0000-0000-0000-000000000184'
	`).Scan(&n); err != nil {
		t.Fatalf("count trusted signers: %v", err)
	}
	if n != 1 {
		t.Errorf("trusted signer count = %d, want 1", n)
	}

	// (9) Cascade-on-account-delete: drop the seeded account, the
	// trusted-signer row should follow (account_id FK ON DELETE
	// CASCADE). The apps.account_id FK would otherwise block the
	// delete, so the test removes the app first, mirroring the
	// production DeleteApp cleanup order.
	if _, err := pool.Exec(ctx, `
		delete from apps where id = '00000000-0000-0000-0000-000000000184'
	`); err != nil {
		t.Fatalf("delete app: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		delete from accounts where id = '00000000-0000-0000-0000-000000000084'
	`); err != nil {
		t.Fatalf("delete account: %v", err)
	}
	if err := pool.QueryRow(ctx, `
		select count(*) from app_trusted_signers
		where app_id = '00000000-0000-0000-0000-000000000184'
	`).Scan(&n); err != nil {
		t.Fatalf("count after cascade: %v", err)
	}
	if n != 0 {
		t.Errorf("trusted signer count after account cascade = %d, want 0", n)
	}
}
