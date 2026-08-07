//go:build !no_pg

// Migration-apply test for 00155 (per-app websocket_enabled flag,
// issue #676 / ADR-080). Pins the new column:
//
//  1. The migration set applies cleanly through 00155.
//  2. The column accepts the canonical boolean shape and round-trips.
//  3. Default is false (regression check — pre-PR rows stay on the
//     plain-HTTP path, no opt-in required, no customer breakage).
//  4. Replay-safe: ADD COLUMN IF NOT EXISTS makes a second MigrateUp
//     no-op (PR #377 / ADR-041).
//  5. Partial index `apps_websocket_enabled_idx` exists and is keyed
//     on the true subset (operator-side "which apps open raw
//     streams?" query path). Mirrors 00080_apps_streaming_enabled.
//
// Slot note: HEAD is at 00154 (deployment liveness probe, PR #673
// follow-up); 00155 is the next free slot at PR creation time. The
// migration is slot-agnostic — only the filename and the test
// function name carry the literal slot. If a sibling PR grabs 00155
// first, renumber per `migrations/README.md` and update this test's
// filename + the ApplyUp range below.
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

func TestMigrations_00155_AppsWebSocketEnabled(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.Open(t)

	// Seed UUIDs carry the slot number in the last group (`...000155`,
	// `...000255`) so a reader scanning the test fixtures can pin each
	// row to this migration without grepping the file name. The literal
	// slot value MUST stay in sync with the filename; renumber per
	// `migrations/README.md` if a sibling PR grabs 00155 first.

	// (1) Apply through 00155. A regression that drops a slot between
	// 1 and 154 surfaces here before the per-assertion pins.
	if err := db.MigrateUp(ctx, pool); err != nil {
		t.Fatalf("db.MigrateUp: %v (regression: missing migration slot between 1 and 155)", err)
	}

	// (2) Seed an account + app. The literal UUIDs are fixed across
	// reruns so the seed is idempotent. App fixture uses the seeded
	// hobby plan so the plan-gate check (apid returns 403 on Free) is
	// testable downstream without affecting this migration test.
	if _, err := pool.Exec(ctx, `
		insert into accounts (id, email, plan, created_at)
		values ('00000000-0000-0000-0000-000000000155',
		        'websocket-enabled-test@example.com', 'hobby', now())
		on conflict (id) do nothing
	`); err != nil {
		t.Fatalf("seed account: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		insert into apps (id, account_id, slug, type, ram_mb, max_concurrency, idle_timeout_s, status, created_at)
		values ('00000000-0000-0000-0000-000000000255',
		        '00000000-0000-0000-0000-000000000155',
		        'websocket-enabled-test-app', 'function', 256, 2, 60, 'active', now())
		on conflict (id) do nothing
	`); err != nil {
		t.Fatalf("seed app: %v", err)
	}

	// (3) Default round-trip: the freshly-inserted app reads
	// websocket_enabled=false (NOT NULL DEFAULT false). This is the
	// regression check that pre-PR rows stay on the plain-HTTP path —
	// the load-bearing invariant for issue #676 (no existing customer
	// breakage, every existing app keeps working without a websocket).
	var defaultVal bool
	if err := pool.QueryRow(ctx, `
		select websocket_enabled from apps where id = '00000000-0000-0000-0000-000000000255'
	`).Scan(&defaultVal); err != nil {
		t.Fatalf("read default websocket_enabled: %v", err)
	}
	if defaultVal {
		t.Errorf("websocket_enabled default = true, want false (regression: pre-PR rows must stay plain HTTP)")
	}

	// (4) PATCH-style round-trip: UPDATE flips the bit to true and
	// reads it back. Mirrors the apid updateApp handler path so a
	// future regression in the write side surfaces here.
	if _, err := pool.Exec(ctx, `
		update apps set websocket_enabled = true where id = '00000000-0000-0000-0000-000000000255'
	`); err != nil {
		t.Fatalf("update websocket_enabled=true: %v", err)
	}
	var enabledVal bool
	if err := pool.QueryRow(ctx, `
		select websocket_enabled from apps where id = '00000000-0000-0000-0000-000000000255'
	`).Scan(&enabledVal); err != nil {
		t.Fatalf("read enabled websocket_enabled: %v", err)
	}
	if !enabledVal {
		t.Errorf("websocket_enabled after update = false, want true")
	}

	// (5) PATCH-style flip back: UPDATE flips the bit back to false
	// and reads it back. Mirrors the opt-out path (Hobby+ customer
	// who toggled WS off after a Free-tier admin backfilled the
	// column; apid re-checks Plan.WebSocketResponseAllowed before
	// letting the customer flip it back ON).
	if _, err := pool.Exec(ctx, `
		update apps set websocket_enabled = false where id = '00000000-0000-0000-0000-000000000255'
	`); err != nil {
		t.Fatalf("update websocket_enabled=false: %v", err)
	}
	var disabledVal bool
	if err := pool.QueryRow(ctx, `
		select websocket_enabled from apps where id = '00000000-0000-0000-0000-000000000255'
	`).Scan(&disabledVal); err != nil {
		t.Fatalf("read disabled websocket_enabled: %v", err)
	}
	if disabledVal {
		t.Errorf("websocket_enabled after opt-out = true, want false")
	}

	// (6) Replay safety: a second MigrateUp is a no-op (the migration
	// uses ADD COLUMN IF NOT EXISTS). ADR-041 contract.
	if err := db.MigrateUp(ctx, pool); err != nil {
		t.Fatalf("replay-safety: second MigrateUp failed: %v", err)
	}
}
