// reconnect_test.go — tests for vmmd's shared reconnect helpers
// (ADR-025 axis 5).
//
// Covers the three documented properties:
//
//   1. nextBackoff doubles up to max: 1s → 2s → 4s → 8s →
//      16s → 30s, and 30s + 30s doubles → 30s (cap held).
//
//   2. backoffLadder returns the canonical sequence and the
//      final element equals MaxBackoff. The two helpers are
//      pinned together so a future refactor that breaks one
//      breaks the other loudly.
//
//   3. sleepCtx jitter is bounded [0, jitterBound): a 5000-
//      call sample over a seeded RNG never exceeds the bound;
//      the min is 0 (uniform).
//
// Plus two safety nets that aren't property-pinning but catch
// regressions cheaply:
//
//   4. sleepCtx returns false on ctx cancel: the sleep loop
//      must NOT block past ctx.Done.
//
//   5. The seeded rng seam (setRngForTest) restores the
//      original on teardown — a test that leaks the swap would
//      silently de-randomize every other concurrent test.

package main

import (
	"context"
	"math/rand"
	"sync"
	"testing"
	"time"
)

// TestNextBackoff_Ladder pins the doubling cadence: 1s → 2s →
// 4s → 8s → 16s → 30s (capped, doubled until cap). This is the
// same shape cmd/gatewayd-internal/warmhints.go:103-138 uses today, so the
// vmmd capacity-publisher reconnect stays in sync with the
// gatewayd-internal warmhint-publisher reconnect. The test asserts the
// exact cadence so a future refactor that switches to "5s/10s"
// steps can't silently regress the gatewayd-internal/vmmd coordination.
func TestNextBackoff_Ladder(t *testing.T) {
	t.Parallel()
	steps := []time.Duration{
		1 * time.Second,
		2 * time.Second,
		4 * time.Second,
		8 * time.Second,
		16 * time.Second,
		30 * time.Second, // capped at MaxBackoff
	}
	got := []time.Duration{steps[0]}
	cur := steps[0]
	for i := 1; i < len(steps); i++ {
		cur = nextBackoff(cur, MaxBackoff)
		got = append(got, cur)
	}
	for i := range steps {
		if got[i] != steps[i] {
			t.Errorf("step %d = %v, want %v (full ladder: %v vs %v)",
				i, got[i], steps[i], got, steps)
		}
	}
}

// TestNextBackoff_Capped pins the cap: a cur at MaxBackoff
// stays at MaxBackoff (the doubling is saturated, not
// overflowed). Time.Duration is int64 nanoseconds — without
// the cap, d*2 at 30s would be 60s, then 120s, ..., and a
// malicious use-site could push the sleep into minutes.
func TestNextBackoff_Capped(t *testing.T) {
	t.Parallel()
	if nextBackoff(MaxBackoff, MaxBackoff) != MaxBackoff {
		t.Errorf("nextBackoff(%v, %v) != %v", MaxBackoff, MaxBackoff, MaxBackoff)
	}
	// Doubling one step below must still hit the cap.
	if nextBackoff(20*time.Second, MaxBackoff) != MaxBackoff {
		t.Errorf("nextBackoff(20s, %v) = %v, want %v", MaxBackoff, nextBackoff(20*time.Second, MaxBackoff), MaxBackoff)
	}
	// Doubling at exactly the cap must not overflow or wrap.
	cur := MaxBackoff
	for i := 0; i < 10; i++ {
		cur = nextBackoff(cur, MaxBackoff)
		if cur != MaxBackoff {
			t.Errorf("iteration %d: nextBackoff(%v, %v) = %v, want %v", i, MaxBackoff, MaxBackoff, cur, MaxBackoff)
		}
	}
}

// TestBackoffLadder_Pinned asserts the helper returns the
// canonical sequence and that the final element equals
// MaxBackoff. The two pins are deliberately coupled: a
// regression that breaks backoffLadder must also break the
// caller that trusts MaxBackoff == last step.
func TestBackoffLadder_Pinned(t *testing.T) {
	t.Parallel()
	ladder := backoffLadder()
	want := []time.Duration{
		1 * time.Second,
		2 * time.Second,
		4 * time.Second,
		8 * time.Second,
		16 * time.Second,
		MaxBackoff,
	}
	if len(ladder) != len(want) {
		t.Fatalf("len(ladder) = %d, want %d", len(ladder), len(want))
	}
	for i := range want {
		if ladder[i] != want[i] {
			t.Errorf("ladder[%d] = %v, want %v", i, ladder[i], want[i])
		}
	}
}

// TestSleepCtx_JitterWithinBounds samples 5000 jitterMs
// calls with a seeded RNG and asserts the observed jitter
// is in [0, jitterBound). The lower bound is 0 (uniform);
// the upper bound is the documented cap. We use a
// deterministic seed (42) so the test is reproducible on
// rerun. 5000 samples makes the min/observed sanity check
// deterministic — the failure probability of 0 over 5000
// samples is (499/500)^5000 ≈ 4.5e-5.
//
// The injected RNG is wrapped in a mutex so concurrent
// sleepCtx calls (which the parallel test suite exercises)
// don't race on math/rand's internal state.
func TestSleepCtx_JitterWithinBounds(t *testing.T) {
	// Not t.Parallel: this test mutates the package-level
	// defaultRng via setRngForTest. A parallel sibling would
	// race the swap (PR-1 review).
	seeded := newLockedRng(rand.New(rand.NewSource(42)))
	restore := setRngForTest(seeded)
	defer restore()

	const samples = 5000
	var (
		min time.Duration = 1 << 62 // sentinel
		max time.Duration
	)
	for i := 0; i < samples; i++ {
		// We can't directly observe the jitter applied
		// inside sleepCtx (the timer is opaque), but we
		// can sample jitterMs() directly because it's
		// package-level. The test pins the rng output,
		// not the timer.
		d := jitterMs()
		if d < min {
			min = d
		}
		if d > max {
			max = d
		}
		if d < 0 {
			t.Errorf("sample %d: jitterMs = %v < 0", i, d)
		}
		if d >= jitterBound {
			t.Errorf("sample %d: jitterMs = %v >= jitterBound %v", i, d, jitterBound)
		}
	}
	// Statistical sanity: a uniform [0, jitterBound) sample
	// of 5000 draws must hit 0 (probability 1 - 499/500^5000
	// ≈ 0.99995). If we never see 0, the rng isn't uniform.
	if min != 0 {
		t.Errorf("min = %v, want 0 (rng not uniform over [0, jitterBound))", min)
	}
	// And must approach jitterBound from below. With 5000
	// samples, hitting within 1ms of jitterBound is
	// overwhelmingly likely; flag if not.
	if max < jitterBound-time.Millisecond {
		t.Errorf("max = %v, expected to be near jitterBound %v", max, jitterBound)
	}
}

// TestSleepCtx_CtxCancelReturnsFalse asserts the helper
// returns false when ctx fires before the timer. A regression
// that swapped the select arms would block past ctx cancel
// and the reconnect loop would never exit.
func TestSleepCtx_CtxCancelReturnsFalse(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel before sleep
	if sleepCtx(ctx, 1*time.Hour) {
		t.Error("sleepCtx returned true after ctx cancel; want false")
	}
}

// TestSleepCtx_TimerFiresReturnsTrue asserts the happy path:
// a 1ms sleep with a fresh ctx returns true on timer fire.
func TestSleepCtx_TimerFiresReturnsTrue(t *testing.T) {
	t.Parallel()
	if !sleepCtx(context.Background(), 1*time.Millisecond) {
		t.Error("sleepCtx returned false on a 1ms timer; want true")
	}
}

// TestSetRngForTest_RestoresOnDefer pins the teardown contract:
// a deferred restore() must put the production RNG back. A
// test that forgets the defer would leak the swap; the next
// concurrent test would observe a seeded RNG and silently
// lose entropy.
func TestSetRngForTest_RestoresOnDefer(t *testing.T) {
	// Not t.Parallel: this test mutates the package-level
	// defaultRng and depends on the original being restored
	// before any other test starts.
	original := defaultRng
	seeded := newLockedRng(rand.New(rand.NewSource(1)))
	restore := setRngForTest(seeded)
	if defaultRng == original {
		t.Fatal("setRngForTest did not swap defaultRng")
	}
	restore()
	if defaultRng != original {
		t.Fatal("restore() did not put defaultRng back")
	}
}

// lockedRng wraps a *rand.Rand with a mutex so concurrent
// Int31n calls don't race on math/rand's internal state. Two
// parallel tests using the same seeded RNG via setRngForTest
// would otherwise trip -race.
type lockedRng struct {
	mu sync.Mutex
	r  *rand.Rand
}

func (l *lockedRng) Int31n(n int32) int32 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.r.Int31n(n)
}

func newLockedRng(r *rand.Rand) rng { return &lockedRng{r: r} }
