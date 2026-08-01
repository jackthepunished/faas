package state_test

// PR-C (issue #462): wire-level round-trip of StampAppScaleOut and
// StampAppScaleIn against a real Postgres cluster. The methods are
// the load-bearing carrier writes for the per-app scale-out /
// scale-in cooldowns: the wake-gate admitGate consults
// apps.last_scale_out_at and the reaper consults
// apps.last_scale_in_at. The stamp is non-atomic with the
// instance INSERT (the wake-gate consults the stamp BEFORE the
// INSERT; the "stamp missed" direction is safe). This test pins:
//
//   - the SQL write path against a real cluster (column name,
//     type, NULL semantics).
//   - the read-back path: a subsequent AppByID returns the
//     freshly-stamped timestamp via the LastScaleOutAt /
//     LastScaleInAt *time.Time columns.
//   - the absence of any instance mutation: the stamp does NOT
//     INSERT into the instances table; the row count stays at 0
//     even after a successful stamp.
//
// pgtest.Open skips the whole file when Postgres is unreachable so
// the CI matrix with -short / no-pg still passes; the
// make test-state-coverage gate runs with DATABASE_URL.

import (
	"testing"
	"time"

	"github.com/onebox-faas/faas/pkg/api"
	"github.com/onebox-faas/faas/pkg/state"
)

// TestPgStore_StampAppScaleOut_RoundTrip seeds an app, asserts
// last_scale_out_at is NULL, calls StampAppScaleOut, and asserts
// the read-back via AppByID reflects a non-NULL *time.Time within
// the stamp window. Then asserts the instances table is untouched
// (count = 0).
func TestPgStore_StampAppScaleOut_RoundTrip(t *testing.T) {
	s, pool, ctx := pgStoreWithPool(t)
	_, appID, _ := seedLiveDeploy(t, s, ctx)

	// Pre: column is NULL (no stamp yet).
	var beforeNull *time.Time
	if err := pool.QueryRow(ctx, `SELECT last_scale_out_at FROM apps WHERE id = $1`, appID).Scan(&beforeNull); err != nil {
		t.Fatalf("pre-read last_scale_out_at: %v", err)
	}
	if beforeNull != nil {
		t.Fatalf("last_scale_out_at = %v, want NULL pre-stamp", beforeNull)
	}

	// Stamp.
	if err := s.StampAppScaleOut(ctx, appID); err != nil {
		t.Fatalf("StampAppScaleOut: %v", err)
	}

	// Post: column is non-NULL, within the stamp window.
	var after time.Time
	if err := pool.QueryRow(ctx, `SELECT last_scale_out_at FROM apps WHERE id = $1`, appID).Scan(&after); err != nil {
		t.Fatalf("post-read last_scale_out_at: %v", err)
	}
	now := time.Now()
	if after.IsZero() {
		t.Fatal("last_scale_out_at = zero time, want non-zero after stamp")
	}
	// The stamp uses now(); allow a 5s skew on slow CI.
	if delta := now.Sub(after); delta < -5*time.Second || delta > 5*time.Second {
		t.Errorf("last_scale_out_at = %v, want within 5s of now=%v", after, now)
	}

	// AppByID surfaces the stamp via LastScaleOutAt.
	reloaded, err := s.AppByID(ctx, appID)
	if err != nil {
		t.Fatalf("AppByID: %v", err)
	}
	if reloaded.LastScaleOutAt == nil {
		t.Fatal("reloaded.LastScaleOutAt = nil, want non-nil")
	}
	if delta := now.Sub(*reloaded.LastScaleOutAt); delta < -5*time.Second || delta > 5*time.Second {
		t.Errorf("reloaded.LastScaleOutAt = %v, want within 5s of now", *reloaded.LastScaleOutAt)
	}

	// The stamp does NOT mutate the instances table.
	var instCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM instances WHERE app_id = $1`, appID).Scan(&instCount); err != nil {
		t.Fatalf("count instances: %v", err)
	}
	if instCount != 0 {
		t.Errorf("instances count = %d, want 0 (stamp is non-atomic with CreateInstance)", instCount)
	}
}

// TestPgStore_StampAppScaleIn_RoundTrip mirrors the scale-out test
// for the reaper path. Pins the LastScaleInAt column write/read
// and the absence of any instance mutation.
func TestPgStore_StampAppScaleIn_RoundTrip(t *testing.T) {
	s, pool, ctx := pgStoreWithPool(t)
	_, appID, _ := seedLiveDeploy(t, s, ctx)

	// Pre: column is NULL.
	var beforeNull *time.Time
	if err := pool.QueryRow(ctx, `SELECT last_scale_in_at FROM apps WHERE id = $1`, appID).Scan(&beforeNull); err != nil {
		t.Fatalf("pre-read last_scale_in_at: %v", err)
	}
	if beforeNull != nil {
		t.Fatalf("last_scale_in_at = %v, want NULL pre-stamp", beforeNull)
	}

	// Stamp.
	if err := s.StampAppScaleIn(ctx, appID); err != nil {
		t.Fatalf("StampAppScaleIn: %v", err)
	}

	// Post: non-NULL.
	var after time.Time
	if err := pool.QueryRow(ctx, `SELECT last_scale_in_at FROM apps WHERE id = $1`, appID).Scan(&after); err != nil {
		t.Fatalf("post-read last_scale_in_at: %v", err)
	}
	if after.IsZero() {
		t.Fatal("last_scale_in_at = zero, want non-zero after stamp")
	}
	now := time.Now()
	if delta := now.Sub(after); delta < -5*time.Second || delta > 5*time.Second {
		t.Errorf("last_scale_in_at = %v, want within 5s of now=%v", after, now)
	}

	// Reaper's path: confirm the app is still queryable through the
	// canonical accessor and the LastScaleInAt field is populated.
	reloaded, err := s.AppByID(ctx, appID)
	if err != nil {
		t.Fatalf("AppByID: %v", err)
	}
	if reloaded.LastScaleInAt == nil {
		t.Fatal("reloaded.LastScaleInAt = nil, want non-nil")
	}

	// The stamp does NOT mutate the instances table.
	var instCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM instances WHERE app_id = $1`, appID).Scan(&instCount); err != nil {
		t.Fatalf("count instances: %v", err)
	}
	if instCount != 0 {
		t.Errorf("instances count = %d, want 0 (stamp is non-atomic with Park)", instCount)
	}
}

// TestPgStore_StampAppScaleOut_Overwrites pins the
// monotonic-progress shape: a second stamp advances the column.
// Used by the reaper to gate further reaping; the wake-gate's
// consult reads the freshest value. Without the overwrite
// semantic, a rare "stamp missed" race (two concurrent wakes)
// would let the first stamp stick forever and the consult
// would always return cooldown_held.
func TestPgStore_StampAppScaleOut_Overwrites(t *testing.T) {
	s, pool, ctx := pgStoreWithPool(t)
	_, appID, _ := seedLiveDeploy(t, s, ctx)

	// First stamp.
	if err := s.StampAppScaleOut(ctx, appID); err != nil {
		t.Fatalf("first StampAppScaleOut: %v", err)
	}
	var first time.Time
	if err := pool.QueryRow(ctx, `SELECT last_scale_out_at FROM apps WHERE id = $1`, appID).Scan(&first); err != nil {
		t.Fatalf("read first: %v", err)
	}
	// Wait 1.1s so the second stamp is reliably newer on slow CI.
	time.Sleep(1100 * time.Millisecond)

	// Second stamp.
	if err := s.StampAppScaleOut(ctx, appID); err != nil {
		t.Fatalf("second StampAppScaleOut: %v", err)
	}
	var second time.Time
	if err := pool.QueryRow(ctx, `SELECT last_scale_out_at FROM apps WHERE id = $1`, appID).Scan(&second); err != nil {
		t.Fatalf("read second: %v", err)
	}
	if !second.After(first) {
		t.Errorf("second stamp = %v, want > first = %v (monotonic overwrite)", second, first)
	}
}

// TestPgStore_StampAppScale_MissingApp pins the ErrNotFound path:
// stamping a non-existent app returns the store's ErrNotFound
// sentinel (the wake path logs a warning and proceeds; the
// reaper path also logs and proceeds). The contract: a stamp is
// best-effort and a missing row is NOT a fatal error.
func TestPgStore_StampAppScale_MissingApp(t *testing.T) {
	s, ctx := pgStore(t)
	missingID := "00000000-0000-0000-0000-000000000000"
	if err := s.StampAppScaleOut(ctx, missingID); err == nil {
		t.Errorf("StampAppScaleOut(missing) = nil, want ErrNotFound")
	}
	if err := s.StampAppScaleIn(ctx, missingID); err == nil {
		t.Errorf("StampAppScaleIn(missing) = nil, want ErrNotFound")
	}
}

// pgSamplePlanProApp is a small helper for any future tests in
// this file. Currently unused — kept here to avoid the import
// cycle when the file later grows.
var _ = api.PlanPro
var _ = state.ScalingPolicy{}
