package netstats

import (
	"testing"
	"time"
)

// fixedNow is a monotonically advancing clock for tests. Each
// call to Now returns the next instant in the sequence. Tests
// use it to pin the deltas to a known wall-clock interval so the
// regression / clock-skew branches are exercised against a
// deterministic axis.
type fixedNow struct {
	t  time.Time
	at time.Time
}

func newFixedClock(start time.Time) *fixedNow {
	return &fixedNow{t: start, at: start}
}

// Now satisfies the func() time.Time contract used by Cache.
func (f *fixedNow) Now() time.Time { return f.t }

// Advance moves the clock forward by d and returns the new time.
// Tests call Advance inside Observe's `At` argument so the cache
// sees a controlled wall-clock.
func (f *fixedNow) Advance(d time.Duration) time.Time {
	f.t = f.t.Add(d)
	return f.t
}

func TestObserve_FirstObservationIsBaseline(t *testing.T) {
	clk := newFixedClock(time.Unix(0, 0))
	c := New(clk.Now)
	rd, ok := c.Observe(Observation{InstanceID: "inst-1", RXBytes: 1000})
	if ok {
		t.Errorf("first observation ok = true, want false (baseline not yet established)")
	}
	if rd.Valid {
		t.Errorf("first observation Valid = true, want false")
	}
	if rd.DeltaBytes != 0 {
		t.Errorf("first observation DeltaBytes = %d, want 0", rd.DeltaBytes)
	}
	if rd.CumulativeBytes != 0 {
		t.Errorf("first observation CumulativeBytes = %d, want 0", rd.CumulativeBytes)
	}
}

func TestObserve_SecondObservationComputesDelta(t *testing.T) {
	clk := newFixedClock(time.Unix(0, 0))
	c := New(clk.Now)
	_, _ = c.Observe(Observation{InstanceID: "inst-1", RXBytes: 1000, At: clk.Advance(0)})
	rd, ok := c.Observe(Observation{InstanceID: "inst-1", RXBytes: 1500, At: clk.Advance(250 * time.Millisecond)})
	if !ok {
		t.Fatalf("second observation ok = false, want true")
	}
	if !rd.Valid {
		t.Errorf("second observation Valid = false, want true")
	}
	if rd.DeltaBytes != 500 {
		t.Errorf("DeltaBytes = %d, want 500", rd.DeltaBytes)
	}
	if rd.CumulativeBytes != 500 {
		t.Errorf("CumulativeBytes = %d, want 500", rd.CumulativeBytes)
	}
}

func TestObserve_RegressionDropsBaseline(t *testing.T) {
	clk := newFixedClock(time.Unix(0, 0))
	c := New(clk.Now)
	_, _ = c.Observe(Observation{InstanceID: "inst-1", RXBytes: 5000, At: clk.Advance(0)})
	rd, ok := c.Observe(Observation{InstanceID: "inst-1", RXBytes: 100, At: clk.Advance(250 * time.Millisecond)})
	if ok {
		t.Errorf("regression ok = true, want false (veth recreation → drop baseline)")
	}
	if rd.Valid {
		t.Errorf("regression Valid = true, want false")
	}
	if rd.CumulativeBytes != 0 {
		t.Errorf("regression CumulativeBytes = %d, want 0 (reset on regression)", rd.CumulativeBytes)
	}
}

func TestObserve_NoClockProgressionReturnsInvalid(t *testing.T) {
	clk := newFixedClock(time.Unix(0, 0))
	c := New(clk.Now)
	_, _ = c.Observe(Observation{InstanceID: "inst-1", RXBytes: 1000, At: clk.Advance(0)})
	rd, ok := c.Observe(Observation{InstanceID: "inst-1", RXBytes: 1500, At: clk.Advance(0)})
	if ok {
		t.Errorf("same-instant observation ok = true, want false (clock didn't advance)")
	}
	if rd.Valid {
		t.Errorf("same-instant observation Valid = true, want false")
	}
}

func TestObserve_AdditiveAcrossMultipleTicks(t *testing.T) {
	clk := newFixedClock(time.Unix(0, 0))
	c := New(clk.Now)
	_, _ = c.Observe(Observation{InstanceID: "inst-1", RXBytes: 0, At: clk.Advance(0)})
	rd1, _ := c.Observe(Observation{InstanceID: "inst-1", RXBytes: 1000, At: clk.Advance(250 * time.Millisecond)})
	rd2, _ := c.Observe(Observation{InstanceID: "inst-1", RXBytes: 2500, At: clk.Advance(250 * time.Millisecond)})
	rd3, _ := c.Observe(Observation{InstanceID: "inst-1", RXBytes: 4500, At: clk.Advance(250 * time.Millisecond)})
	if rd1.CumulativeBytes != 1000 {
		t.Errorf("rd1 CumulativeBytes = %d, want 1000", rd1.CumulativeBytes)
	}
	if rd2.CumulativeBytes != 2500 {
		t.Errorf("rd2 CumulativeBytes = %d, want 2500", rd2.CumulativeBytes)
	}
	if rd3.CumulativeBytes != 4500 {
		t.Errorf("rd3 CumulativeBytes = %d, want 4500", rd3.CumulativeBytes)
	}
	if rd1.DeltaBytes != 1000 || rd2.DeltaBytes != 1500 || rd3.DeltaBytes != 2000 {
		t.Errorf("deltas = (%d, %d, %d), want (1000, 1500, 2000)", rd1.DeltaBytes, rd2.DeltaBytes, rd3.DeltaBytes)
	}
}

func TestObserve_EmptyInstanceIDIsRejected(t *testing.T) {
	c := New(time.Now)
	rd, ok := c.Observe(Observation{InstanceID: "", RXBytes: 1000})
	if ok {
		t.Errorf("empty instance id ok = true, want false")
	}
	if rd.Valid {
		t.Errorf("empty instance id Valid = true, want false")
	}
}

func TestObserve_PerInstanceIsolation(t *testing.T) {
	clk := newFixedClock(time.Unix(0, 0))
	c := New(clk.Now)
	_, _ = c.Observe(Observation{InstanceID: "inst-A", RXBytes: 1000, At: clk.Advance(0)})
	// inst-B is independent.
	_, okB := c.Observe(Observation{InstanceID: "inst-B", RXBytes: 5000, At: clk.Advance(0)})
	if okB {
		t.Errorf("inst-B first observation ok = true, want false (baseline not yet established)")
	}
	// inst-A still has its baseline; second observation should compute.
	rdA, okA := c.Observe(Observation{InstanceID: "inst-A", RXBytes: 1500, At: clk.Advance(250 * time.Millisecond)})
	if !okA {
		t.Errorf("inst-A second observation ok = false, want true")
	}
	if rdA.DeltaBytes != 500 {
		t.Errorf("inst-A DeltaBytes = %d, want 500", rdA.DeltaBytes)
	}
	// inst-B's baseline is unaffected.
	rdBLate, _ := c.Observe(Observation{InstanceID: "inst-B", RXBytes: 9000, At: clk.Advance(250 * time.Millisecond)})
	if rdBLate.DeltaBytes != 4000 {
		t.Errorf("inst-B DeltaBytes = %d, want 4000 (independent baseline)", rdBLate.DeltaBytes)
	}
}

func TestObserve_ObservationAtOverridesNow(t *testing.T) {
	clk := newFixedClock(time.Unix(0, 0))
	c := New(clk.Now)
	// First observation does not consult clock (baseline only).
	_, _ = c.Observe(Observation{InstanceID: "inst-1", RXBytes: 0, At: clk.Advance(0)})
	// Second observation's At wins over c.now() — the test source.
	rd, ok := c.Observe(Observation{InstanceID: "inst-1", RXBytes: 100, At: clk.Advance(500 * time.Millisecond)})
	if !ok || rd.DeltaBytes != 100 {
		t.Errorf("Observation.At override failed: ok=%v delta=%d", ok, rd.DeltaBytes)
	}
}

func TestLookup_NoBaselineIsMissing(t *testing.T) {
	c := New(time.Now)
	if _, ok := c.Lookup("never-observed"); ok {
		t.Errorf("Lookup on unobserved instance ok = true, want false")
	}
}

func TestLookup_ReturnsLastReading(t *testing.T) {
	clk := newFixedClock(time.Unix(0, 0))
	c := New(clk.Now)
	_, _ = c.Observe(Observation{InstanceID: "inst-1", RXBytes: 0, At: clk.Advance(0)})
	_, _ = c.Observe(Observation{InstanceID: "inst-1", RXBytes: 750, At: clk.Advance(250 * time.Millisecond)})
	rd, ok := c.Lookup("inst-1")
	if !ok {
		t.Fatalf("Lookup ok = false, want true")
	}
	if rd.DeltaBytes != 750 {
		t.Errorf("Lookup DeltaBytes = %d, want 750", rd.DeltaBytes)
	}
	if rd.CumulativeBytes != 750 {
		t.Errorf("Lookup CumulativeBytes = %d, want 750", rd.CumulativeBytes)
	}
	// Lookup must NOT advance the baseline — calling Observe again
	// with the same rx_bytes must produce a zero delta (no movement
	// since Lookup), not the value Lookup returned.
	rd2, ok2 := c.Observe(Observation{InstanceID: "inst-1", RXBytes: 750, At: clk.Advance(250 * time.Millisecond)})
	if !ok2 {
		t.Fatalf("second same-rx Observe ok = false, want true")
	}
	if rd2.DeltaBytes != 0 {
		t.Errorf("post-Lookup same-rx DeltaBytes = %d, want 0 (Lookup must not advance baseline)", rd2.DeltaBytes)
	}
}

func TestForget_RemovesBaseline(t *testing.T) {
	clk := newFixedClock(time.Unix(0, 0))
	c := New(clk.Now)
	_, _ = c.Observe(Observation{InstanceID: "inst-1", RXBytes: 0, At: clk.Advance(0)})
	if got := c.Size(); got != 1 {
		t.Errorf("Size before Forget = %d, want 1", got)
	}
	c.Forget("inst-1")
	if got := c.Size(); got != 0 {
		t.Errorf("Size after Forget = %d, want 0", got)
	}
	if _, ok := c.Lookup("inst-1"); ok {
		t.Errorf("Lookup after Forget ok = true, want false")
	}
}

func TestReset_ClearsAllBaselines(t *testing.T) {
	clk := newFixedClock(time.Unix(0, 0))
	c := New(clk.Now)
	_, _ = c.Observe(Observation{InstanceID: "a", RXBytes: 0, At: clk.Advance(0)})
	_, _ = c.Observe(Observation{InstanceID: "b", RXBytes: 0, At: clk.Advance(0)})
	if got := c.Size(); got != 2 {
		t.Errorf("Size before Reset = %d, want 2", got)
	}
	c.Reset()
	if got := c.Size(); got != 0 {
		t.Errorf("Size after Reset = %d, want 0", got)
	}
}

func TestObserve_RegressionThenNewBaseline(t *testing.T) {
	clk := newFixedClock(time.Unix(0, 0))
	c := New(clk.Now)
	_, _ = c.Observe(Observation{InstanceID: "inst-1", RXBytes: 0, At: clk.Advance(0)})
	_, _ = c.Observe(Observation{InstanceID: "inst-1", RXBytes: 1000, At: clk.Advance(250 * time.Millisecond)})
	// Regression: veth recreated, rx_bytes drops to 0.
	rd, ok := c.Observe(Observation{InstanceID: "inst-1", RXBytes: 0, At: clk.Advance(250 * time.Millisecond)})
	if ok || rd.Valid {
		t.Errorf("regression row should be invalid (drop baseline)")
	}
	// Next observation: new baseline kicks in; the cumulative
	// counter starts from the post-regression value.
	rd2, ok2 := c.Observe(Observation{InstanceID: "inst-1", RXBytes: 250, At: clk.Advance(250 * time.Millisecond)})
	if !ok2 {
		t.Fatalf("post-regression second observation ok = false, want true")
	}
	if rd2.DeltaBytes != 250 {
		t.Errorf("post-regression DeltaBytes = %d, want 250 (from the new baseline, not the pre-regression cumulative)", rd2.DeltaBytes)
	}
	if rd2.CumulativeBytes != 250 {
		t.Errorf("post-regression CumulativeBytes = %d, want 250 (cumulative resets on regression)", rd2.CumulativeBytes)
	}
}
