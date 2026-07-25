package logintoken

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/onebox-faas/faas/pkg/api"
	"github.com/onebox-faas/faas/pkg/state"
)

// TestCleanup_RunOnce_DeletesExpiredTokens pins the contract:
// RunOnce deletes every login_tokens row whose expires_at is past
// the cutoff, leaving rows in the future untouched. The test
// mints two tokens, ages one past the cutoff, and asserts the
// row count.
func TestCleanup_RunOnce_DeletesExpiredTokens(t *testing.T) {
	store := state.NewMemStore()
	ctx := context.Background()

	acct, err := store.CreateAccount(ctx, "alice@example.com", api.PlanFree)
	if err != nil {
		t.Fatalf("seed account: %v", err)
	}

	// Mint two tokens: one already expired, one in the future.
	expiredHash := []byte("sha256-of-token-1")
	liveHash := []byte("sha256-of-token-2")
	if err := store.IssueLoginToken(ctx, expiredHash, acct.ID, time.Now().Add(-time.Hour)); err != nil {
		t.Fatalf("issue expired: %v", err)
	}
	if err := store.IssueLoginToken(ctx, liveHash, acct.ID, time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("issue live: %v", err)
	}

	c := New(Params{
		Store: store,
		Log:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	deleted, err := c.RunOnce(ctx)
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if deleted != 1 {
		t.Errorf("deleted = %d, want 1", deleted)
	}

	// The expired token must be gone; the live token must still
	// resolve.
	if _, err := store.ConsumeLoginToken(ctx, expiredHash); err == nil {
		t.Errorf("expired token still resolves after RunOnce")
	}
	if _, err := store.ConsumeLoginToken(ctx, liveHash); err != nil {
		t.Errorf("live token consumed by RunOnce: %v", err)
	}
}

// TestCleanup_Run_StopsOnContextCancel pins the graceful-shutdown
// contract: Run returns when ctx is cancelled. The first pass runs
// immediately (so a daemon restart catches up); we let the loop
// tick once via a 1ms interval to ensure the goroutine is parked
// on the ctx.Done() branch before cancel.
func TestCleanup_Run_StopsOnContextCancel(t *testing.T) {
	store := state.NewMemStore()
	c := New(Params{
		Store:    store,
		Interval: time.Millisecond,
		Log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		_ = c.Run(ctx)
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return within 2s of ctx cancel")
	}
}

// TestCleanup_New_PanicsOnNilStore pins the init-time guard: a
// nil Store would crash the loop on the first tick; better to fail
// at startup than three hours into a daemon uptime.
func TestCleanup_New_PanicsOnNilStore(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Errorf("New(Params{Store:nil}) did not panic")
		}
	}()
	_ = New(Params{})
}
