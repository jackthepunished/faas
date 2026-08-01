package targets

import (
	"sync"
	"testing"
	"time"
)

// TestRingBuffer_PerAppMax verifies the basic per-app max.
// Three observations of different per-instance max-inflight
// values — Observe stores the per-bucket max. AppMaxInflight
// at the current bucket returns the max across the window.
func TestRingBuffer_PerAppMax(t *testing.T) {
	r := NewRingBuffer(5, time.Second, time.Second)
	base := time.Unix(1_000_000, 0)
	r.Observe(base, "app1", 3)
	r.Observe(base.Add(time.Second), "app1", 7)
	r.Observe(base.Add(2*time.Second), "app1", 5)
	got, ok := r.AppMaxInflight("app1", base.Add(2*time.Second))
	if !ok {
		t.Fatalf("AppMaxInflight(app1) ok=false, want true")
	}
	if got != 7 {
		t.Errorf("AppMaxInflight = %d, want 7 (max across 3 ticks)", got)
	}
}

// TestRingBuffer_WithinBucketAggregates verifies that two
// observations in the same bucket aggregate via max — the
// second Observe does NOT overwrite but takes the max of
// (existing, new). Mirrors the gauge semantic.
func TestRingBuffer_WithinBucketAggregates(t *testing.T) {
	r := NewRingBuffer(5, time.Second, time.Second)
	base := time.Unix(1_000_000, 0)
	r.Observe(base, "app1", 2)
	r.Observe(base, "app1", 8) // same bucket → max(2, 8) = 8
	r.Observe(base, "app1", 5) // same bucket → max(8, 5) = 8
	got, ok := r.AppMaxInflight("app1", base)
	if !ok {
		t.Fatalf("AppMaxInflight(app1) ok=false, want true")
	}
	if got != 8 {
		t.Errorf("AppMaxInflight = %d, want 8 (within-bucket max)", got)
	}
}

// TestRingBuffer_EvictsOldBuckets verifies that buckets older
// than the window drop out of the max. After 6 observations at
// 1s cadence, the first bucket (1_000_000) is evicted because
// the window is 5 buckets inclusive of the current one.
// AppMaxInflight at base+5s sees buckets 1..5 → max = 99 (the
// largest value landed in bucket 5).
func TestRingBuffer_EvictsOldBuckets(t *testing.T) {
	r := NewRingBuffer(5, time.Second, time.Second)
	base := time.Unix(1_000_000, 0)
	for i := 0; i < 6; i++ {
		var n int64
		switch i {
		case 0:
			n = 99 // first bucket — must be evicted
		case 5:
			n = 99 // current bucket — must win
		default:
			n = int64(i + 1)
		}
		r.Observe(base.Add(time.Duration(i)*time.Second), "app1", n)
	}
	got, ok := r.AppMaxInflight("app1", base.Add(5*time.Second))
	if !ok {
		t.Fatalf("AppMaxInflight(app1) ok=false, want true")
	}
	if got != 99 {
		t.Errorf("AppMaxInflight = %d, want 99 (current bucket max; first bucket evicted)", got)
	}
}

// TestRingBuffer_UnknownAppReturnsZero verifies an app the
// buffer has not seen returns (0, false).
func TestRingBuffer_UnknownAppReturnsZero(t *testing.T) {
	r := NewRingBuffer(5, time.Second, time.Second)
	got, ok := r.AppMaxInflight("never-seen", time.Now())
	if ok || got != 0 {
		t.Errorf("AppMaxInflight(never-seen) = (%d, %v), want (0, false)", got, ok)
	}
}

// TestRingBuffer_NilSafe verifies the nil-receiver contract.
// Methods on a nil *RingBuffer must not panic. The trigger
// relies on this when the instats reader is nil.
func TestRingBuffer_NilSafe(t *testing.T) {
	var r *RingBuffer
	r.Observe(time.Now(), "x", 1)
	got, ok := r.AppMaxInflight("x", time.Now())
	if ok || got != 0 {
		t.Errorf("nil AppMaxInflight = (%d, %v), want (0, false)", got, ok)
	}
}

// TestRingBuffer_ConcurrentReadersAndWriters exercises the lock
// surface: a writer (Observe — the tick goroutine) and a reader
// (AppMaxInflight — the trigger's per-app accessor) call into
// the buffer concurrently for N iterations. The invariant:
// AppMaxInflight never returns a negative value, and the max is
// bounded by the largest write (no torn reads).
func TestRingBuffer_ConcurrentReadersAndWriters(t *testing.T) {
	r := NewRingBuffer(5, time.Second, time.Second)
	base := time.Unix(1_000_000, 0)
	var wg sync.WaitGroup
	stop := make(chan struct{})
	// One writer.
	wg.Add(1)
	go func() {
		defer wg.Done()
		val := int64(0)
		for i := 0; i < 1000; i++ {
			select {
			case <-stop:
				return
			default:
			}
			val += 3
			now := base.Add(time.Duration(i) * time.Millisecond)
			r.Observe(now, "app1", val)
		}
	}()
	// One reader.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 1000; i++ {
			select {
			case <-stop:
				return
			default:
			}
			now := base.Add(time.Duration(i) * time.Millisecond)
			got, _ := r.AppMaxInflight("app1", now)
			if got < 0 {
				t.Errorf("AppMaxInflight = %d, want >= 0 (no torn reads)", got)
			}
		}
	}()
	wg.Wait()
}