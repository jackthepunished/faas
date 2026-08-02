//go:build !no_pg

// Migration-apply test for 00095 (issue #463 / ADR-066 — sidecar
// containers, hard cap 2). Pins the deployments.sidecars shape:
//
//  1. The migration set applies cleanly through 00095.
//  2. NOT NULL DEFAULT '[]'::jsonb backfills legacy rows correctly.
//  3. The CHECK constraint enforces the 2-cap at the schema layer
//     (a 3-sidecar INSERT is rejected).
//  4. JSONB round-trip preserves element shape (name, type, image).
//  5. An over-cap INSERT trips the CHECK before any FK layer sees
//     the row (the cap is the load-bearing gate).
//  6. Empty-array insert and omitted-sidecars insert (column default
//     fill) both read back as `[]`.
//  7. Replay-safe: a second MigrateUp is a no-op (PR #377 / ADR-041).
//
// Slot note: HEAD on origin/main is 094 (app_registry_credentials, PR
// #522); 00095 is the next free slot. The cross-PR slot gate
// (migrations/README.md + ADR-041 / PR #391) applies if a sibling PR
// claims 095 first — renumber per the fence pattern and update this
// test's filename + test function name + ApplyUp range + the
// cmd/e2e/harness.go::e2eMigrationTarget constant together.
//
// Build tag matches the rest of the migration tests; set
// FAAS_SKIP_PG_TESTS=1 to skip locally (see migrations/README.md).
package migrations_test

import (
	"bytes"
	"context"
	"testing"

	"github.com/onebox-faas/faas/pkg/db"
	"github.com/onebox-faas/faas/pkg/db/pgtest"
)

func TestMigrations_00095_DeploymentsSidecars(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.Open(t)

	// Seed UUIDs carry the slot number in the last group (`...000095`,
	// `...000195`, `...000295`, `...000395`, `...000495`) so a reader
	// scanning the test fixtures can pin each row to this migration
	// without grepping the file name. The literal slot value MUST
	// stay in sync with the filename; renumber per
	// migrations/README.md if a sibling PR grabs 00095 first.

	// (1) Apply through 00095. A regression that drops a slot
	// between 1 and 95 surfaces here before the per-assertion pins.
	if err := db.MigrateUp(ctx, pool); err != nil {
		t.Fatalf("db.MigrateUp: %v (regression: missing migration slot between 1 and 95)", err)
	}

	// (2) Seed an account + app. The literal UUIDs are fixed across
	// reruns so the seed is idempotent.
	if _, err := pool.Exec(ctx, `
		insert into accounts (id, email, plan, created_at)
		values ('00000000-0000-0000-0000-000000000095',
		        'sidecars-test@example.com', 'hobby', now())
		on conflict (id) do nothing
	`); err != nil {
		t.Fatalf("seed account: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		insert into apps (id, account_id, slug, type, ram_mb, max_concurrency, idle_timeout_s, status, created_at)
		values ('00000000-0000-0000-0000-000000000195',
		        '00000000-0000-0000-0000-000000000095',
		        'sidecars-test-app', 'function', 256, 1, 30, 'active', now())
		on conflict (id) do nothing
	`); err != nil {
		t.Fatalf("seed app: %v", err)
	}

	// (3) Empty-array insert passes — the default fill path. '[]'
	// is a valid 0-sidecar payload (read back as `[]`, not NULL).
	if _, err := pool.Exec(ctx, `
		insert into deployments (id, app_id, image_ref, status, sidecars, created_at)
		values ('00000000-0000-0000-0000-000000000295',
		        '00000000-0000-0000-0000-000000000195',
		        'ghcr.io/foo/bar@sha256:0000000000000000000000000000000000000000000000000000000000000000',
		        'pending', '[]'::jsonb, now())
		on conflict (id) do nothing
	`); err != nil {
		t.Fatalf("insert deployment (empty sidecars): %v", err)
	}

	// (4) 2-cap insert passes — the cap is a `<=` check, so 2 is
	// the maximum legal size. The shape validates per-sidecar
	// fields at the API layer (Sidecar.Validate); the schema only
	// enforces length + NOT NULL + jsonb-array-of-objects (PG's
	// jsonb type already validates array-of-some-value).
	if _, err := pool.Exec(ctx, `
		insert into deployments (id, app_id, image_ref, status, sidecars, created_at)
		values ('00000000-0000-0000-0000-000000000395',
		        '00000000-0000-0000-0000-000000000195',
		        'ghcr.io/foo/bar@sha256:0000000000000000000000000000000000000000000000000000000000000000',
		        'pending',
		        '[
		          {"name":"migrator",
		           "image":"ghcr.io/me/migrator@sha256:0000000000000000000000000000000000000000000000000000000000000001",
		           "type":"init",
		           "cmd":["--to","head"]},
		          {"name":"scraper",
		           "image":"ghcr.io/me/scraper@sha256:0000000000000000000000000000000000000000000000000000000000000002",
		           "type":"sidecar"}
		        ]'::jsonb,
		        now())
		on conflict (id) do nothing
	`); err != nil {
		t.Fatalf("insert deployment (2 sidecars): %v", err)
	}

	// (5) 3-cap insert rejected by CHECK. This is the load-bearing
	// test: the schema enforces the cap even when the API gate is
	// bypassed (manual SQL, future grpc handler, debug shell).
	if _, err := pool.Exec(ctx, `
		insert into deployments (id, app_id, image_ref, status, sidecars, created_at)
		values ('00000000-0000-0000-0000-000000000495',
		        '00000000-0000-0000-0000-000000000195',
		        'ghcr.io/foo/bar@sha256:0000000000000000000000000000000000000000000000000000000000000000',
		        'pending',
		        '[
		          {"name":"a","image":"x","type":"init"},
		          {"name":"b","image":"x","type":"init"},
		          {"name":"c","image":"x","type":"sidecar"}
		        ]'::jsonb,
		        now())
	`); err == nil {
		t.Errorf("3-sidecar insert: got no error; want CHECK cap violation")
	}

	// (6) JSONB round-trip preserves element shape. The schema
	// allows any well-formed JSONB; element shape is validated by
	// the API layer (Sidecar.Validate). This test pins the
	// round-trip contract so the persistence layer doesn't lose
	// fields the API passed in.
	var sidecarsJSON []byte
	if err := pool.QueryRow(ctx, `
		select sidecars from deployments
		where id = '00000000-0000-0000-0000-000000000395'
	`).Scan(&sidecarsJSON); err != nil {
		t.Fatalf("read deployment (2 sidecars): %v", err)
	}
	if !bytes.Contains(sidecarsJSON, []byte(`"name":"migrator"`)) {
		t.Errorf("sidecars round-trip lost migrator: %s", sidecarsJSON)
	}
	if !bytes.Contains(sidecarsJSON, []byte(`"type":"init"`)) {
		t.Errorf("sidecars round-trip lost type=init: %s", sidecarsJSON)
	}
	if !bytes.Contains(sidecarsJSON, []byte(`"name":"scraper"`)) {
		t.Errorf("sidecars round-trip lost scraper: %s", sidecarsJSON)
	}
	if !bytes.Contains(sidecarsJSON, []byte(`"type":"sidecar"`)) {
		t.Errorf("sidecars round-trip lost type=sidecar: %s", sidecarsJSON)
	}
	if !bytes.Contains(sidecarsJSON, []byte(`"cmd":["--to","head"]`)) {
		t.Errorf("sidecars round-trip lost cmd: %s", sidecarsJSON)
	}

	// (7) NOT NULL DEFAULT — insert without an explicit sidecars
	// column populates with `[]` via the column DEFAULT. This
	// proves the ALTER TABLE … NOT NULL DEFAULT did not break
	// legacy INSERTs that don't mention the column.
	if _, err := pool.Exec(ctx, `
		insert into deployments (id, app_id, image_ref, status, created_at)
		values ('00000000-0000-0000-0000-000000000595',
		        '00000000-0000-0000-0000-000000000195',
		        'ghcr.io/foo/bar@sha256:0000000000000000000000000000000000000000000000000000000000000000',
		        'pending', now())
	`); err != nil {
		t.Fatalf("insert deployment (no sidecars): %v", err)
	}
	if err := pool.QueryRow(ctx, `
		select sidecars from deployments
		where id = '00000000-0000-0000-0000-000000000595'
	`).Scan(&sidecarsJSON); err != nil {
		t.Fatalf("read default sidecars: %v", err)
	}
	if !bytes.Equal(bytes.TrimSpace(sidecarsJSON), []byte(`[]`)) {
		t.Errorf("default sidecars = %s; want []", sidecarsJSON)
	}

	// (8) Replay-safety: a second MigrateUp is a no-op (the
	// migration uses `ADD COLUMN IF NOT EXISTS` and a
	// pg_constraint existence check before the ADD CONSTRAINT).
	// PR #377 / ADR-041 contract.
	if err := db.MigrateUp(ctx, pool); err != nil {
		t.Fatalf("replay-safety: second MigrateUp failed: %v", err)
	}
}
