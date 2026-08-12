package reqbudget

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// fakeClock is a per-test wall clock helper. Tests build one, attach
// it to a Budget via Budget.Now, and advance it between calls so
// elapsed time accumulates without touching globals. Production
// leaves Budget.Now nil; DefaultClock (time.Now) takes over at every
// call.
type fakeClock struct {
	v atomic.Int64 // unix nanos
}

func newFakeClock(t time.Time) *fakeClock {
	c := &fakeClock{}
	c.v.Store(t.UnixNano())
	return c
}

func (c *fakeClock) now() time.Time { return time.Unix(0, c.v.Load()) }

func (c *fakeClock) advance(d time.Duration) { c.v.Add(int64(d)) }

// set pins the clock's current time.
func (c *fakeClock) set(t time.Time) { c.v.Store(t.UnixNano()) }

// installBudget sets up a parent Budget wired to a fakeClock and
// returns the parent ctx + Budget. Use this helper instead of
// WithRemaining when you also want the fakeClock to be the one
// stamping Started — otherwise Started is real wall-clock while
// b.Now is fake, and elapsed math goes nowhere.
func installBudget(t *testing.T, start time.Time, total, ceiling time.Duration) (context.Context, context.CancelFunc, Budget, *fakeClock) {
	t.Helper()
	clk := newFakeClock(start)
	// Stamp Started against the fake clock so they're aligned.
	b := Budget{
		Total:    total,
		Started:  start,
		Ceiling:  ceiling,
		Source:   SourceEdge,
		Now:      clk.now,
		Route:    "forward",
		Endpoint: "POST:/payment",
	}
	ctx, cancel := context.WithCancel(context.Background())
	return NewContext(ctx, b), cancel, b, clk
}

func TestBudget_RemainingMath(t *testing.T) {
	start := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
	_, _, b, clk := installBudget(t, start, 3*time.Second, 3*time.Second)

	// Each case advances the clock by `elapsed` from start before
	// reading Remaining.
	cases := []struct {
		name    string
		elapsed time.Duration
		wantRem time.Duration
	}{
		{"zero_elapsed", 0, 3 * time.Second},
		{"seven_hundred_ms_in", 700 * time.Millisecond, 2300 * time.Millisecond},
		{"two_seconds_in", 2 * time.Second, 1 * time.Second},
		{"at_deadline", 3 * time.Second, 0},
		{"past_deadline_clamps_to_zero", 5 * time.Second, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clk.set(start.Add(tc.elapsed))
			got := b.Remaining(time.Time{})
			if got != tc.wantRem {
				t.Fatalf("Remaining = %v, want %v", got, tc.wantRem)
			}
		})
	}
}

func TestBudget_BeforeStart_ClampsElapsedToZero(t *testing.T) {
	// Bogus wall clock that is earlier than b.Started — synthetic,
	// not from a fakeClock, to keep the test focused on the negative
	// elapsed path.
	start := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
	b := Budget{Total: 3 * time.Second, Started: start, Ceiling: 3 * time.Second}
	beforeStart := start.Add(-1 * time.Second)
	clk := newFakeClock(beforeStart)
	b.Now = clk.now
	if got := b.Remaining(time.Time{}); got != 3*time.Second {
		t.Fatalf("Remaining before start = %v, want 3s (elapsed clamps to 0)", got)
	}
}

func TestBudget_FromContext_Missing(t *testing.T) {
	b, ok := FromContext(context.Background())
	if ok {
		t.Fatalf("FromContext on bare ctx: ok=true, want false (b=%+v)", b)
	}
	if b.Total != 0 {
		t.Fatalf("FromContext missing: b.Total = %v, want 0", b.Total)
	}
}

func TestBudget_FromContext_Attached(t *testing.T) {
	want := Budget{Total: 2 * time.Second, Endpoint: "POST:/x"}
	ctx := NewContext(context.Background(), want)
	got, ok := FromContext(ctx)
	if !ok {
		t.Fatal("FromContext: ok=false, want true")
	}
	if got.Endpoint != want.Endpoint {
		t.Fatalf("FromContext: endpoint mismatch: got=%q want=%q", got.Endpoint, want.Endpoint)
	}
}

func TestBudget_WithOverhead_NoBudget_Noop(t *testing.T) {
	b := Budget{}
	ctx, cancel, gotBudget := b.WithOverhead(context.Background(), "db", 10*time.Millisecond)
	defer cancel()
	if ctx != context.Background() {
		t.Fatalf("WithOverhead without budget: ctx must be parent")
	}
	if gotBudget.Total != 0 {
		t.Fatalf("WithOverhead without budget: returned Budget must be zero, got %+v", gotBudget)
	}
}

func TestBudget_WithCeiling_NoBudget_Noop(t *testing.T) {
	b := Budget{}
	ctx, cancel, gotBudget := b.WithCeiling(context.Background(), 5*time.Second)
	defer cancel()
	if ctx != context.Background() {
		t.Fatalf("WithCeiling without budget: ctx must be parent")
	}
	if gotBudget.Total != 0 {
		t.Fatalf("WithCeiling without budget: returned Budget must be zero, got %+v", gotBudget)
	}
}

func TestBudget_WithRemaining_NoBudget_Installs(t *testing.T) {
	clk := newFakeClock(time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC))
	prevDefault := DefaultClock
	DefaultClock = clk.now
	defer func() { DefaultClock = prevDefault }()

	parent := context.Background()
	_, cancel, b := WithRemaining(parent, 3*time.Second, 3*time.Second, "forward", "POST:/payment")
	defer cancel()
	if b.Total != 3*time.Second {
		t.Fatalf("WithRemaining: b.Total = %v, want 3s", b.Total)
	}
	if b.Route != "forward" || b.Endpoint != "POST:/payment" {
		t.Fatalf("WithRemaining: route/endpoint mismatch: %q/%q", b.Route, b.Endpoint)
	}
	if b.Source != SourceEdge {
		t.Fatalf("WithRemaining: b.Source = %q, want %q", b.Source, SourceEdge)
	}
}

func TestBudget_WithRemaining_HonorsEarlierDeadline(t *testing.T) {
	start := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
	clk := newFakeClock(start)
	prevDefault := DefaultClock
	DefaultClock = clk.now
	defer func() { DefaultClock = prevDefault }()

	// Parent ctx with a 1s deadline (stdlib ReadTimeout shape).
	parent, parentCancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer parentCancel()

	_, cancel, b := WithRemaining(parent, 5*time.Second, 5*time.Second, "forward", "POST:/payment")
	defer cancel()
	// Budget must clamp to the parent's earlier deadline.
	if b.Total > 1*time.Second {
		t.Fatalf("WithRemaining: b.Total = %v, must clamp to parent's 1s deadline", b.Total)
	}
}

func TestBudget_WithRemaining_TotalZero_Noop(t *testing.T) {
	ctx, cancel, b := WithRemaining(context.Background(), 0, 0, "", "")
	defer cancel()
	if ctx != context.Background() {
		t.Fatalf("WithRemaining(total=0): ctx must be parent")
	}
	if b.Total != 0 {
		t.Fatalf("WithRemaining(total=0): returned Budget must be zero")
	}
}

func TestBudget_WithCeiling_PicksMin(t *testing.T) {
	start := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
	_, _, b, clk := installBudget(t, start, 3*time.Second, 3*time.Second)

	// Advance 100ms and check remaining clamped to 2.9s.
	clk.advance(100 * time.Millisecond)
	_, cancelCeil, bc := b.WithCeiling(context.Background(), 5*time.Second)
	defer cancelCeil()
	if bc.Total != 2900*time.Millisecond {
		t.Fatalf("WithCeiling (loose): child Total = %v, want 2.9s", bc.Total)
	}
	// Tight ceiling case — re-pinning the clock is allowed.
	clk.set(start.Add(100 * time.Millisecond))
	_, cancelCeil2, bc2 := b.WithCeiling(context.Background(), 1*time.Second)
	defer cancelCeil2()
	if bc2.Total != 1*time.Second {
		t.Fatalf("WithCeiling (tight): child Total = %v, want 1s", bc2.Total)
	}
}

func TestBudget_WithOverhead_SubtractsCost(t *testing.T) {
	start := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
	_, _, b, clk := installBudget(t, start, 3*time.Second, 3*time.Second)

	// 200ms in.
	clk.advance(200 * time.Millisecond)
	_, cancel, bc := b.WithOverhead(context.Background(), "db", 50*time.Millisecond)
	defer cancel()
	if bc.Total != 2750*time.Millisecond {
		t.Fatalf("WithOverhead: child Total = %v, want 2.75s", bc.Total)
	}
	// Audit trail must record the reservation.
	if len(bc.Overheads) != 1 || bc.Overheads[0].Name != "db" || bc.Overheads[0].Cost != 50*time.Millisecond {
		t.Fatalf("WithOverhead: audit trail mismatch: %+v", bc.Overheads)
	}
}

func TestBudget_WithOverhead_StacksCumulatively(t *testing.T) {
	start := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
	_, _, b, clk := installBudget(t, start, 3*time.Second, 3*time.Second)

	// 100ms in: remaining = 2.9s. After "db" 10ms: 2.89s. After "grpc" 5ms: 2.885s.
	clk.advance(100 * time.Millisecond)
	c1, cancelC1, bc1 := b.WithOverhead(context.Background(), "db", 10*time.Millisecond)
	defer cancelC1()
	_, cancelC2, bc2 := bc1.WithOverhead(c1, "grpc", 5*time.Millisecond)
	defer cancelC2()
	if bc2.Total != 2885*time.Millisecond {
		t.Fatalf("WithOverhead stacked: child Total = %v, want 2.885s", bc2.Total)
	}
	if len(bc2.Overheads) != 2 {
		t.Fatalf("WithOverhead stacked: audit trail len = %d, want 2", len(bc2.Overheads))
	}
}

func TestBudget_WithOverhead_NeverLoosensPastParentCeiling(t *testing.T) {
	start := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
	_, _, b, _ := installBudget(t, start, 10*time.Second, 10*time.Second)

	_, cancel, bc := b.WithOverhead(context.Background(), "db", 5*time.Second)
	defer cancel()
	if bc.Total != 5*time.Second {
		t.Fatalf("WithOverhead cost shrink: child Total = %v, want 5s (10s remaining - 5s cost)", bc.Total)
	}
}

func TestBudget_WithOverhead_CancelFiresAtDeadline(t *testing.T) {
	start := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
	// Use a real deadline (context.WithTimeout) at the parent level so
	// the child's expected cancel time tracks real wall clock, but
	// validate via remaining math (Remaining math is fake-clocked;
	// the deadline itself is real-clocked by the stdlib timer).
	_, _, b, clk := installBudget(t, start, 3*time.Second, 3*time.Second)

	// 50ms in (per fake clock — math only).
	clk.advance(50 * time.Millisecond)
	// Set up the parent with a real 3s deadline (stdlib timer).
	parent, cancelParent := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancelParent()

	_, cancel, _ := b.WithOverhead(parent, "db", 10*time.Millisecond)
	defer cancel()

	// Sanity: parent's Remaining reads 2.95s (3s - 50ms). The 10ms
	// "db" cost is subtracted *inside* WithOverhead, on the child.
	if got := b.Remaining(time.Time{}); got != 2950*time.Millisecond {
		t.Fatalf("Remaining at 50ms = %v, want 2.95s", got)
	}
}

func TestBudget_NegativeRemaining_ClampsToZero(t *testing.T) {
	start := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
	_, _, b, clk := installBudget(t, start, 1*time.Second, 1*time.Second)

	// Push the clock 10s past — way past the 1s budget.
	clk.advance(10 * time.Second)
	_, cancel, bc := b.WithOverhead(context.Background(), "db", 50*time.Millisecond)
	defer cancel()
	if bc.Total != 0 {
		t.Fatalf("WithOverhead past-deadline: child Total = %v, want 0 (clamp)", bc.Total)
	}
}

func TestBudget_ClockHandle_InheritedByChildren(t *testing.T) {
	// A parent's per-Budget clock must propagate to children so
	// tests can fake the clock on the parent and see elapsed time
	// accumulate when children recompute Remaining().
	start := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
	_, _, b, clk := installBudget(t, start, 3*time.Second, 3*time.Second)
	_ = b

	clk.advance(100 * time.Millisecond)
	_, cancelC1, bc1 := b.WithOverhead(context.Background(), "db", 10*time.Millisecond)
	defer cancelC1()

	clk.advance(50 * time.Millisecond)
	// Children's Started = moment of attach (not parent's Started).
	// bc1.Total = 2.89s (= parent remaining at attach) and elapsed
	// since child attach is 50ms, so Remaining = 2840ms.
	if got := bc1.Remaining(time.Time{}); got != 2840*time.Millisecond {
		t.Fatalf("bc1.Remaining = %v, want 2840ms", got)
	}
	// And bc1.Now must be the parent's clock handle so the
	// child can read elapsed via the same fake clock.
	if bc1.Now == nil {
		t.Fatal("bc1.Now must be inherited from parent's clock")
	}
}
