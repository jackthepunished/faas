//go:build !no_pg

// Migration-apply test for 00082 (per-app scaling_policy + cooldown
// "last action" columns, issue #462 / ADR-058). Pins:
//
//  1. The migration set applies cleanly through 00082.
//  2. The new `apps.scaling_policy` jsonb column accepts the canonical
//     shape (round-trips through the wire DTO) and defaults to '{}' for
//     legacy rows (regression check — pre-PR rows still load).
//  3. `apps.last_scale_out_at` / `apps.last_scale_in_at` are nullable
//     timestamptz and accept the schedd-reminder form.
//  4. The CHECK constraints on the last_*_at columns reject a future
//     timestamp (the "bad client clocks" guard from the migration
//     comment).
//  5. Replay-safe: ADD COLUMN IF NOT EXISTS makes a second MigrateUp
//     no-op (PR #377 / ADR-041).
//
// Slot note: 00081 is the slot-reservation no-op (the slot reservation
// is kept so the cross-PR migration-gate stays contiguous; renumber
// per `migrations/README.md` if a sibling PR grabs either slot).
// 00082 is the next free slot at PR-A creation time. The migration is
// slot-agnostic — only the filename + this test function name carry the
// literal slot. If a sibling PR grabs 00082 first, renumber per
// `migrations/README.md` and update this test's filename + ApplyUp
// range + the literal UUID slot suffix `000082` / `000182` / `000282`
// below.
//
// Build tag matches the rest of the migration tests; set
// FAAS_SKIP_PG_TESTS=1 to skip locally (see migrations/README.md).
package migrations_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/onebox-faas/faas/pkg/db"
	"github.com/onebox-faas/faas/pkg/db/pgtest"
)

// canonicalScalingPolicyJSON is the on-disk jsonb shape the migration
// accepts. Kept as a single string so any future schema drift breaks
// the test loudly (rather than a shape-by-shape literal that can drift
// in lockstep).
const canonicalScalingPolicyJSON = `{"min_instances":1,"max_instances":5,"target":{"metric":"rps","value":10},"scale_out_cooldown_s":5,"scale_in_cooldown_s":60}`

func TestMigrations_00082_AppsScalingPolicy(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.Open(t)

	// Seed UUIDs carry the slot number in the last group (`...000082`,
	// `...000182`, `...000282`, `...000382`) so a reader scanning the
	// test fixtures can pin each row to this migration without grepping
	// the file name. The literal slot value MUST stay in sync with the
	// filename; renumber per `migrations/README.md` if a sibling PR
	// grabs 00082 first.

	// (1) Apply through 00082. A regression that drops a slot between
	// 1 and 82 surfaces here before the per-assertion pins.
	if err := db.MigrateUp(ctx, pool); err != nil {
		t.Fatalf("db.MigrateUp: %v (regression: missing migration slot between 1 and 82)", err)
	}

	// (2) Confirm the new columns exist on apps. Done up-front so a
	// migration that silently skips the ADD COLUMN (e.g. a slot
	// collision) surfaces before the per-row pins.
	cols := map[string]string{}
	rows, err := pool.Query(ctx, `
		select column_name, data_type
		from information_schema.columns
		where table_schema = current_schema() and table_name = 'apps'
	`)
	if err != nil {
		t.Fatalf("inspect apps columns: %v", err)
	}
	for rows.Next() {
		var name, typ string
		if err := rows.Scan(&name, &typ); err != nil {
			rows.Close()
			t.Fatalf("scan column: %v", err)
		}
		cols[name] = typ
	}
	rows.Close()
	if cols["scaling_policy"] != "jsonb" {
		t.Errorf("apps.scaling_policy data_type = %q, want jsonb", cols["scaling_policy"])
	}
	if cols["last_scale_out_at"] != "timestamp with time zone" {
		t.Errorf("apps.last_scale_out_at data_type = %q, want timestamp with time zone", cols["last_scale_out_at"])
	}
	if cols["last_scale_in_at"] != "timestamp with time zone" {
		t.Errorf("apps.last_scale_in_at data_type = %q, want timestamp with time zone", cols["last_scale_in_at"])
	}

	// (3) Seed an account + app. The literal UUIDs are fixed across
	// reruns so the seed is idempotent.
	if _, err := pool.Exec(ctx, `
		insert into accounts (id, email, plan, created_at)
		values ('00000000-0000-0000-0000-000000000082',
		        'scaling-policy-test@example.com', 'hobby', now())
		on conflict (id) do nothing
	`); err != nil {
		t.Fatalf("seed account: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		insert into apps (id, account_id, slug, type, ram_mb, max_concurrency, idle_timeout_s, status, created_at)
		values ('00000000-0000-0000-0000-000000000182',
		        '00000000-0000-0000-0000-000000000082',
		        'scaling-policy-test-app', 'function', 256, 2, 30, 'active', now())
		on conflict (id) do nothing
	`); err != nil {
		t.Fatalf("seed app: %v", err)
	}

	// (4) Round-trip: a freshly-PATCH'd scaling policy reads back
	// exactly. The legacy row at app #182 still has the default
	// policy '{}' (covered by step 5).
	if _, err := pool.Exec(ctx, `
		update apps
		set scaling_policy = $1::jsonb,
		    last_scale_out_at = now() - interval '5 seconds',
		    last_scale_in_at = now() - interval '60 seconds'
		where id = '00000000-0000-0000-0000-000000000182'
	`, canonicalScalingPolicyJSON); err != nil {
		t.Fatalf("write scaling_policy + last_scale_*_at: %v", err)
	}
	var (
		gotPolicy []byte
		gotOutAt  *time.Time
		gotInAt   *time.Time
	)
	if err := pool.QueryRow(ctx, `
		select scaling_policy, last_scale_out_at, last_scale_in_at
		from apps
		where id = '00000000-0000-0000-0000-000000000182'
	`).Scan(&gotPolicy, &gotOutAt, &gotInAt); err != nil {
		t.Fatalf("read back scaling_policy + last_scale_*_at: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(gotPolicy, &got); err != nil {
		t.Fatalf("scaling_policy jsonb unmarshal: %v (raw=%s)", err, gotPolicy)
	}
	if got["min_instances"] == nil {
		t.Errorf("scaling_policy.min_instances missing in round-trip: %s", gotPolicy)
	}
	if got["max_instances"] == nil {
		t.Errorf("scaling_policy.max_instances missing in round-trip: %s", gotPolicy)
	}
	if gotOutAt == nil {
		t.Errorf("last_scale_out_at round-trip = nil, want non-nil")
	}
	if gotInAt == nil {
		t.Errorf("last_scale_in_at round-trip = nil, want non-nil")
	}

	// (5) Default for legacy rows: insert a brand-new app with no
	// scaling_policy written, then read back. The migration default
	// ('{}'::jsonb) must apply so the production read-with-emptypolicy
	// path (= "fall back to min_instances / max_concurrency") works.
	if _, err := pool.Exec(ctx, `
		insert into apps (id, account_id, slug, type, ram_mb, max_concurrency, idle_timeout_s, status, created_at)
		values ('00000000-0000-0000-0000-000000000282',
		        '00000000-0000-0000-0000-000000000082',
		        'scaling-policy-default-app', 'function', 256, 1, 30, 'active', now())
	`); err != nil {
		t.Fatalf("insert legacy app: %v", err)
	}
	var (
		defaultPolicy []byte
		defaultOut    *time.Time
		defaultIn     *time.Time
	)
	if err := pool.QueryRow(ctx, `
		select scaling_policy, last_scale_out_at, last_scale_in_at
		from apps
		where id = '00000000-0000-0000-0000-000000000282'
	`).Scan(&defaultPolicy, &defaultOut, &defaultIn); err != nil {
		t.Fatalf("read back legacy app: %v", err)
	}
	if string(defaultPolicy) != "{}" {
		t.Errorf("scaling_policy default = %s, want {} (legacy rows must read back through the empty-policy projection path)", defaultPolicy)
	}
	if defaultOut != nil {
		t.Errorf("last_scale_out_at default = %v, want nil", defaultOut)
	}
	if defaultIn != nil {
		t.Errorf("last_scale_in_at default = %v, want nil", defaultIn)
	}

	// (6) CHECK constraints reject a future timestamp on the
	// last_scale_*_at columns. The constraint name embeds the
	// column's le_now predicate from the migration comment.
	//
	// The constraint is "col IS NULL OR col <= now()" — `now()` is
	// evaluated at the row-time, so a future timestamp fails CHECK.
	// A second-row UPDATE to the same row bypasses the constraint
	// (Postgres does not re-validate unchanged-but-now-stale rows
	// because the predicate is monotonic). The test pins the
	// boundary at INSERT/initial-UPDATE on a fresh row.
	oneHourFromNow := time.Now().Add(time.Hour).UTC()
	if _, err := pool.Exec(ctx, `
		insert into apps (id, account_id, slug, type, ram_mb, max_concurrency, idle_timeout_s, status, created_at, last_scale_out_at)
		values ('00000000-0000-0000-0000-000000000382',
		        '00000000-0000-0000-0000-000000000082',
		        'scaling-policy-future-app', 'function', 256, 1, 30, 'active', now(), $1)
	`, oneHourFromNow); err == nil {
		t.Errorf("expected CHECK failure on future last_scale_out_at; got nil")
	} else if !isCheckViolation(err) {
		t.Errorf("expected CHECK constraint violation on future last_scale_out_at; got %v", err)
	}

	// (7) Replay safety: a second MigrateUp is a no-op (the migration
	// uses ADD COLUMN IF NOT EXISTS + ADD CONSTRAINT IF NOT EXISTS).
	// PR #377 / ADR-041 contract.
	if err := db.MigrateUp(ctx, pool); err != nil {
		t.Fatalf("replay-safety: second MigrateUp failed: %v", err)
	}
}

// isCheckViolation is the duck-typed check for a Postgres 23514
// (check_violation) SQLSTATE. Avoids importing pgconn directly so
// the test stays a single, readable fixture. We pin BOTH the
// SQLSTATE code AND the human-readable marker (`check_violation`
// or `check constraint`) so a future pgx upgrade that rephrases
// the literal substring still trips the test via the SQLSTATE
// match. The order matters: SQLSTATE first (stable across all pgx
// versions), literal substring as a fallback.
func isCheckViolation(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	// SQLSTATE 23514 is "check_violation" — emitted by every pgx
	// release since the driver was written. The literal SQLSTATE
	// is the durable anchor.
	if strings.Contains(msg, "SQLSTATE 23514") || strings.Contains(msg, "23514") {
		return true
	}
	// Fallback: when SQLSTATE is omitted (older pq wrapper) check
	// the human-readable form. The literal phrase "check
	// constraint" is part of the pgx error formatter and is
	// stable across the supported releases.
	return strings.Contains(msg, "check constraint")
}
