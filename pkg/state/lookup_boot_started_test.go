// Tests for PgStore.LookupBootStartedForWakes (ADR-123). Closes PR #1015
// review finding #3: pin the DISTINCT ON semantics + canonical-row
// preference against future schema drift. Lives behind pgtest.Open (skips
// when DATABASE_URL is unset) so it runs as part of `make test` on
// machines with a Postgres available and silently no-ops elsewhere.
//
// The test exercises four contracts:
//  1. DISTINCT ON (data->>'wake_id') picks the EARLIEST row per
//     wake_id. A re-wake that emits two wake.boot_started rows for
//     the same wake_id must surface the first row's telemetry.
//  2. The mirror row (the canonical-emit-failure path closed by
//     pkg/vmmdgrpc/server.go:emitBootStartedMirror) must NOT be
//     surfaced — the mirror uses a later `at` timestamp and is
//     the fallback, never the preferred row.
//  3. Empty / nil input returns an empty map without touching the
//     pool (exercises the early-return branch at pgstore.go:10362).
//  4. Unknown wake_ids produce an empty map (not an error) so the
//     dashboard's join-LATERAL doesn't have to special-case.
package state

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/onebox-faas/faas/pkg/db"
	"github.com/onebox-faas/faas/pkg/db/pgtest"
)

// insertBootStarted is a tiny test fixture helper. Production code
// goes through events.BootStarted.Payload() to build the jsonb body
// (pkg/events/wake.go:311); the test bypasses that to keep the test
// self-contained — no events-package import, no follow-the-package
// tx. The payload shape mirrors the production BootStarted.Payload
// keys exactly so a future schema migration that renames a key
// surfaces here as a SQL scan error rather than a silent miss.
func insertBootStarted(t *testing.T, ctx context.Context, pool *pgxpool.Pool, data map[string]any, at string) {
	t.Helper()
	raw, err := json.Marshal(data)
	if err != nil {
		t.Fatalf("marshal data: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO events (actor, kind, data, at) VALUES ($1, $2, $3, $4::timestamptz)`,
		"schedd", "wake.boot_started", raw, at,
	); err != nil {
		t.Fatalf("insert wake.boot_started: %v", err)
	}
}

// cleanupBootStarted removes the rows this test inserted so re-runs
// (or sibling tests in the same package) don't accumulate. Pinned to
// the wake_id namespace below — the t.Name() suffix makes the names
// unique per test.
func cleanupBootStarted(t *testing.T, ctx context.Context, pool *pgxpool.Pool, wakeIDs []string) {
	t.Helper()
	if _, err := pool.Exec(ctx,
		`DELETE FROM events WHERE data->>'wake_id' = ANY($1)`,
		wakeIDs,
	); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
}

func TestLookupBootStartedForWakes_RoundTrip(t *testing.T) {
	pool := pgtest.Open(t)
	ctx := context.Background()
	if err := db.MigrateUp(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	s := NewPgStore(pool)

	// Unique-per-test wake_ids so concurrent runs and re-runs don't
	// collide on the partial jsonb index (events_wake_id_idx, ADR-064).
	wakeID1 := "wake-rt-1-" + t.Name()
	wakeID2 := "wake-rt-2-" + t.Name()

	// wakeID1: canonical then mirror. The mirror is the
	// emitBootStartedMirror fallback — it MUST be discarded by
	// DISTINCT ON ORDER BY at ASC.
	insertBootStarted(t, ctx, pool, map[string]any{
		"wake_id":              wakeID1,
		"app_id":               "app-a",
		"instance_id":          "ins-1",
		"node_id":              "node-1",
		"method":               "cold_boot",
		"requested_at":         "2026-08-21T10:00:00Z",
		"trigger":              "gateway",
		"queued_count":         8,
		"concurrency_at_admit": 2,
	}, "2026-08-21T10:00:00Z")
	insertBootStarted(t, ctx, pool, map[string]any{
		"wake_id":              wakeID1,
		"app_id":               "app-a",
		"instance_id":          "ins-1",
		"node_id":              "node-1",
		"method":               "cold_boot",
		"requested_at":         "2026-08-21T10:00:00.100Z",
		"trigger":              "mirror",
		"queued_count":         99,
		"concurrency_at_admit": 99,
	}, "2026-08-21T10:00:00.100Z")

	// wakeID2: cron schedule, capacity-1 cold start (no mirror row).
	insertBootStarted(t, ctx, pool, map[string]any{
		"wake_id":              wakeID2,
		"app_id":               "app-b",
		"instance_id":          "ins-2",
		"node_id":              "node-1",
		"method":               "cold_boot",
		"requested_at":         "2026-08-21T10:01:00Z",
		"trigger":              "cron.schedule",
		"queued_count":         0,
		"concurrency_at_admit": 0,
	}, "2026-08-21T10:01:00Z")

	t.Cleanup(func() {
		cleanupBootStarted(t, ctx, pool, []string{wakeID1, wakeID2})
	})

	got, err := s.LookupBootStartedForWakes(ctx, []string{wakeID1, wakeID2})
	if err != nil {
		t.Fatalf("LookupBootStartedForWakes: %v", err)
	}

	// wakeID1: trigger MUST be 'gateway' (canonical), NOT 'mirror'
	// (fallback) — DISTINCT ON ORDER BY at ASC picks the earliest.
	m1, ok := got[wakeID1]
	if !ok {
		t.Fatalf("missing wakeID1 in result; got keys: %v", keys(got))
	}
	if m1.Trigger != "gateway" {
		t.Errorf("wakeID1 trigger: got %q want %q (DISTINCT ON must prefer canonical over mirror)", m1.Trigger, "gateway")
	}
	if m1.QueuedCount != 8 {
		t.Errorf("wakeID1 queued_count: got %d want 8", m1.QueuedCount)
	}
	if m1.ConcurrencyAtAdmit != 2 {
		t.Errorf("wakeID1 concurrency_at_admit: got %d want 2", m1.ConcurrencyAtAdmit)
	}

	// wakeID2: cron-driven, zero queue at admit.
	m2, ok := got[wakeID2]
	if !ok {
		t.Fatalf("missing wakeID2 in result; got keys: %v", keys(got))
	}
	if m2.Trigger != "cron.schedule" {
		t.Errorf("wakeID2 trigger: got %q want cron.schedule", m2.Trigger)
	}
	if m2.QueuedCount != 0 || m2.ConcurrencyAtAdmit != 0 {
		t.Errorf("wakeID2 cold-start telemetry not zero: %+v", m2)
	}
}

func TestLookupBootStartedForWakes_EmptyInput(t *testing.T) {
	pool := pgtest.Open(t)
	ctx := context.Background()
	if err := db.MigrateUp(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	s := NewPgStore(pool)

	// nil input — exercises the early-return branch at pgstore.go:10362
	// without touching the pool. The empty-map result is the contract.
	got, err := s.LookupBootStartedForWakes(ctx, nil)
	if err != nil {
		t.Fatalf("nil input: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("nil input produced non-empty map: %v", got)
	}

	// Empty slice — same branch, same contract.
	got, err = s.LookupBootStartedForWakes(ctx, []string{})
	if err != nil {
		t.Fatalf("empty input: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("empty input produced non-empty map: %v", got)
	}
}

func TestLookupBootStartedForWakes_UnknownWakeID(t *testing.T) {
	pool := pgtest.Open(t)
	ctx := context.Background()
	if err := db.MigrateUp(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	s := NewPgStore(pool)

	got, err := s.LookupBootStartedForWakes(ctx, []string{"wake-does-not-exist-" + t.Name()})
	if err != nil {
		t.Fatalf("unknown wakeID: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("unknown wakeID produced non-empty map: %v", got)
	}
}

// keys is a small helper for nicer error messages — drops the wakeID
// values into a slice so multi-row failures show the same key set
// across runs.
func keys(m map[string]WakeBootMeta) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
