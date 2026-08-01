// Tests for the issue #517 / PR-B acceptance #4 accessors on
// pkg/fcvm/logbuf.Ring: LowestRetainedSeq and HeadWrittenAt. Both
// are additive on the public API (the ring's other entry points —
// Snapshot / Subscribe — are unchanged) and are read once per
// StreamAppLogs attach on the vmmdgrpc side, so the lock cost is
// microseconds vs. the gRPC dial round-trip.
package logbuf

import (
	"testing"
	"time"
)

// TestLowestRetainedSeq_Empty pins the empty-ring sentinel: an
// idle ring reports 0 (matches Snapshot's "tail from now" sentinel)
// so the vmmdgrpc gap-synthesis branch can use a single test
// (`req.GetSinceSeq() < lowest && lowest > 0`) to distinguish
// "no retained lines yet — silent pass-through" from "the cursor
// fell below the high-water mark — emit a gap frame".
func TestLowestRetainedSeq_Empty(t *testing.T) {
	r := New(1 << 20)
	if got := r.LowestRetainedSeq(); got != 0 {
		t.Errorf("LowestRetainedSeq empty = %d, want 0", got)
	}
}

// TestHeadWrittenAt_Empty pins the empty-ring zero-time return so
// the gap-synthesis branch can hand `time.Time{}` straight to
// timestamppb.New (which yields the proto zero timestamp) without a
// nil check.
func TestHeadWrittenAt_Empty(t *testing.T) {
	r := New(1 << 20)
	if got := r.HeadWrittenAt(); !got.IsZero() {
		t.Errorf("HeadWrittenAt empty = %v, want zero", got)
	}
}

// TestLowestRetainedSeq_AfterWrites verifies the monotonic Seq
// the ring assigns at Write time survives eviction and tracks the
// head slot. After 7 writes, LowestRetainedSeq must report exactly
// 1 (the seq of the oldest retained line) — not nextSeq — and the
// corresponding HeadWrittenAt must equal the time the FIRST write
// committed.
func TestLowestRetainedSeq_AfterWrites(t *testing.T) {
	r := New(1 << 20)
	first := time.Now()
	for i := 0; i < 7; i++ {
		if _, err := r.Write("stdout", []byte("line\n")); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}
	if got := r.LowestRetainedSeq(); got != 1 {
		t.Errorf("LowestRetainedSeq after 7 writes = %d, want 1", got)
	}
	headAt := r.HeadWrittenAt()
	// first was sampled before the Write calls; headAt must be ≥
	// first (the ring's commitLocked uses time.Now() per line so
	// the head sample is at-or-after the pre-write first).
	if headAt.Before(first) {
		t.Errorf("HeadWrittenAt = %v, expected ≥ %v", headAt, first)
	}
}

// TestLowestRetainedSeq_AfterWrapAround is the load-bearing
// property for the gap-frame branch: after the byte budget evicts
// the oldest lines, LowestRetainedSeq must report the seq of the
// NEW oldest retained line. A regression that reported the
// pre-eviction seq would falsely emit a gap frame on every attach
// after a chatty restart.
//
// We pin the byte-budget eviction by writing lines whose total
// payload exceeds 1 KiB and confirming the lowest seq is the
// post-eviction value, not the first write.
func TestLowestRetainedSeq_AfterWrapAround(t *testing.T) {
	r := New(1 << 10) // 1 KiB budget
	// 64 lines × 16 bytes = 1024 bytes — right at the budget edge;
	// push the count higher so the head advances well past seq=1.
	const lines = 200
	for i := 0; i < lines; i++ {
		if _, err := r.Write("stdout", []byte("0123456789abcdef\n")); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}
	lowest := r.LowestRetainedSeq()
	if lowest < 2 {
		t.Fatalf("LowestRetainedSeq after %d writes = %d, want >1 (ring did not evict)", lines, lowest)
	}
	// The ring evicts by bytes (not by count), so the exact lowest
	// depends on the chunk size. Confirm the snapshot agrees:
	// LowestRetainedSeq must equal the lowest seq a Snapshot
	// returns, modulo the sinceSeq filter. Pass the sinceSeq at
	// the lowest bound so Snapshot returns the full surviving
	// buffer (sinceSeq=0 is the "tail from now" sentinel that
	// returns nil — per pkg/fcvm/logbuf.Ring.Snapshot docs).
	snap := r.Snapshot(lowest)
	if len(snap) == 0 {
		t.Fatalf("Snapshot(lowest=%d) empty after %d writes", lowest, lines)
	}
	if snap[0].Seq != lowest {
		t.Errorf("Snapshot[0].Seq = %d, want LowestRetainedSeq=%d", snap[0].Seq, lowest)
	}
	if !r.HeadWrittenAt().Equal(snap[0].WrittenAt) {
		t.Errorf("HeadWrittenAt = %v, want Snapshot[0].WrittenAt = %v",
			r.HeadWrittenAt(), snap[0].WrittenAt)
	}
}

// TestLowestRetainedSeq_RaceWithWrite exercises the r.mu lock under
// the race detector. A goroutine pounds Write and a second goroutine
// reads LowestRetainedSeq / HeadWrittenAt; the test fails only on a
// reported data race (the values themselves may legitimately change
// across reads). The `-race` build tag is set on CI for this path.
func TestLowestRetainedSeq_RaceWithWrite(t *testing.T) {
	r := New(1 << 20)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 5000; i++ {
			_, _ = r.Write("stdout", []byte("tick\n"))
		}
	}()
	for i := 0; i < 5000; i++ {
		_ = r.LowestRetainedSeq()
		_ = r.HeadWrittenAt()
	}
	<-done
}
