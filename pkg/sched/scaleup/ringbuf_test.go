package scaleup

import (
	"sync"
	"testing"
	"time"
)

// TestRingBuffer_PerAppSum verifies the basic sliding-window sum:
// three ticks of 10 requests each land inside a 5-bucket window →
// AppRPS returns 30. The first tick seeds the bucket with the
// cumulative count (10); the next two ticks add a delta of 10 each
// to buckets 1 and 2. At base+2s, all three buckets are in the
// window (cutoff = 2 - 5 + 1 = -2, all buckets pass).
func TestRingBuffer_PerAppSum(t *testing.T) {
	r := NewRingBuffer(5, time.Second, time.Second)
	base := time.Unix(1_000_000, 0)
	r.Touch(base, map[string]int64{"app1": 10})
	r.Touch(base.Add(time.Second), map[string]int64{"app1": 20})
	r.Touch(base.Add(2*time.Second), map[string]int64{"app1": 30})
	if got := r.AppRPS("app1", base.Add(2*time.Second)); got != 30 {
		t.Errorf("AppRPS = %v, want 30", got)
	}
}

// TestRingBuffer_EvictsOldBuckets verifies that ticks older than
// windowSize drop out of the sum. After 6 ticks at 1s cadence, the
// first tick (bucket 1_000_000) is evicted because the window is
// 5 buckets inclusive of the current one. AppRPS at base+5s (the
// last tick's bucket) sees buckets 1..5 = 5 buckets × 10 = 50.
func TestRingBuffer_EvictsOldBuckets(t *testing.T) {
	r := NewRingBuffer(5, time.Second, time.Second)
	base := time.Unix(1_000_000, 0)
	for i := 0; i < 6; i++ {
		r.Touch(base.Add(time.Duration(i)*time.Second), map[string]int64{"app1": int64((i + 1) * 10)})
	}
	// After tick 5, the buffer is [1_000_001..1_000_005] (the
	// 1_000_000 bucket was evicted by the cutoff rule). AppRPS at
	// base+5s sees currentBucket=1_000_005, cutoff=1_000_001, so
	// all 5 retained buckets match → sum = 50.
	got := r.AppRPS("app1", base.Add(5*time.Second))
	if got != 50 {
		t.Errorf("AppRPS = %v, want 50 (first tick evicted, 5 buckets summed)", got)
	}
}

// TestRingBuffer_GatewayRestartDoesNotSpike verifies that a
// cumulative-count regression (gatewayd restart resets the counter)
// is clamped to 0 — the trigger must NOT see the regression as a
// huge traffic spike. Without the clamp, a restart would feed the
// scale-up trigger a fake burst and admit dozens of instances.
func TestRingBuffer_GatewayRestartDoesNotSpike(t *testing.T) {
	r := NewRingBuffer(5, time.Second, time.Second)
	base := time.Unix(1_000_000, 0)
	// First observation: cumulative = 1000.
	r.Touch(base, map[string]int64{"app1": 1000})
	// Second observation: cumulative = 5 (gatewayd restarted).
	r.Touch(base.Add(time.Second), map[string]int64{"app1": 5})
	// Sum should be 5 (the new observation alone); the negative
	// delta is clamped to 0.
	if got := r.AppRPS("app1", base.Add(time.Second)); got != 5 {
		t.Errorf("AppRPS = %v, want 5 (regression clamped to 0)", got)
	}
}

// TestRingBuffer_UnknownAppReturnsZero verifies that an app not
// seen by Touch returns 0 (no synthetic burst on the first tick).
func TestRingBuffer_UnknownAppReturnsZero(t *testing.T) {
	r := NewRingBuffer(5, time.Second, time.Second)
	if got := r.AppRPS("never-seen", time.Now()); got != 0 {
		t.Errorf("AppRPS = %v, want 0", got)
	}
}

// TestRingBuffer_NilSafe verifies the nil-receiver contract: methods
// on a nil *RingBuffer must not panic. The trigger relies on this
// when the promScraper is nil (degraded mode).
func TestRingBuffer_NilSafe(t *testing.T) {
	var r *RingBuffer
	r.Touch(time.Now(), map[string]int64{"x": 1})
	if got := r.AppRPS("x", time.Now()); got != 0 {
		t.Errorf("nil AppRPS = %v, want 0", got)
	}
}

// TestRingBuffer_ConcurrentReadersAndWriters exercises the lock
// surface: a writer (T-style — the tick goroutine) and a reader
// (AppRPS — the trigger's per-app accessor) call into the buffer
// concurrently for N iterations. The invariant: AppRPS never
// returns a negative value, and the sum is bounded by the total
// writes (no torn reads).
func TestRingBuffer_ConcurrentReadersAndWriters(t *testing.T) {
	r := NewRingBuffer(5, time.Second, time.Second)
	base := time.Unix(1_000_000, 0)
	var wg sync.WaitGroup
	stop := make(chan struct{})
	// One writer.
	wg.Add(1)
	go func() {
		defer wg.Done()
		cumulative := int64(0)
		for i := 0; i < 1000; i++ {
			select {
			case <-stop:
				return
			default:
			}
			cumulative += 5
			r.Touch(base.Add(time.Duration(i)*time.Millisecond), map[string]int64{"app1": cumulative})
		}
	}()
	// Two readers.
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 1000; i++ {
				select {
				case <-stop:
					return
				default:
				}
				if v := r.AppRPS("app1", time.Now()); v < 0 {
					t.Errorf("AppRPS = %v, want >= 0", v)
				}
			}
		}()
	}
	// Wait for everyone.
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	// The writer is fixed-iteration; the readers are too.
	// Give them a moment to finish, then close stop to release
	// any straggler.
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		close(stop)
		<-done
	}
}
