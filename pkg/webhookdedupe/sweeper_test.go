package webhookdedupe

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// resetStoreForTest is duplicated from dedupe_test.go because Go
// test files in the same package can call each other's helpers,
// but a single-file dedupe_test.go re-define is also fine. Kept
// inline to avoid cross-test-file coupling.
func resetSweeperStoreForTest(t *testing.T) {
	t.Helper()
	store.Range(func(k, _ any) bool { store.Delete(k); return true })
}

// TestSweeper_RunOnce_RemovesExpiredRows covers the canonical path:
// pre-seed one expired + one unexpired row; RunOnce drops the
// expired one and keeps the unexpired one. Seed bypasses
// CheckReplay (which would stamp the row with now()+TTL) by
// writing directly into the package store.
func TestSweeper_RunOnce_RemovesExpiredRows(t *testing.T) {
	resetSweeperStoreForTest(t)
	now := time.Now()
	// Seed: "expired" has an expires_at in the past (TTL elapsed),
	// "fresh" has an expires_at in the future.
	store.Store(dedupeKey{provider: ProviderGitHub, deliveryID: "expired"}, now.Add(-time.Hour))
	store.Store(dedupeKey{provider: ProviderGitHub, deliveryID: "fresh"}, now.Add(time.Hour))

	sw := &Sweeper{interval: DefaultSweepInterval, now: func() time.Time { return now }}
	if n := sw.RunOnce(); n != 1 {
		t.Errorf("rows deleted = %d, want 1", n)
	}

	// Round-trip: the fresh row is still a replay (sweep didn't
	// touch it); the expired row is now fresh (sweep removed it).
	if err := CheckReplay(context.Background(), ProviderGitHub, "expired"); err != nil {
		t.Errorf("post-sweep, expired should be fresh; err=%v", err)
	}
	if err := CheckReplay(context.Background(), ProviderGitHub, "fresh"); !IsReplay(err) {
		t.Errorf("post-sweep, fresh should still be a replay; err=%v", err)
	}
}

// TestSweeper_Run_StopsOnContextCancel covers the goroutine
// lifecycle: Run blocks on the ticker + ctx.Done, returns
// ctx.Err() on cancellation. Important for the apid shutdown path.
func TestSweeper_Run_StopsOnContextCancel(t *testing.T) {
	resetSweeperStoreForTest(t)
	sw := NewSweeper(50 * time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- sw.Run(ctx) }()

	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("Run returned %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return within 2s of cancel")
	}
}

// TestSweeper_NilSafe covers the nil-pointer contract: a nil
// *Sweeper must not panic. The constructor returns *Sweeper, but
// callers may stash it in a struct field that is nil-initialized
// during early-boot wiring.
func TestSweeper_NilSafe(t *testing.T) {
	var nilSw *Sweeper
	if n := nilSw.RunOnce(); n != 0 {
		t.Errorf("nil Sweeper RunOnce = %d, want 0", n)
	}
	if err := nilSw.Run(context.Background()); err != nil {
		t.Errorf("nil Sweeper Run = %v, want nil", err)
	}
}

// TestNewSweeper_DefaultsInterval covers the constructor's
// "interval <= 0 → default" branch.
func TestNewSweeper_DefaultsInterval(t *testing.T) {
	sw := NewSweeper(0)
	if sw.interval != DefaultSweepInterval {
		t.Errorf("interval = %v, want %v", sw.interval, DefaultSweepInterval)
	}
	sw = NewSweeper(-time.Hour)
	if sw.interval != DefaultSweepInterval {
		t.Errorf("negative interval defaults: got %v, want %v", sw.interval, DefaultSweepInterval)
	}
}

// TestSweeper_ConcurrentRun_RangeSafe covers the sync.Map Range
// loop's thread-safety: the sweep walks the map while concurrent
// CheckReplay calls may be inserting. The race detector under
// `go test -race` is the canonical pin; this test just makes the
// property explicit by overlapping the two for a short window.
func TestSweeper_ConcurrentRun_RangeSafe(t *testing.T) {
	resetSweeperStoreForTest(t)
	sw := NewSweeper(2 * time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = sw.Run(ctx) }()

	var wg sync.WaitGroup
	for i := 0; i < 25; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				_ = CheckReplay(context.Background(), ProviderGitHub, "concurrent-id")
			}
		}(i)
	}
	wg.Wait()
}
