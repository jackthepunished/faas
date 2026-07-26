// pgstore_cron_test.go — round-trip tests for the crons paths touched
// by PR #239. Migration 00047 adds created_at to crons (the column
// 00002's `create table if not exists` silently dropped); the
// sqlc-check gate keeps pkg/state/queries.sql and
// pkg/state/pgstore.go in sync.
//
// These tests are the load-bearing pair: if a future schema change
// removes created_at or a future pgstore.go edit changes the column
// count, CreateCron/CronByID/ListCronsForAccount will fail to Scan
// (the regression TestPg_ListCronsForAccount_ReturnsSeededRows found
// via PR #83; that one covers ListCronsForAccount specifically —
// this file adds the CreateCron/CronByID round-trips the simpler
// test does not, plus an UpdateCron idempotency check).
//
// Skips when Postgres is unreachable (pgtest.Open handles the skip).
package state_test

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/onebox-faas/faas/pkg/api"
	"github.com/onebox-faas/faas/pkg/db"
	"github.com/onebox-faas/faas/pkg/db/pgtest"
	"github.com/onebox-faas/faas/pkg/state"
)

// cronSeedCounter avoids email/slug collisions across cron tests in
// the same run. pgtest.Open shares a Postgres across the test binary
// (one schema per pool), so concurrent tests using hardcoded
// fixtures would 23505 on accounts_email_key.
var cronSeedCounter atomic.Uint64

// seedCronApp creates account + app (the minimum surface for
// CreateCron). Returns the app id so the test can call CreateCron
// directly without walking the full seedFullAccountWithDep surface.
// The label + counter + UnixNano are folded into the email + slug
// so concurrent tests AND cross-invocation reruns (against a
// persisted Postgres) don't collide on the unique index. CI's fresh
// runner per PR doesn't need the timestamp, but local re-runs do.
func seedCronApp(t *testing.T, ctx context.Context, s *state.PgStore, label string) string {
	t.Helper()
	n := cronSeedCounter.Add(1)
	now := time.Now().UnixNano()
	email := fmt.Sprintf("cron-%s-%d-%d@example.com", label, n, now)
	slug := fmt.Sprintf("cron-%s-%d-%d", label, n, now)
	acct, err := s.CreateAccount(ctx, email, api.PlanHobby)
	if err != nil {
		t.Fatalf("CreateAccount: %v", err)
	}
	app, err := s.CreateApp(ctx, state.App{
		AccountID: acct.ID, Slug: slug, Type: state.AppTypeApp,
		RAMMB: 256, MaxConcurrency: 2, IdleTimeoutS: 60,
	})
	if err != nil {
		t.Fatalf("CreateApp: %v", err)
	}
	return app.ID
}

// cronPgStore stands up a fresh schema, migrates it, and returns a
// PgStore. Mirrors pgStore(t) from pgstore_test.go (package-internal,
// not visible from package state_test). Skips when Postgres is
// unreachable.
func cronPgStore(t *testing.T) (*state.PgStore, context.Context) {
	t.Helper()
	pool := pgtest.Open(t)
	ctx := context.Background()
	if err := db.MigrateUp(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return state.NewPgStore(pool), ctx
}

// TestPg_CreateCron_ReturnsCreatedAt is the round-trip regression for
// the 6-column RETURNING clause pgstore.CreateCron now uses. A scan
// count mismatch surfaces here as a fatal error (rows.Scan returns
// "sql: expected N destination arguments, got M") — exactly what the
// sqlc-check drift gate is meant to catch at codegen time.
func TestPg_CreateCron_ReturnsCreatedAt(t *testing.T) {
	s, ctx := cronPgStore(t)
	appID := seedCronApp(t, ctx, s, "create-cron-created-at")

	before := time.Now().UTC().Add(-time.Second) // clock skew buffer
	c, err := s.CreateCron(ctx, appID, "*/10 * * * *", "/healthz", true)
	if err != nil {
		t.Fatalf("CreateCron: %v", err)
	}
	after := time.Now().UTC().Add(time.Second)

	if c.ID == "" || c.AppID != appID || c.Schedule != "*/10 * * * *" || c.Path != "/healthz" || !c.Enabled {
		t.Errorf("CreateCron returned %+v, want populated fields", c)
	}
	// created_at must be set by the DEFAULT now() and lie within the
	// before/after window. A zero value would indicate the column was
	// dropped or the RETURNING clause lost it — the exact bug 00047
	// fixes.
	if c.CreatedAt.IsZero() {
		t.Fatal("CreateCron returned zero CreatedAt — column missing from RETURNING")
	}
	if c.CreatedAt.UTC().Before(before) || c.CreatedAt.UTC().After(after) {
		t.Errorf("CreatedAt = %v, want between %v and %v", c.CreatedAt.UTC(), before, after)
	}
}

// TestPg_CronByID_RoundTripsCreatedAt is the round-trip regression
// for the 6-column SELECT clause pgstore.CronByID now uses. Seeds a
// cron via CreateCron (so the test is independent of seedFullAccount
// changes), then verifies CronByID returns the same id + a non-zero
// CreatedAt.
func TestPg_CronByID_RoundTripsCreatedAt(t *testing.T) {
	s, ctx := cronPgStore(t)
	appID := seedCronApp(t, ctx, s, "cronbyid-roundtrip")

	created, err := s.CreateCron(ctx, appID, "*/5 * * * *", "/healthz", true)
	if err != nil {
		t.Fatalf("CreateCron: %v", err)
	}

	got, err := s.CronByID(ctx, created.ID)
	if err != nil {
		t.Fatalf("CronByID: %v", err)
	}
	if got.ID != created.ID {
		t.Errorf("CronByID id = %q, want %q", got.ID, created.ID)
	}
	if got.Schedule != "*/5 * * * *" || got.Path != "/healthz" || !got.Enabled {
		t.Errorf("CronByID shape = %+v, want schedule=path=enabled round-trip", got)
	}
	if got.CreatedAt.IsZero() {
		t.Error("CronByID returned zero CreatedAt — column missing from SELECT")
	}
	if !got.CreatedAt.Equal(created.CreatedAt) {
		t.Errorf("CronByID CreatedAt = %v, want %v (round-trip mismatch)", got.CreatedAt, created.CreatedAt)
	}
}

// TestPg_UpdateCron_PreservesCreatedAt is the regression for the
// created_at column's coalesce in pgstore.UpdateCron. A future
// contributor who changes the UPDATE to omit created_at (e.g. drops
// the `created_at = coalesce($5, created_at)` clause) breaks
// idempotency — a re-UpdateCron with nil createdAt would no-op the
// column, but the RETURNING still projects it. This test catches the
// "column drift but projection intact" half of the bug.
func TestPg_UpdateCron_PreservesCreatedAt(t *testing.T) {
	s, ctx := cronPgStore(t)
	appID := seedCronApp(t, ctx, s, "updatecron-preserve")

	original, err := s.CreateCron(ctx, appID, "*/5 * * * *", "/healthz", true)
	if err != nil {
		t.Fatalf("CreateCron: %v", err)
	}
	if original.CreatedAt.IsZero() {
		t.Fatal("seeded CreatedAt is zero — column not stamped by 00047")
	}

	// UpdateCron with nil createdAt must not change the column.
	schedule := "*/15 * * * *"
	after, err := s.UpdateCron(ctx, original.ID, &schedule, nil, nil, nil)
	if err != nil {
		t.Fatalf("UpdateCron: %v", err)
	}
	if !after.CreatedAt.Equal(original.CreatedAt) {
		t.Errorf("UpdateCron with nil createdAt changed the column: %v -> %v",
			original.CreatedAt, after.CreatedAt)
	}
	if after.Schedule != "*/15 * * * *" {
		t.Errorf("UpdateCron schedule = %q, want %q", after.Schedule, schedule)
	}
}
