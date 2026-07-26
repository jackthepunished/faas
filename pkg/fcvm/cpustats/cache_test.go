package cpustats

import (
	"sync"
	"testing"
	"time"
)

func TestObserve_FirstSampleIsBaseline(t *testing.T) {
	c := New(func() time.Time { return time.Unix(0, 0) })
	r, ok := c.Observe(Observation{InstanceID: "a", CPUUsageUsec: 1000})
	if ok {
		t.Fatalf("first sample returned ok=true, want false (baseline only): r=%+v", r)
	}
	if r.CPUSeconds != 0 || r.CPUPct != 0 {
		t.Errorf("baseline reading should be zero, got %+v", r)
	}
}

func TestObserve_SecondSampleComputesRate(t *testing.T) {
	now := time.Unix(0, 0)
	c := New(func() time.Time { return now })
	c.Observe(Observation{InstanceID: "a", CPUUsageUsec: 1000, At: now})
	// 250 ms later, +100_000 usec → 100ms of CPU → 40% of one vCPU.
	now = now.Add(250 * time.Millisecond)
	r, ok := c.Observe(Observation{InstanceID: "a", CPUUsageUsec: 101_000, At: now})
	if !ok {
		t.Fatalf("second sample returned ok=false, want true")
	}
	if r.CPUPct <= 0 {
		t.Errorf("CPUPct = %f, want > 0", r.CPUPct)
	}
	// Expect ~40% (100ms of CPU over 250ms wall clock).
	const want = 40.0
	if r.CPUPct < want-1.0 || r.CPUPct > want+1.0 {
		t.Errorf("CPUPct = %f, want ≈ %f (100ms of CPU in 250ms wall clock)", r.CPUPct, want)
	}
	// 100_000 usec delta = 0.1 seconds accumulated.
	if r.CPUSeconds <= 0 || r.CPUSeconds > 0.2 {
		t.Errorf("CPUSeconds = %f, want ≈ 0.1", r.CPUSeconds)
	}
}

func TestObserve_RegressionResetsBaseline(t *testing.T) {
	now := time.Unix(0, 0)
	c := New(func() time.Time { return now })
	c.Observe(Observation{InstanceID: "a", CPUUsageUsec: 100_000, At: now})
	now = now.Add(time.Second)
	c.Observe(Observation{InstanceID: "a", CPUUsageUsec: 150_000, At: now}) // valid delta
	// Regression: usage drops (cgroup was recreated).
	now = now.Add(time.Second)
	r, ok := c.Observe(Observation{InstanceID: "a", CPUUsageUsec: 1000, At: now})
	if ok {
		t.Fatalf("regression returned ok=true, want false (baseline reset): r=%+v", r)
	}
	// Accumulator should be reset to 0 after regression.
	snap, sok := c.Snapshot("a")
	if !sok {
		t.Fatalf("Snapshot lost the post-regression baseline; want it recorded")
	}
	if snap != 0 {
		t.Errorf("post-regression accumSeconds = %f, want 0", snap)
	}
	// Next valid observation after regression should produce a rate.
	now = now.Add(250 * time.Millisecond)
	r, ok = c.Observe(Observation{InstanceID: "a", CPUUsageUsec: 25_000, At: now})
	if !ok {
		t.Fatalf("post-regression sample returned ok=false, want true")
	}
	if r.CPUSeconds < 0 {
		t.Errorf("post-regression CPUSeconds = %f, want >= 0", r.CPUSeconds)
	}
}

func TestObserve_ZeroDeltaIsInvalid(t *testing.T) {
	now := time.Unix(0, 0)
	c := New(func() time.Time { return now })
	c.Observe(Observation{InstanceID: "a", CPUUsageUsec: 1000, At: now})
	// Same instant: degenerate.
	r, ok := c.Observe(Observation{InstanceID: "a", CPUUsageUsec: 1000, At: now})
	if ok {
		t.Fatalf("zero-delta returned ok=true, want false: r=%+v", r)
	}
	if r.Valid {
		t.Errorf("Valid should be false on degenerate (same-instant) sample, got %+v", r)
	}
}

func TestObserve_EmptyInstanceIDIsNoOp(t *testing.T) {
	c := New(nil)
	r, ok := c.Observe(Observation{InstanceID: "", CPUUsageUsec: 1000})
	if ok {
		t.Fatalf("empty instance id returned ok=true")
	}
	if r.CPUSeconds != 0 || r.CPUPct != 0 {
		t.Errorf("empty id reading = %+v, want zero", r)
	}
	if c.Size() != 0 {
		t.Errorf("empty id should not be tracked, got Size=%d", c.Size())
	}
}

func TestObserve_AccumulatorMonotonic(t *testing.T) {
	now := time.Unix(0, 0)
	c := New(func() time.Time { return now })
	c.Observe(Observation{InstanceID: "a", CPUUsageUsec: 0, At: now})
	prev := 0.0
	for i := 1; i <= 5; i++ {
		now = now.Add(time.Second)
		_, ok := c.Observe(Observation{InstanceID: "a", CPUUsageUsec: uint64(i * 1_000_000), At: now})
		if !ok {
			t.Fatalf("sample %d returned ok=false", i)
		}
		snap, _ := c.Snapshot("a")
		if snap < prev {
			t.Errorf("accumulator regressed: prev=%f, current=%f", prev, snap)
		}
		prev = snap
	}
	// After 5 × 1s @ 1 vCPU each, accum should be ~5 seconds.
	if prev < 4.9 || prev > 5.1 {
		t.Errorf("final accumSeconds = %f, want ≈ 5.0", prev)
	}
}

func TestObserve_PctClampedToMaxPct(t *testing.T) {
	now := time.Unix(0, 0)
	c := New(func() time.Time { return now })
	c.Observe(Observation{InstanceID: "a", CPUUsageUsec: 0, At: now})
	// 10 seconds of CPU in 1 wall-clock second → 1000% → clamped to 400.
	now = now.Add(time.Second)
	r, ok := c.Observe(Observation{InstanceID: "a", CPUUsageUsec: 10_000_000, At: now})
	if !ok {
		t.Fatalf("clamp test: ok=false")
	}
	if r.CPUPct > 400.01 {
		t.Errorf("CPUPct = %f, want <= 400 (clamp)", r.CPUPct)
	}
}

func TestForget_RemovesInstance(t *testing.T) {
	c := New(nil)
	c.Observe(Observation{InstanceID: "a", CPUUsageUsec: 1000})
	c.Observe(Observation{InstanceID: "b", CPUUsageUsec: 2000})
	if c.Size() != 2 {
		t.Fatalf("Size = %d, want 2", c.Size())
	}
	c.Forget("a")
	if c.Size() != 1 {
		t.Errorf("Size after Forget = %d, want 1", c.Size())
	}
	// Re-observing a forgotten instance is a fresh baseline.
	_, ok := c.Observe(Observation{InstanceID: "a", CPUUsageUsec: 5000})
	if ok {
		t.Errorf("post-Forget Observe returned ok=true; want baseline reset")
	}
}

func TestReset_WipesAll(t *testing.T) {
	c := New(nil)
	for _, id := range []string{"a", "b", "c"} {
		c.Observe(Observation{InstanceID: id, CPUUsageUsec: 1000})
	}
	if c.Size() != 3 {
		t.Fatalf("pre-Reset Size = %d, want 3", c.Size())
	}
	c.Reset()
	if c.Size() != 0 {
		t.Errorf("post-Reset Size = %d, want 0", c.Size())
	}
}

func TestObserve_Concurrent(t *testing.T) {
	c := New(nil)
	const n = 8
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(id int) {
			defer wg.Done()
			instanceID := "inst-" + string(rune('a'+id%4))
			for j := 0; j < 100; j++ {
				c.Observe(Observation{
					InstanceID:   instanceID,
					CPUUsageUsec: uint64(j * 1000),
				})
			}
		}(i)
	}
	wg.Wait()
	if c.Size() == 0 {
		t.Errorf("Size = 0 after concurrent Observe; expected ≥ 1 instance")
	}
	// No panics, no negative accumulators.
	for i := 0; i < 4; i++ {
		id := "inst-" + string(rune('a'+i))
		if snap, ok := c.Snapshot(id); ok && snap < 0 {
			t.Errorf("instance %s snap = %f, want >= 0", id, snap)
		}
	}
}

func TestObserve_DefaultNowIsUsed(t *testing.T) {
	c := New(nil) // nil now → time.Now
	r, ok := c.Observe(Observation{InstanceID: "a", CPUUsageUsec: 1000})
	if ok {
		t.Errorf("first sample with default clock returned ok=true")
	}
	if r.CPUSeconds != 0 {
		t.Errorf("first sample CPUSeconds = %f, want 0", r.CPUSeconds)
	}
}
