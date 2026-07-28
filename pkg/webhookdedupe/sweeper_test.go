package webhookdedupe

import (
	"context"
	"log/slog"
	"io"
	"testing"
	"time"

	"github.com/onebox-faas/faas/pkg/state"
)

// TestSweeper_RunOnce_RemovesExpiredRows covers the canonical path:
// pre-seed one expired + one unexpired row; RunOnce drops the
// expired one and keeps the unexpired one. Uses the real MemStore
// so the test exercises the same TTL semantics the production
// PgStore will (MemStore drops expired rows inline at access time
// AND via the explicit sweep).
func TestSweeper_RunOnce_RemovesExpiredRows(t *testing.T) {
	mem := state.NewMemStore()
	ctx := context.Background()
	now := time.Now()

	if err := mem.RecordWebhookDelivery(ctx, ProviderGitHub, "expired", now.Add(-time.Hour)); err != nil {
		t.Fatalf("seed expired: %v", err)
	}
	if err := mem.RecordWebhookDelivery(ctx, ProviderGitHub, "fresh", now.Add(time.Hour)); err != nil {
		t.Fatalf("seed fresh: %v", err)
	}

	sw := NewSweeper(mem, slog.New(slog.NewTextHandler(io.Discard, nil)), DefaultSweepInterval)
	n, err := sw.RunOnce(ctx)
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if n != 1 {
		t.Errorf("rows deleted = %d, want 1", n)
	}

	// Round-trip: the fresh row is still a replay (sweep didn't touch
	// it); the expired row is now fresh (sweep removed it).
	if err := CheckReplay(ctx, mem, ProviderGitHub, "expired"); err != nil {
		t.Errorf("post-sweep, expired should be fresh; err=%v", err)
	}
	if err := CheckReplay(ctx, mem, ProviderGitHub, "fresh"); !IsReplay(err) {
		t.Errorf("post-sweep, fresh should still be a replay; err=%v", err)
	}
}

// TestSweeper_Run_StopsOnContextCancel covers the goroutine
// lifecycle: Run blocks on the ticker + ctx.Done, returns ctx.Err()
// on cancellation. Important for the apid shutdown path.
func TestSweeper_Run_StopsOnContextCancel(t *testing.T) {
	mem := state.NewMemStore()
	sw := NewSweeper(mem, slog.New(slog.NewTextHandler(io.Discard, nil)), 50*time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- sw.Run(ctx) }()

	cancel()
	select {
	case err := <-done:
		if err != context.Canceled {
			t.Errorf("Run returned %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return within 2s of cancel")
	}
}

// TestSweeper_NilStore_NoOp covers the nil-guard contract: a
// misconfigured apid (zero-value sweeper) must not panic. The
// constructor returns *Sweeper so a nil-pointer check at call
// time is the natural seam.
func TestSweeper_NilStore_NoOp(t *testing.T) {
	var nilSw *Sweeper
	if _, err := nilSw.RunOnce(context.Background()); err != nil {
		t.Errorf("nil Sweeper RunOnce = %v, want nil", err)
	}
	if err := nilSw.Run(context.Background()); err != nil {
		t.Errorf("nil Sweeper Run = %v, want nil", err)
	}
}

// TestNewSweeper_DefaultsInterval covers the constructor's
// "interval <= 0 → default" branch.
func TestNewSweeper_DefaultsInterval(t *testing.T) {
	sw := NewSweeper(state.NewMemStore(), nil, 0)
	if sw.interval != DefaultSweepInterval {
		t.Errorf("interval = %v, want %v", sw.interval, DefaultSweepInterval)
	}
	sw = NewSweeper(state.NewMemStore(), nil, -time.Hour)
	if sw.interval != DefaultSweepInterval {
		t.Errorf("negative interval defaults: got %v, want %v", sw.interval, DefaultSweepInterval)
	}
}