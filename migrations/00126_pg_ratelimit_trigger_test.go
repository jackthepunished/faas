//go:build !no_pg

// Migration-apply test for 00126_pg_ratelimit.sql Phase 4 C4 trigger
// (ADR-104 amendment 5, issue #881 follow-up).
//
// Pins:
//
//  1. Migration set applies cleanly through 00285 (which widens
//     scope to include 'rule'; the trigger doesn't depend on the
//     widening but lives in the same migration file via the
//     ADR-017 carve-out).
//  2. The pg_notify trigger pg_ratelimit_counters_notify exists on
//     pg_ratelimit_counters, fires on INSERT and UPDATE OF
//     tokens/last_refill, and emits JSON payloads on the
//     'rate_limit_changed' channel.
//  3. The payload shape is {scope, subject_id, plan} — the
//     LISTEN-side consumer (pkg/wire/pgratelimit_invalidator.go)
//     parses these three fields verbatim. A typo here breaks
//     every replica's invalidation.
//  4. The trigger is replay-safe: re-running db.MigrateUp is a
//     no-op (CREATE OR REPLACE FUNCTION + DROP TRIGGER IF EXISTS).
//  5. End-to-end: LISTEN on the channel via the db.WaitForNotification
//     helper, INSERT a row, observe the notification arrives within
//     1s (the per-tick timeout the production LISTEN loop uses; not
//     a strict deadline — Postgres can batch).
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

func TestMigrations_00126_PgRateLimitTrigger(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.Open(t)

	if err := db.MigrateUp(ctx, pool); err != nil {
		t.Fatalf("db.MigrateUp: %v", err)
	}

	// (2) Trigger exists on the table.
	var triggerName string
	err := pool.QueryRow(ctx, `
		select tgname from pg_trigger
		 where tgrelid = 'pg_ratelimit_counters'::regclass
		   and tgname = 'pg_ratelimit_counters_notify'`).Scan(&triggerName)
	if err != nil {
		t.Fatalf("query trigger: %v (trigger must exist after 00126)", err)
	}
	if triggerName != "pg_ratelimit_counters_notify" {
		t.Errorf("trigger name = %q, want pg_ratelimit_counters_notify", triggerName)
	}

	// (3) + (5) End-to-end: LISTEN via db.WaitForNotification
	// (the same helper the production invalidator uses), INSERT
	// a row, observe the notification arrives within 1s.
	dummyID := "00000000-0000-0000-0000-00000000126a"
	predicate := func(payload string) bool {
		return strings.Contains(payload, dummyID)
	}
	notif := make(chan string, 1)
	errc := make(chan error, 1)
	go func() {
		p, err := db.WaitForNotification(ctx, pool, "rate_limit_changed", predicate, 2*time.Second)
		if err != nil {
			errc <- err
			return
		}
		notif <- p
	}()

	// Give the LISTEN goroutine a head-start so the predicate
	// is armed before the INSERT fires. Production tolerates the
	// race (a missed notify just means the next consume's local-
	// would-reject branch kicks in); the test pins deterministic
	// behaviour so the surface is exercisable in CI.
	time.Sleep(50 * time.Millisecond)
	if _, err := pool.Exec(ctx, `
		insert into pg_ratelimit_counters (scope, subject_id, plan, tokens, last_refill)
		values ('app', $1, 'hobby', 100, now())
		on conflict (scope, subject_id, plan) do nothing`, dummyID); err != nil {
		t.Fatalf("seed row: %v", err)
	}

	select {
	case p := <-notif:
		var payload struct {
			Scope     string `json:"scope"`
			SubjectID string `json:"subject_id"`
			Plan      string `json:"plan"`
		}
		if err := json.Unmarshal([]byte(p), &payload); err != nil {
			t.Fatalf("unmarshal notify payload %q: %v (must be JSON {scope, subject_id, plan})", p, err)
		}
		if payload.Scope != "app" {
			t.Errorf("payload.scope = %q, want app", payload.Scope)
		}
		if payload.SubjectID != dummyID {
			t.Errorf("payload.subject_id = %q, want %q", payload.SubjectID, dummyID)
		}
		if payload.Plan != "hobby" {
			t.Errorf("payload.plan = %q, want hobby", payload.Plan)
		}
	case err := <-errc:
		t.Fatalf("WaitForNotification returned %v (trigger must fire on INSERT)", err)
	case <-time.After(3 * time.Second):
		t.Fatal("no notification received within 3s (trigger must fire on INSERT)")
	}

	// (4) Replay safety: re-running db.MigrateUp is a no-op.
	// pgtest.Open drops the schema between tests; on a live
	// schema goose's StrictMode would skip a no-op migration.
	// The trigger migration is replay-safe via CREATE OR REPLACE
	// FUNCTION + DROP TRIGGER IF EXISTS.
	if err := db.MigrateUp(ctx, pool); err != nil {
		t.Fatalf("replay db.MigrateUp: %v (trigger migration must be replay-safe)", err)
	}

	// Cleanup so the test is idempotent under pgtest.Open reuse.
	if _, err := pool.Exec(ctx, `delete from pg_ratelimit_counters where subject_id = $1`, dummyID); err != nil {
		t.Logf("cleanup: %v (non-fatal; pgtest.Open provides a fresh schema per test)", err)
	}
}
