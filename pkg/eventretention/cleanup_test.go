package eventretention

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"
)

// fakeStore is the in-test stand-in for the narrow Store surface —
// only DeleteOldEvents matters for this loop. The fake counts
// calls + stamps a deterministic return so the table-driven tests
// below can assert the cleanup behaviour without spinning up
// Postgres or memstore.
//
// The closure-shared counter is atomic so a parallel test (-race)
// can't trigger a false pass; the cleanup loop itself is single-
// threaded so no other synchronisation is needed.
type fakeStore struct {
	calls      atomic.Int64
	lastCutoff atomic.Int64 // unix nanos
	deleteFunc func(before time.Time) (int64, error)
}

func (f *fakeStore) DeleteOldEvents(_ context.Context, before time.Time) (int64, error) {
	f.calls.Add(1)
	f.lastCutoff.Store(before.UnixNano())
	if f.deleteFunc != nil {
		return f.deleteFunc(before)
	}
	return 0, nil
}

// Compile-time witness that fakeStore implements the narrow
// DeleteOldEventsStore interface — the production wiring passes
// a *state.PgStore (or *state.MemStore) which also implements it.
var _ DeleteOldEventsStore = (*fakeStore)(nil)

// TestNew_PanicsOnNilStore pins the same fail-closed contract as
// pkg/logintoken.New and pkg/grace.New: a nil Store means the loop
// has no useful work, so the constructor refuses to silently
// no-op.
func TestNew_PanicsOnNilStore(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("New with nil Store did not panic")
		}
	}()
	_ = New(Params{Store: nil})
}

// TestNew_DefaultsAreApplied pins that Interval/CutoffDays/Log/Now
// fall back to the documented defaults. A future contributor who
// renames DefaultCutoffDays or 24h would fail this test before
// the change lands.
func TestNew_DefaultsAreApplied(t *testing.T) {
	f := &fakeStore{}
	c := New(Params{Store: f})
	if c.interval != 24*time.Hour {
		t.Errorf("interval = %v, want 24h", c.interval)
	}
	if c.cutoffDays != DefaultCutoffDays {
		t.Errorf("cutoffDays = %d, want %d", c.cutoffDays, DefaultCutoffDays)
	}
	if c.log == nil {
		t.Error("log = nil, want slog.Default()")
	}
	if c.now == nil {
		t.Error("now = nil, want time.Now")
	}
}

// TestRunOnce_CutoffIsNowMinusCutoffDays pins the contract that
// RunOnce computes its cutoff as (now - cutoffDays × 24h) and
// passes that to DeleteOldEvents. Tests inject a fixed Now so
// the assertion is deterministic.
func TestRunOnce_CutoffIsNowMinusCutoffDays(t *testing.T) {
	f := &fakeStore{}
	fixedNow := time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)
	c := New(Params{
		Store:      f,
		CutoffDays: 30,
		Now:        func() time.Time { return fixedNow },
	})

	deleted, err := c.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if deleted != 0 {
		t.Errorf("deleted = %d, want 0", deleted)
	}
	if f.calls.Load() != 1 {
		t.Errorf("calls = %d, want 1", f.calls.Load())
	}
	want := fixedNow.AddDate(0, 0, -30).UnixNano()
	if f.lastCutoff.Load() != want {
		t.Errorf("cutoff = %v, want %v", time.Unix(0, f.lastCutoff.Load()), time.Unix(0, want))
	}
}

// TestRunOnce_PropagatesDeleteCount pins that the row count from
// DeleteOldEvents flows back to RunOnce's caller (this is the
// hook the daemon uses for the apid_audit_events_deleted_total
// counter).
func TestRunOnce_PropagatesDeleteCount(t *testing.T) {
	f := &fakeStore{
		deleteFunc: func(_ time.Time) (int64, error) { return 42, nil },
	}
	c := New(Params{Store: f})

	deleted, err := c.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if deleted != 42 {
		t.Errorf("deleted = %d, want 42", deleted)
	}
}

// TestRunOnce_PropagatesError pins that a DeleteOldEvents error
// bubbles up to the caller. The loop driver (Run) catches the
// first-pass error and continues; RunOnce callers (tests) can
// fail-fast on the same error.
func TestRunOnce_PropagatesError(t *testing.T) {
	sentinel := errors.New("delete failed")
	f := &fakeStore{
		deleteFunc: func(_ time.Time) (int64, error) { return 0, sentinel },
	}
	c := New(Params{Store: f, Log: slog.New(slog.NewTextHandler(io.Discard, nil))})

	deleted, err := c.RunOnce(context.Background())
	if !errors.Is(err, sentinel) {
		t.Errorf("err = %v, want %v", err, sentinel)
	}
	if deleted != 0 {
		t.Errorf("deleted = %d, want 0", deleted)
	}
}

// TestRun_StopsOnCtxCancel pins that Run returns nil when ctx is
// cancelled (graceful shutdown). The ticker goroutine stops on
// the next tick (≤ Interval) — bounded, not immediate.
func TestRun_StopsOnCtxCancel(t *testing.T) {
	f := &fakeStore{}
	c := New(Params{
		Store:    f,
		Interval: 10 * time.Millisecond,
		Log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run err = %v, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return within 2s of ctx cancel")
	}
	// At least one pass ran (the first-pass immediate path) — the
	// ticker goroutine was either never started or stopped cleanly.
	if f.calls.Load() < 1 {
		t.Errorf("calls = %d, want ≥ 1 (first-pass)", f.calls.Load())
	}
}

// TestRun_FirstPassErrorDoesNotCrash pins the defence-in-depth on
// the first pass: a failed DeleteOldEvents logs and continues
// rather than crashing the daemon on bad DB connectivity. The
// next tick retries.
func TestRun_FirstPassErrorDoesNotCrash(t *testing.T) {
	calls := atomic.Int64{}
	sentinel := errors.New("transient")
	store := &countingStore{
		calls: &calls,
		deleteFunc: func(_ time.Time) (int64, error) {
			if calls.Add(1) == 1 {
				return 0, sentinel
			}
			return 0, nil
		},
	}
	c := New(Params{
		Store:    store,
		Interval: 5 * time.Millisecond,
		Log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()

	// Let a couple of ticks run.
	time.Sleep(30 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run err = %v, want nil (first-pass error should not crash)", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return within 2s of ctx cancel")
	}
	if calls.Load() < 2 {
		t.Errorf("calls = %d, want ≥ 2 (first pass failed, next tick retried)", calls.Load())
	}
}

// countingStore is the TestRun_FirstPassErrorDoesNotCrash twin
// of fakeStore — same surface, but it tracks a separate counter
// to make the "first pass failed, retry succeeded" assertion
// deterministic.
type countingStore struct {
	calls      *atomic.Int64
	deleteFunc func(before time.Time) (int64, error)
}

func (c *countingStore) DeleteOldEvents(_ context.Context, before time.Time) (int64, error) {
	return c.deleteFunc(before)
}

// Compile-time witness for the countingStore twin.
var _ DeleteOldEventsStore = (*countingStore)(nil)
