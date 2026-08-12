package eventretention

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
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

// fakeOps is the in-test stand-in for the narrow Ops surface.
// It implements the Ops interface using stub counters / gauges
// that record every .Add / .Set call so the table-driven tests
// below can assert the exact metric contract:
//
//   - AuditEventsDeleted().Add: only called when deleted > 0
//     (an idle pass that prunes nothing does not bump the counter).
//   - AuditEventsRetentionLag().Set: called on EVERY successful
//     pass (so a pinned-zero lag is the canary for "loop running
//     but never deleting").
//
// The mu guards the slices so -race is happy with parallel tests.
type fakeOps struct {
	mu              sync.Mutex
	deletedCalls    []float64
	lagCallsSeconds []float64
}

func (f *fakeOps) AuditEventsDeleted() prometheus.Counter {
	return &fakeCounter{ops: f}
}

func (f *fakeOps) AuditEventsRetentionLag() prometheus.Gauge {
	return &fakeGauge{ops: f}
}

// fakeCounter / fakeGauge are the smallest Prometheus objects
// that satisfy the Counter / Gauge interfaces (both require
// Desc + Collect + Write). We don't register them with a real
// registry; the cleanup loop only calls .Add / .Set and the
// tests assert on those alone.
type fakeCounter struct {
	ops *fakeOps
}

func (c *fakeCounter) Add(v float64) {
	c.ops.mu.Lock()
	defer c.ops.mu.Unlock()
	c.ops.deletedCalls = append(c.ops.deletedCalls, v)
}

func (c *fakeCounter) Inc()                             { c.Add(1) }
func (c *fakeCounter) Desc() *prometheus.Desc           { return nil }
func (c *fakeCounter) Write(*dto.Metric) error          { return nil }
func (c *fakeCounter) Describe(chan<- *prometheus.Desc) {}
func (c *fakeCounter) Collect(chan<- prometheus.Metric) {}

type fakeGauge struct {
	ops *fakeOps
}

func (g *fakeGauge) Set(v float64) {
	g.ops.mu.Lock()
	defer g.ops.mu.Unlock()
	g.ops.lagCallsSeconds = append(g.ops.lagCallsSeconds, v)
}

func (g *fakeGauge) Inc()                             {}
func (g *fakeGauge) Dec()                             {}
func (g *fakeGauge) Add(float64)                      {}
func (g *fakeGauge) Sub(float64)                      {}
func (g *fakeGauge) SetToCurrentTime()                {}
func (g *fakeGauge) Desc() *prometheus.Desc           { return nil }
func (g *fakeGauge) Write(*dto.Metric) error          { return nil }
func (g *fakeGauge) Describe(chan<- *prometheus.Desc) {}
func (g *fakeGauge) Collect(chan<- prometheus.Metric) {}

// Compile-time witness: fakeOps satisfies Ops so the production
// wiring can pass *wire.OpsMetrics and tests can pass *fakeOps
// without an adapter.
var _ Ops = (*fakeOps)(nil)

// TestRunOnce_ObservesMetricsOnSuccess pins the happy-path metric
// contract: a successful RunOnce that deletes 42 rows must fire
// AuditEventsDeleted().Add(42) AND AuditEventsRetentionLag().Set
// (with cutoffDays × 24h as the lag). ADR-091 D20.3 / PR-B residual.
func TestRunOnce_ObservesMetricsOnSuccess(t *testing.T) {
	f := &fakeStore{
		deleteFunc: func(_ time.Time) (int64, error) { return 42, nil },
	}
	ops := &fakeOps{}
	fixedNow := time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)
	c := New(Params{
		Store:      f,
		CutoffDays: 30,
		Now:        func() time.Time { return fixedNow },
		Ops:        ops,
	})

	if _, err := c.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	ops.mu.Lock()
	defer ops.mu.Unlock()
	if got := len(ops.deletedCalls); got != 1 {
		t.Fatalf("AuditEventsDeleted.Add calls = %d, want 1", got)
	}
	if ops.deletedCalls[0] != 42 {
		t.Errorf("AuditEventsDeleted.Add[0] = %v, want 42", ops.deletedCalls[0])
	}
	if got := len(ops.lagCallsSeconds); got != 1 {
		t.Fatalf("AuditEventsRetentionLag.Set calls = %d, want 1", got)
	}
	wantLag := 30 * 24 * time.Hour
	if got := time.Duration(ops.lagCallsSeconds[0] * float64(time.Second)); got != wantLag {
		t.Errorf("AuditEventsRetentionLag.Set[0] = %v, want %v", got, wantLag)
	}
}

// TestRunOnce_ObservesLagButNotDeletedWhenZero pins the asymmetric
// contract: a successful pass that prunes NOTHING still fires
// AuditEventsRetentionLag (so the gauge reflects "the loop
// is alive"), but does NOT fire AuditEventsDeleted (so the
// counter doesn't tick up on idle days). The asymmetry is the
// load-bearing behaviour for the runbook's "is the loop running
// AND is it making progress" question — a pinned-zero gauge plus
// a non-zero counter is healthy; a pinned-zero gauge plus a
// pinned-zero counter is a red flag.
func TestRunOnce_ObservesLagButNotDeletedWhenZero(t *testing.T) {
	f := &fakeStore{
		deleteFunc: func(_ time.Time) (int64, error) { return 0, nil },
	}
	ops := &fakeOps{}
	c := New(Params{Store: f, Ops: ops})

	if _, err := c.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	ops.mu.Lock()
	defer ops.mu.Unlock()
	if got := len(ops.deletedCalls); got != 0 {
		t.Errorf("AuditEventsDeleted calls = %d, want 0 (idle pass must NOT bump the counter)", got)
	}
	if got := len(ops.lagCallsSeconds); got != 1 {
		t.Fatalf("AuditEventsRetentionLag calls = %d, want 1", got)
	}
}

// TestRunOnce_DoesNotObserveOnError pins that BOTH metric calls
// are skipped when DeleteOldEvents returns an error. A failed
// pass must not leave a stale lag value on the gauge (the next
// successful pass overwrites it), and must not bump the delete
// counter (the row count came back as 0 anyway).
func TestRunOnce_DoesNotObserveOnError(t *testing.T) {
	sentinel := errors.New("delete failed")
	f := &fakeStore{
		deleteFunc: func(_ time.Time) (int64, error) { return 0, sentinel },
	}
	ops := &fakeOps{}
	c := New(Params{
		Store: f,
		Log:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		Ops:   ops,
	})

	if _, err := c.RunOnce(context.Background()); !errors.Is(err, sentinel) {
		t.Fatalf("RunOnce err = %v, want %v", err, sentinel)
	}

	ops.mu.Lock()
	defer ops.mu.Unlock()
	if got := len(ops.deletedCalls); got != 0 {
		t.Errorf("AuditEventsDeleted calls = %d, want 0 (failed pass must not bump)", got)
	}
	if got := len(ops.lagCallsSeconds); got != 0 {
		t.Errorf("AuditEventsRetentionLag calls = %d, want 0 (failed pass must not bump)", got)
	}
}

// TestNew_NilOpsDisablesMetricPath pins that an Ops-less Cleanup
// runs without panicking. This is the unit-test default — the
// existing 7 tests above use New(Params{Store: f}) with no Ops
// and must continue to work. The test pins the behaviour so a
// future change that adds a mandatory Ops check breaks the
// existing test suite at the same time.
func TestNew_NilOpsDisablesMetricPath(t *testing.T) {
	f := &fakeStore{
		deleteFunc: func(_ time.Time) (int64, error) { return 7, nil },
	}
	c := New(Params{Store: f}) // Ops nil on purpose

	deleted, err := c.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if deleted != 7 {
		t.Errorf("deleted = %d, want 7", deleted)
	}
}

// typedNilOps is a typed-nil pointer-to-pointer concrete; passing it
// to SetOps exercises the typed-nil footgun guard. The equivalent
// real-world failure mode is cmd/apid passing `srv.ops` (a
// *wire.OpsMetrics) into SetOps when WithOpsMetrics was never
// called — the interface is non-nil but the underlying pointer is
// nil.
type typedNilOps struct{}

func (*typedNilOps) AuditEventsDeleted() prometheus.Counter  { return nil }
func (*typedNilOps) AuditEventsRetentionLag() prometheus.Gauge { return nil }

// TestSetOps_TypedNilDoesNotPin traps the typed-nil interface
// regression: SetOps(&nilPointer) must treat the input as nil so
// RunOnce's `c.ops != nil` check stays accurate. Without the
// guard, the per-call counter / gauge return nil from the typed-
// nil receiver and .Add/.Set panics on nil. PR-B code review
// surfaced this as a latent footgun.
func TestSetOps_TypedNilDoesNotPin(t *testing.T) {
	f := &fakeStore{
		deleteFunc: func(_ time.Time) (int64, error) { return 1, nil },
	}
	c := New(Params{Store: f})

	var pn *typedNilOps // typed nil
	var ops Ops = pn    // non-nil interface wrapping typed-nil pointer

	if ops == nil {
		t.Fatal("pre-condition broken: typed-nil interface unexpectedly nil")
	}
	c.SetOps(ops)

	// RunOnce must NOT panic — and it must NOT increment the
	// underlying counter, since the typed-nil interface was
	// normalised to nil by the guard.
	if _, err := c.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
}

// TestSetOps_RealOpsPinStillWorks is the positive twin: a non-nil
// Ops must still be retained and observed.
func TestSetOps_RealOpsPinStillWorks(t *testing.T) {
	f := &fakeStore{
		deleteFunc: func(_ time.Time) (int64, error) { return 3, nil },
	}
	c := New(Params{Store: f})

	real := &fakeOps{}
	c.SetOps(real)

	if _, err := c.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	real.mu.Lock()
	defer real.mu.Unlock()
	if len(real.deletedCalls) != 1 {
		t.Errorf("deletedCalls = %d, want 1", len(real.deletedCalls))
	}
}

// Compile-time witness: *typedNilOps implements Ops (with nil-
// returning methods) so the typed-nil test above exercises the
// same call-shape production uses.
var _ Ops = (*typedNilOps)(nil)
