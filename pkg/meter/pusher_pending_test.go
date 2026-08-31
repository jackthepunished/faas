package meter_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/onebox-faas/faas/pkg/api"
	"github.com/onebox-faas/faas/pkg/meter"
	"github.com/onebox-faas/faas/pkg/state"
)

func TestPushPendingRetriesFailedWindowsFromDurableUsage(t *testing.T) {
	ctx := context.Background()
	store := state.NewMemStore()
	acct := makeAccount(t, ctx, store, api.PlanHobby)
	t0 := time.Date(2026, 8, 31, 10, 0, 0, 0, time.UTC)
	if err := store.AppendUsage(ctx, acct.ID, "app-a", "instance-a", t0.Add(5*time.Minute), 100, 0, 0, 0, 0, 0, 0, 0); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendUsage(ctx, acct.ID, "app-a", "instance-a", t0.Add(65*time.Minute), 200, 0, 0, 0, 0, 0, 0, 0); err != nil {
		t.Fatal(err)
	}

	retryErr := errors.New("temporary provider outage")
	provider := &recordingStripe{err: retryErr}
	now := t0.Add(2 * time.Hour)
	pusher := meter.NewPusher(store, provider, discardLog(), func() time.Time { return now }, nil)
	if pushed, err := pusher.PushPending(ctx, 30*24*time.Hour); pushed != 0 || !errors.Is(err, retryErr) {
		t.Fatalf("first PushPending = (%d, %v), want (0, temporary error)", pushed, err)
	}

	provider.err = nil
	pushed, err := pusher.PushPending(ctx, 30*24*time.Hour)
	if err != nil || pushed != 2 {
		t.Fatalf("retry PushPending = (%d, %v), want (2, nil)", pushed, err)
	}
	if got := len(provider.Calls()); got != 4 {
		t.Fatalf("provider calls = %d, want 4 (two failed + two replayed windows)", got)
	}
}
