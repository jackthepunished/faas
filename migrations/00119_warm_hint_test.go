//go:build !no_pg

// Migration-apply tests for the Tier A7 edge split (ADR-070) —
// slots 119, 120. Pins:
//
//  1. The migration set applies cleanly through 120.
//  2. 00119 (warm_hint):
//     - The table exists and accepts canonical (app_id, node_id,
//     written_at) inserts.
//     - The CHECK constraint on written_at allows recent values
//     and rejects a future timestamp (the "bad client clocks"
//     guard from the migration comment).
//     - The index on node_id exists (for the future "list all hot
//     apps on node X" dashboard query).
//     - Replay-safe: a second MigrateUp is a no-op.
//  3. 00120 (pg_ratelimit_counters):
//     - The table exists and accepts canonical (scope, subject_id,
//     plan, tokens) inserts.
//     - tokens is bigint (not float — the "no floats near money"
//     invariant from CLAUDE.md).
//     - CHECK constraints on scope / plan / tokens reject bad
//     values.
//     - The hot-path partial index on (subject_id) WHERE scope='app'
//     exists.
//     - Replay-safe: a second MigrateUp is a no-op.
//
// Slot note: 00118 (deployments_sidecars on main) is the previous
// slot — the embedded migration set is contiguous 1..N where N
// is the highest slot in the embedded set (N = 120 here). The
// literal UUID slot values `000116` / `000216` / `000316` / `000416`
// are test fixture identifiers — unrelated to the slot numbers,
// chosen so the UUIDs stay unique within this PR's fixture set.
// Renumber history: this PR originally added migrations at 116/117/118
// but main added sidecars + fences at those slots in the interim, so
// the real schemas renumbered to 119/120 (ADR-070 §Migration slot
// renumber).
package migrations_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/onebox-faas/faas/pkg/db"
	"github.com/onebox-faas/faas/pkg/db/pgtest"
)

// TestMigrations_00119_WarmHint pins the warm_hint table shape.
func TestMigrations_00119_WarmHint(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.Open(t)

	if err := db.MigrateUp(ctx, pool); err != nil {
		t.Fatalf("db.MigrateUp: %v (regression: missing slot between 1 and 119)", err)
	}

	// (1) Confirm the table exists with the canonical columns.
	cols := map[string]string{}
	rows, err := pool.Query(ctx, `
		select column_name, data_type
		from information_schema.columns
		where table_schema = current_schema() and table_name = 'warm_hint'
	`)
	if err != nil {
		t.Fatalf("inspect warm_hint columns: %v", err)
	}
	for rows.Next() {
		var name, typ string
		if err := rows.Scan(&name, &typ); err != nil {
			rows.Close()
			t.Fatalf("scan: %v", err)
		}
		cols[name] = typ
	}
	rows.Close()
	if cols["app_id"] != "uuid" {
		t.Errorf("warm_hint.app_id data_type = %q, want uuid", cols["app_id"])
	}
	if cols["node_id"] != "uuid" {
		t.Errorf("warm_hint.node_id data_type = %q, want uuid", cols["node_id"])
	}
	if cols["written_at"] != "timestamp with time zone" {
		t.Errorf("warm_hint.written_at data_type = %q, want timestamp with time zone", cols["written_at"])
	}

	// (2) Seed a canonical row + verify the round-trip.
	const appID = "00000000-0000-0000-0000-000000000116"
	const nodeID = "00000000-0000-0000-0000-000000000216"
	if _, err := pool.Exec(ctx, `
		insert into warm_hint (app_id, node_id, written_at)
		values ($1, $2, now())
		on conflict (app_id) do update
		    set node_id = excluded.node_id,
		        written_at = excluded.written_at
	`, appID, nodeID); err != nil {
		t.Fatalf("seed warm_hint: %v", err)
	}
	var gotNode string
	if err := pool.QueryRow(ctx, `select node_id::text from warm_hint where app_id = $1`, appID).Scan(&gotNode); err != nil {
		t.Fatalf("read back warm_hint: %v", err)
	}
	if gotNode != nodeID {
		t.Errorf("warm_hint.node_id = %q, want %q", gotNode, nodeID)
	}

	// (3) CHECK rejects a future timestamp.
	oneHourFromNow := time.Now().Add(time.Hour).UTC()
	if _, err := pool.Exec(ctx, `
		update warm_hint set written_at = $1 where app_id = $2
	`, oneHourFromNow, appID); err == nil {
		t.Errorf("expected CHECK failure on future written_at; got nil")
	} else if !isCheckViolation(err) {
		t.Errorf("expected CHECK constraint violation on future written_at; got %v", err)
	}

	// (4) Index on node_id exists.
	idxRows, err := pool.Query(ctx, `
		select indexname from pg_indexes
		where schemaname = current_schema() and tablename = 'warm_hint'
	`)
	if err != nil {
		t.Fatalf("inspect warm_hint indexes: %v", err)
	}
	foundNodeIDIndex := false
	for idxRows.Next() {
		var name string
		if err := idxRows.Scan(&name); err != nil {
			idxRows.Close()
			t.Fatalf("scan index: %v", err)
		}
		if strings.Contains(name, "node_id") {
			foundNodeIDIndex = true
		}
	}
	idxRows.Close()
	if !foundNodeIDIndex {
		t.Errorf("warm_hint index on node_id missing")
	}

	// (5) Replay safety.
	if err := db.MigrateUp(ctx, pool); err != nil {
		t.Fatalf("replay-safety: second MigrateUp failed: %v", err)
	}
}

// TestMigrations_00120_PgRateLimit pins the pg_ratelimit_counters
// table shape (the central rate-limit counter, ADR-070 item 7).
func TestMigrations_00120_PgRateLimit(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.Open(t)

	if err := db.MigrateUp(ctx, pool); err != nil {
		t.Fatalf("db.MigrateUp: %v", err)
	}

	// (1) tokens is bigint (no floats near money).
	cols := map[string]string{}
	rows, err := pool.Query(ctx, `
		select column_name, data_type
		from information_schema.columns
		where table_schema = current_schema() and table_name = 'pg_ratelimit_counters'
	`)
	if err != nil {
		t.Fatalf("inspect pg_ratelimit_counters columns: %v", err)
	}
	for rows.Next() {
		var name, typ string
		if err := rows.Scan(&name, &typ); err != nil {
			rows.Close()
			t.Fatalf("scan: %v", err)
		}
		cols[name] = typ
	}
	rows.Close()
	if cols["tokens"] != "bigint" {
		t.Errorf("pg_ratelimit_counters.tokens data_type = %q, want bigint (no floats near money)", cols["tokens"])
	}

	// (2) Seed a canonical row + verify the round-trip.
	const subjectID = "00000000-0000-0000-0000-000000000316"
	if _, err := pool.Exec(ctx, `
		insert into pg_ratelimit_counters (scope, subject_id, plan, tokens)
		values ('app', $1, 'hobby', 100)
		on conflict (scope, subject_id, plan) do update
		    set tokens = excluded.tokens
	`, subjectID); err != nil {
		t.Fatalf("seed pg_ratelimit_counters: %v", err)
	}
	var tokens int64
	if err := pool.QueryRow(ctx, `
		select tokens from pg_ratelimit_counters
		where scope = 'app' and subject_id = $1 and plan = 'hobby'
	`, subjectID).Scan(&tokens); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if tokens != 100 {
		t.Errorf("tokens = %d, want 100", tokens)
	}

	// (3) CHECKs reject bad values.
	// 3a: bad scope.
	if _, err := pool.Exec(ctx, `
		insert into pg_ratelimit_counters (scope, subject_id, plan, tokens)
		values ('bad', '00000000-0000-0000-0000-000000000416', 'hobby', 100)
	`); err == nil {
		t.Errorf("expected CHECK failure on bad scope; got nil")
	} else if !isCheckViolation(err) {
		t.Errorf("expected CHECK constraint violation on bad scope; got %v", err)
	}
	// 3b: bad plan.
	if _, err := pool.Exec(ctx, `
		insert into pg_ratelimit_counters (scope, subject_id, plan, tokens)
		values ('app', '00000000-0000-0000-0000-000000000416', 'enterprise', 100)
	`); err == nil {
		t.Errorf("expected CHECK failure on bad plan; got nil")
	} else if !isCheckViolation(err) {
		t.Errorf("expected CHECK constraint violation on bad plan; got %v", err)
	}
	// 3c: negative tokens.
	if _, err := pool.Exec(ctx, `
		insert into pg_ratelimit_counters (scope, subject_id, plan, tokens)
		values ('app', '00000000-0000-0000-0000-0000-000000000416', 'hobby', -1)
	`); err == nil {
		t.Errorf("expected CHECK failure on negative tokens; got nil")
	} else if !isCheckViolation(err) {
		t.Errorf("expected CHECK constraint violation on negative tokens; got %v", err)
	}

	// (4) The hot-path partial index exists.
	idxRows, err := pool.Query(ctx, `
		select indexname from pg_indexes
		where schemaname = current_schema() and tablename = 'pg_ratelimit_counters'
	`)
	if err != nil {
		t.Fatalf("inspect pg_ratelimit_counters indexes: %v", err)
	}
	foundSubjectIDIndex := false
	for idxRows.Next() {
		var name string
		if err := idxRows.Scan(&name); err != nil {
			idxRows.Close()
			t.Fatalf("scan index: %v", err)
		}
		if strings.Contains(name, "subject_id") {
			foundSubjectIDIndex = true
		}
	}
	idxRows.Close()
	if !foundSubjectIDIndex {
		t.Errorf("pg_ratelimit_counters partial index on subject_id missing")
	}

	// (5) Replay safety.
	if err := db.MigrateUp(ctx, pool); err != nil {
		t.Fatalf("replay-safety: second MigrateUp failed: %v", err)
	}
}

// TestMigrations_00119_ReserveSlot removed: no fence at 121 in this PR.
// The slot between 00120 (pg_ratelimit_counters) and the next main
// migration is unowned by this branch — the migration set applies
// contiguously from 00118 (deployments_sidecars, on main) through
// 00120 (this PR's last migration). A future fence, if needed,
// would land at the next free slot; not in this PR.
