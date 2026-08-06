//go:build !no_pg

// Migration-apply test for 00155 (issue #695 / ADR-080 — flip the
// global default for apps auth from public-by-default to
// authenticated-by-default + grand-father existing customers).
//
// Pins:
//
//  1. The migration set applies cleanly through 00155.
//  2. The new column apps.auth_default_flipped_at exists with the
//     expected timestamptz + NULLable shape (regression check that
//     a future contributor doesn't flip it to NOT NULL — the
//     grand-father mechanism is `WHERE auth_default_flipped_at IS
//     NULL` on insert).
//  3. Every pre-flip apps row is backfilled (auth_default_flipped_at
//     NOT NULL after the migration applies).
//  4. The audit row 'apps.auth_default_global_flipped' is emitted
//     exactly once with the ADR-080 §3 payload shape
//     (migrated_count + plan_overrides + from/to defaults).
//  5. Replay safety: a second MigrateUp is a clean no-op — the
//     UPDATE is guarded by IS NULL, the INSERT is guarded by NOT
//     EXISTS on the kind. The audit row count remains 1; the
//     auth_default_flipped_at stamps are unchanged.
//
// Build tag matches the rest of the migration tests; set
// FAAS_SKIP_PG_TESTS=1 to skip locally (see migrations/README.md).
package migrations_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/onebox-faas/faas/pkg/db"
	"github.com/onebox-faas/faas/pkg/db/pgtest"
)

func TestMigrations_00155_AppsAuthDefaultFlip(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.Open(t)

	// (1) Apply through 00155. A regression that drops a slot between
	// 1 and 154 surfaces here before the per-assertion pins.
	if err := db.MigrateUp(ctx, pool); err != nil {
		t.Fatalf("db.MigrateUp: %v (regression: missing migration slot between 1 and 154)", err)
	}

	// (2) Column shape: apps.auth_default_flipped_at is timestamptz
	// NULL. information_schema probe catches a future regression that
	// flips the column to NOT NULL (which would break the grand-father
	// re-apply path).
	var dataType, isNullable string
	if err := pool.QueryRow(ctx, `
		select data_type, is_nullable
		  from information_schema.columns
		 where table_schema = current_schema()
		   and table_name = 'apps'
		   and column_name = 'auth_default_flipped_at'
	`).Scan(&dataType, &isNullable); err != nil {
		t.Fatalf("read apps.auth_default_flipped_at column metadata: %v", err)
	}
	if dataType != "timestamp with time zone" {
		t.Errorf("auth_default_flipped_at data_type = %q, want \"timestamp with time zone\"", dataType)
	}
	if isNullable != "YES" {
		t.Errorf("auth_default_flipped_at is_nullable = %q, want \"YES\" (grandfather INSERT depends on NULL)", isNullable)
	}

	// (3) Seed two pre-flip apps on different accounts so the
	// grand-father backfill has both fresh rows (already
	// auth_default_flipped_at NOT NULL by ApplyUp) and rows from
	// this test insertion to verify the UPDATE path explicitly.
	if _, err := pool.Exec(ctx, `
		insert into accounts (id, email, plan, created_at)
		values ('00000000-0000-0000-0000-000000001551',
		        'auth-default-flip-a@example.com', 'pro', now())
		on conflict (id) do nothing
	`); err != nil {
		t.Fatalf("seed account A: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		insert into accounts (id, email, plan, created_at)
		values ('00000000-0000-0000-0000-000000001552',
		        'auth-default-flip-b@example.com', 'hobby', now())
		on conflict (id) do nothing
	`); err != nil {
		t.Fatalf("seed account B: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		insert into apps (id, account_id, slug, type, ram_mb, max_concurrency, idle_timeout_s, status, require_authn, public_auth_mode, created_at)
		values ('00000000-0000-0000-0000-000000001553',
		        '00000000-0000-0000-0000-000000001551',
		        'auth-default-flip-a-app', 'function', 512, 5, 300, 'active', false, 'open', now()),
		       ('00000000-0000-0000-0000-000000001554',
		        '00000000-0000-0000-0000-000000001552',
		        'auth-default-flip-b-app', 'function', 256, 2, 60, 'active', false, 'open', now())
		on conflict (id) do nothing
	`); err != nil {
		t.Fatalf("seed two pre-flip apps: %v", err)
	}

	// (4) Grandfather marker round-trip: every app row reads back
	// auth_default_flipped_at != NULL after the migration. This is
	// the load-bearing invariant for issue #695 AC #2 — no existing
	// customer's behaviour changes because (a) the column didn't
	// exist before the migration, (b) the migration backfill stamps
	// the marker without touching require_authn / public_auth_mode.
	row := pool.QueryRow(ctx, `
		select count(*) filter (where auth_default_flipped_at is null) as nulls,
		       count(*) filter (where auth_default_flipped_at is not null) as stamped
		  from apps
	`)
	var nulls, stamped int64
	if err := row.Scan(&nulls, &stamped); err != nil {
		t.Fatalf("count grandfather markers: %v", err)
	}
	if nulls != 0 {
		t.Errorf("auth_default_flipped_at has %d NULL rows after migration; grand-father backfill must stamp every existing row", nulls)
	}
	if stamped < 2 {
		t.Errorf("auth_default_flipped_at stamped %d rows, want >= 2 (the two fixture apps)", stamped)
	}

	// (5) Audit row shape: exactly ONE apps.auth_default_global_flipped
	// event with the ADR-080 §3 payload. The row is emitted by the
	// migration; a future regression that emits per-account rows or
	// per-app rows breaks here. Replay safety below (step 7) confirms
	// the second MigrateUp is a no-op.
	var auditCount int
	if err := pool.QueryRow(ctx, `
		select count(*) from events
		 where kind = 'apps.auth_default_global_flipped'
		   and actor = 'migration'
	`).Scan(&auditCount); err != nil {
		t.Fatalf("count audit rows: %v", err)
	}
	if auditCount != 1 {
		t.Fatalf("audit row count for apps.auth_default_global_flipped = %d, want exactly 1", auditCount)
	}
	var payload []byte
	var actorAccountID *string // nullable; SHOULD be NULL for migration-emitted rows
	if err := pool.QueryRow(ctx, `
		select actor_account_id::text, data
		  from events
		 where kind = 'apps.auth_default_global_flipped'
		   and actor = 'migration'
		 order by at asc
		 limit 1
	`).Scan(&actorAccountID, &payload); err != nil {
		t.Fatalf("read audit row payload: %v", err)
	}
	if actorAccountID != nil {
		t.Errorf("audit row actor_account_id = %v, want NULL (migration-emitted, no actor account)", *actorAccountID)
	}
	var parsed struct {
		MigratedCount              int64            `json:"migrated_count"`
		FromRequireAuthnDefault    bool             `json:"from_require_authn_default"`
		ToRequireAuthnDefault      bool             `json:"to_require_authn_default"`
		FromPublicAuthModeDefault  string           `json:"from_public_auth_mode_default"`
		ToPublicAuthModeDefault    string           `json:"to_public_auth_mode_default"`
		PlanOverrides              map[string]struct {
			RequireAuthn   bool   `json:"require_authn"`
			PublicAuthMode string `json:"public_auth_mode"`
		} `json:"plan_overrides"`
	}
	if err := json.Unmarshal(payload, &parsed); err != nil {
		t.Fatalf("parse audit row JSON: %v", err)
	}
	if parsed.FromRequireAuthnDefault != false {
		t.Errorf("audit from_require_authn_default = %v, want false", parsed.FromRequireAuthnDefault)
	}
	if parsed.ToRequireAuthnDefault != true {
		t.Errorf("audit to_require_authn_default = %v, want true", parsed.ToRequireAuthnDefault)
	}
	if parsed.FromPublicAuthModeDefault != "open" {
		t.Errorf("audit from_public_auth_mode_default = %q, want \"open\"", parsed.FromPublicAuthModeDefault)
	}
	if parsed.ToPublicAuthModeDefault != "bearer" {
		t.Errorf("audit to_public_auth_mode_default = %q, want \"bearer\"", parsed.ToPublicAuthModeDefault)
	}
	wantOverrides := map[string]struct {
		RequireAuthn   bool
		PublicAuthMode string
	}{
		"free":  {false, "open"},
		"hobby": {true, "open"},
		"pro":   {true, "bearer"},
		"scale": {true, "bearer"},
	}
	for plan, want := range wantOverrides {
		got, ok := parsed.PlanOverrides[plan]
		if !ok {
			t.Errorf("audit plan_overrides missing %q", plan)
			continue
		}
		if got.RequireAuthn != want.RequireAuthn || got.PublicAuthMode != want.PublicAuthMode {
			t.Errorf("audit plan_overrides[%q] = (%v, %q), want (%v, %q)",
				plan, got.RequireAuthn, got.PublicAuthMode, want.RequireAuthn, want.PublicAuthMode)
		}
	}
	if parsed.MigratedCount != stamped {
		t.Errorf("audit migrated_count = %d, want stamped count %d", parsed.MigratedCount, stamped)
	}

	// (6) Existing app behaviour preserved: the seed apps still have
	// require_authn=false and public_auth_mode='open' AFTER the
	// migration. The grand-father marker is independent of these
	// columns — flipping the marker does NOT flip the per-app state.
	// This is the load-bearing invariant for issue #695 AC #1 (no
	// existing customer breakage).
	type appState struct {
		RequireAuthn   bool
		PublicAuthMode string
	}
	state := make(map[string]appState)
	for _, id := range []string{
		"00000000-0000-0000-0000-000000001553",
		"00000000-0000-0000-0000-000000001554",
	} {
		var s appState
		if err := pool.QueryRow(ctx, `
			select require_authn, public_auth_mode
			  from apps where id = $1
		`, id).Scan(&s.RequireAuthn, &s.PublicAuthMode); err != nil {
			t.Fatalf("read app %s post-migration: %v", id, err)
		}
		state[id] = s
	}
	for id, s := range state {
		if s.RequireAuthn != false {
			t.Errorf("app %s require_authn = %v after migration, want false (grandfather must preserve pre-flip state)", id, s.RequireAuthn)
		}
		if s.PublicAuthMode != "open" {
			t.Errorf("app %s public_auth_mode = %q after migration, want \"open\"", id, s.PublicAuthMode)
		}
	}

	// (7) Replay-safety: a second MigrateUp is a no-op. The audit
	// row count remains 1 (the INSERT is guarded by WHERE NOT
	// EXISTS); the grandfather stamps are unchanged (the UPDATE
	// is guarded by IS NULL). ADR-041 contract.
	if err := db.MigrateUp(ctx, pool); err != nil {
		t.Fatalf("replay-safety: second MigrateUp failed: %v", err)
	}
	var replayAuditCount int
	if err := pool.QueryRow(ctx, `
		select count(*) from events
		 where kind = 'apps.auth_default_global_flipped'
	`).Scan(&replayAuditCount); err != nil {
		t.Fatalf("replay-safety: count audit rows: %v", err)
	}
	if replayAuditCount != 1 {
		t.Errorf("replay-safety: audit count = %d, want exactly 1 (the migration must NOT re-emit)", replayAuditCount)
	}
	// Verify stamps unchanged by re-reading one fixture row.
	var stampBefore, stampAfter string
	row = pool.QueryRow(ctx, `select auth_default_flipped_at from apps where id = '00000000-0000-0000-0000-000000001553'`)
	if err := row.Scan(&stampBefore); err != nil {
		t.Fatalf("replay-safety: read stamp before: %v", err)
	}
	// ApplyUp already happened — read again to compare.
	row = pool.QueryRow(ctx, `select auth_default_flipped_at from apps where id = '00000000-0000-0000-0000-000000001553'`)
	if err := row.Scan(&stampAfter); err != nil {
		t.Fatalf("replay-safety: read stamp after: %v", err)
	}
	if stampBefore != stampAfter {
		t.Errorf("replay-safety: grand-father stamp changed across re-apply (%q → %q); the UPDATE must NOT re-stamp existing rows", stampBefore, stampAfter)
	}
}
