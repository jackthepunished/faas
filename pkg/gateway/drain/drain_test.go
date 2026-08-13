// Tests for drain.Tracker. Run with `go test -race ./pkg/gateway/drain/...`
// to catch the data-race surface (atomic counters + sync.WaitGroup
// across many goroutines).

package drain

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Begin returns a closure; calling it decrements the counter.
// Verifies the simple arithmetic + MaxInflight monotonicity.
func TestTracker_BeginDone_Arithmetic(t *testing.T) {
	tr := NewTracker()
	for i := 0; i < 5; i++ {
		_ = tr.Begin("http")
	}
	if got := tr.Inflight(); got != 5 {
		t.Fatalf("after 5 Begin: Inflight = %d, want 5", got)
	}
	if got := tr.MaxInflight(); got != 5 {
		t.Errorf("MaxInflight = %d, want 5", got)
	}
	done := tr.Begin("http")
	done()
	if got := tr.Inflight(); got != 5 {
		t.Fatalf("after 1 Begin+Done: Inflight = %d, want 5", got)
	}
	for i := 0; i < 5; i++ {
		_ = tr.Begin("http")
	}
	if got := tr.Inflight(); got != 10 {
		t.Fatalf("after 10 total Begin: Inflight = %d, want 10", got)
	}
	// MaxInflight is monotonic — must NOT regress as items complete.
	if got := tr.MaxInflight(); got != 10 {
		t.Errorf("MaxInflight = %d, want 10 (monotonic)", got)
	}
}

// Defer-style usage: 1000 Begin/Defer-Done pairs across many
// goroutines. Must end at 0 with no -race findings.
func TestTracker_DeferSymmetry(t *testing.T) {
	tr := NewTracker()
	var wg sync.WaitGroup
	for i := 0; i < 1000; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer tr.Begin("http")()
			// Simulate a quick ServeHTTP.
			time.Sleep(time.Microsecond)
		}()
	}
	wg.Wait()
	if got := tr.Inflight(); got != 0 {
		t.Errorf("after 1000 defer-pairs: Inflight = %d, want 0", got)
	}
}

// Drain with no in-flight returns OutcomeClean immediately. This is
// the cold-boot fast path: cmd/readyz=false, then SIGTERM before
// any traffic; drain must not block.
func TestDrain_FastPath_NoInFlight(t *testing.T) {
	tr := NewTracker()
	start := time.Now()
	outcome, err := tr.Drain(context.Background(), DrainGraceSeconds)
	elapsed := time.Since(start)
	if err != nil {
		t.Errorf("err = %v, want nil", err)
	}
	if outcome != OutcomeClean {
		t.Errorf("outcome = %s, want %s", outcome, OutcomeClean)
	}
	if elapsed > 100*time.Millisecond {
		t.Errorf("fast-path Drain took %v, want <100ms", elapsed)
	}
}

// Drain blocks until every Begin closure fires Done. Two goroutines
// each hold a Begin for 50ms; Drain must not return before both
// release.
func TestDrain_WaitsForCompletion(t *testing.T) {
	tr := NewTracker()
	var wg sync.WaitGroup
	wg.Add(2)
	release := make(chan struct{})

	go func() {
		defer wg.Done()
		done := tr.Begin("http")
		<-release
		done()
	}()
	go func() {
		defer wg.Done()
		done := tr.Begin("http")
		<-release
		done()
	}()

	// Give the goroutines a beat to register their Begin.
	time.Sleep(20 * time.Millisecond)

	if got := tr.Inflight(); got != 2 {
		t.Fatalf("Inflight = %d, want 2", got)
	}

	// outcomeCh is buffered + drained exactly once by the test
	// goroutine; the Drain goroutine writes and exits, so the
	// channel has one writer and one reader without contention.
	type drainResult struct {
		outcome DrainOutcome
		err     error
	}
	resultCh := make(chan drainResult, 1)
	go func() {
		outcome, err := tr.Drain(context.Background(), DrainGraceSeconds)
		resultCh <- drainResult{outcome, err}
	}()

	// Drain must NOT return before we close release. Sleep a bit,
	// confirm it's still blocked.
	select {
	case r := <-resultCh:
		t.Fatalf("Drain returned early with %s; want blocked until release", r.outcome)
	case <-time.After(50 * time.Millisecond):
	}

	close(release)
	wg.Wait()

	var got drainResult
	select {
	case got = <-resultCh:
	case <-time.After(DrainGraceSeconds):
		t.Fatalf("Drain did not return after release")
	}
	if got.outcome != OutcomeClean {
		t.Errorf("outcome = %s, want %s", got.outcome, OutcomeClean)
	}
	if got.err != nil {
		t.Errorf("err = %v, want nil", got.err)
	}

	if got := tr.Inflight(); got != 0 {
		t.Errorf("after drain: Inflight = %d, want 0", got)
	}
}

// Drain with a held goroutine + a deadline shorter than the hold
// must return OutcomeDeadlineExceeded with the right error.
func TestDrain_DeadlineExceeded(t *testing.T) {
	tr := NewTracker()
	release := make(chan struct{})
	defer close(release)

	done := tr.Begin("http")
	defer done()

	outcome, err := tr.Drain(context.Background(), 50*time.Millisecond)
	if outcome != OutcomeDeadlineExceeded {
		t.Errorf("outcome = %s, want %s", outcome, OutcomeDeadlineExceeded)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want context.DeadlineExceeded", err)
	}
	// The held goroutine is still counted (Begin not Done yet).
	// Done is deferred so it cleans up; otherwise the test would
	// leak into other tests via the package-level tracker pool.
}

// Drain with a pre-cancelled ctx returns OutcomeCancelled and the
// ctx.Err. This is the second-SIGTERM path in runDrain.
func TestDrain_CtxCancelled(t *testing.T) {
	tr := NewTracker()
	done := tr.Begin("http")
	defer done()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	outcome, err := tr.Drain(ctx, DrainGraceSeconds)
	if outcome != OutcomeCancelled {
		t.Errorf("outcome = %s, want %s", outcome, OutcomeCancelled)
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}

// 200 goroutines each holding a Begin for a randomised duration.
// Drain (in a separate goroutine) must end with Inflight==0.
// Stress surface for the atomic + WaitGroup combo under -race.
func TestTracker_HighConcurrency(t *testing.T) {
	tr := NewTracker()
	const N = 200
	var wg sync.WaitGroup
	var maxObserved atomic.Int64

	for i := 0; i < N; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			done := tr.Begin("http")
			defer done()
			// Track the highest in-flight we ever saw locally.
			for {
				cur := tr.Inflight()
				if cur <= maxObserved.Load() {
					break
				}
				if maxObserved.CompareAndSwap(maxObserved.Load(), cur) {
					break
				}
			}
			time.Sleep(time.Duration(i%5) * time.Millisecond)
		}()
	}

	outcome, err := tr.Drain(context.Background(), 5*time.Second)
	wg.Wait()

	if err != nil {
		t.Errorf("err = %v, want nil", err)
	}
	if outcome != OutcomeClean {
		t.Errorf("outcome = %s, want %s", outcome, OutcomeClean)
	}
	if got := tr.Inflight(); got != 0 {
		t.Errorf("after drain: Inflight = %d, want 0", got)
	}
	if maxObserved.Load() == 0 {
		t.Errorf("local observer never saw in-flight > 0; test is degenerate")
	}
}

// Drain with deadline=0 must fall back to DrainGraceSeconds. This
// catches a footgun where a caller passes a zero-value Duration.
func TestDrain_ZeroDeadline_UsesDefault(t *testing.T) {
	tr := NewTracker()
	outcome, err := tr.Drain(context.Background(), 0)
	if err != nil {
		t.Errorf("err = %v, want nil", err)
	}
	if outcome != OutcomeClean {
		t.Errorf("outcome = %s, want %s", outcome, OutcomeClean)
	}
}

// Drain with nil ctx must not panic; falls back to Background.
func TestDrain_NilCtx_UsesBackground(t *testing.T) {
	tr := NewTracker()
	outcome, err := tr.Drain(nil, 100*time.Millisecond) //nolint:staticcheck // intentional nil ctx
	if err != nil {
		t.Errorf("err = %v, want nil", err)
	}
	if outcome != OutcomeClean {
		t.Errorf("outcome = %s, want %s", outcome, OutcomeClean)
	}
}

// Compile-time interface assertion: Tracker is the per-request drain
// surface; it is intentionally separate from the per-connection
// InFlightCounter (ConnStateTracker in pkg/gateway/inflight.go) so
// future per-(app, plan) wrappers can compose both.
type drainable interface {
	Drain(ctx context.Context, deadline time.Duration) (DrainOutcome, error)
}

var _ drainable = (*Tracker)(nil)
