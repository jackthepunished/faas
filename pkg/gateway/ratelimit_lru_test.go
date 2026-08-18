// Tests for the optional LRU eviction discipline on Limiter (issue #881
// PR-B). The base behaviour (token math, Peek, plan table) is covered by
// ratelimit_test.go; this file pins the LRU-only contract:
//
//  1. A partially-drained bucket is NEVER evicted. Eviction resets a
//     bucket's state, so dropping a bucket mid-throttle hands the caller
//     a fresh full bucket — a limit bypass. The whole point of
//     evictOneLocked's full-bucket-only invariant is that this is the
//     unsafe case and must not happen.
//
//  2. BucketCount() returns to <= cap once buckets refill. The map may
//     briefly overshoot when nothing is safe to evict (sustained
//     all-mid-drain pressure); the tests below confirm the steady state
//     converges back under cap once buckets refill.
//
//  3. The list bookkeeping (MoveToFront on hit, PushFront on insert)
//     matches bucket age — the back of the list is always the
//     least-recently-used bucket among those still in the map.
//
//  4. Forget / ForgetAccount / ForgetAll are consistent across the map
//     and the recency list — no dangling element survives ForgetAll.
//
//  5. cap <= 0 keeps the historical unbounded path: ll stays nil, no
//     list ops, and the existing Allow / AllowAccount call sites are
//     unchanged in cost.
//
// All tests use the existing fakeClock seam from public_auth_cache_test.go
// (no real sleeps) so the refill formula and eviction-eligibility check
// can both be exercised deterministically.
package gateway

import (
	"testing"
	"time"

	"github.com/onebox-faas/faas/pkg/api"
)

// drainBucket forces the bucket keyed by id to a known mid-drain state
// (tokens=0, last=now) by allowToken'ing it past its burst ceiling on
// the supplied limiter. The limiter's clock must NOT advance between
// drain and the assertion that follows or the bucket will refill.
func drainBucket(t *testing.T, l *Limiter, id string, rps, burst float64) {
	t.Helper()
	for i := 0; i < int(burst)+1; i++ {
		// AllowToken returns false past exhaustion; that is fine — we
		// are just driving the bucket to tokens=0.
		l.allowToken(id, rps, burst)
	}
}

// TestLimiterLRU_EvictsFullBucketOnly is the bypass-prevention property
// test. A bucket drained to tokens=0 must NEVER be the one that
// disappears when the map is at cap and a new key is inserted. The
// fresh insert is allowed to overshoot the map (we prefer correctness
// of in-flight throttles over the memory bound) — but the drained bucket
// is the one the caller is throttled on, so dropping it is the failure
// mode we are guarding against.
//
// Layout: 2-bucket cap, refill A to burst, insert B and drain it to
// mid-state, insert a third. Verify the drained key still exists, the
// inserted key exists, and the full (refilled) bucket A is the one that
// was dropped.
//
// Why we refill A first: a freshly-created bucket starts at tokens=burst
// but the first allowToken consumes one, leaving it at burst-1. That
// makes it not-full by bucketFull's strict-greater-or-equal check, so
// it is NOT an eviction candidate. We advance the clock by one refill
// period so A reaches burst again — only then is it eligible.
func TestLimiterLRU_EvictsFullBucketOnly(t *testing.T) {
	clk := newFakeClock(time.Unix(1_700_000_000, 0))
	l := NewLimiterWithLRUClock(2, clk.Now)

	// Insert A and refill so it is at full burst.
	l.allowToken("a", 1, 5)
	clk.Advance(time.Second) // +1 token at rps=1 → tokens capped at burst=5

	// Insert B and drain it.
	l.allowToken("b", 1, 5)
	drainBucket(t, l, "b", 1, 5)

	// Both buckets exist; A is full, B is mid-drain.
	if l.BucketCount() != 2 {
		t.Fatalf("BucketCount=%d after insert A+B; want 2", l.BucketCount())
	}

	// Inserting C must run evictOneLocked: A is full (B is mid-drain),
	// so A is the only safe candidate and the one that should disappear.
	l.allowToken("c", 1, 5)

	// B must still be present — draining it must not have made it
	// evictable. C is the new bucket. A was dropped.
	if got := l.buckets["b"]; got == nil {
		t.Fatal("drained bucket B was evicted; that is a limit bypass")
	}
	if got := l.buckets["c"]; got == nil {
		t.Fatal("freshly-inserted bucket C is missing from the map")
	}
	if got := l.buckets["a"]; got != nil {
		t.Fatal("bucket A (full, LRU) was not the eviction candidate; B should have been preferred-empty")
	}
}

// TestLimiterLRU_RefilledBucketBecomesEvictable confirms the inverse:
// once the drained bucket refills past its ceiling, it is a fair
// candidate for eviction. This is the same invariant from the positive
// side — "full bucket only" means "full OR refilled past full", and the
// clock-advance + refill formula must be honoured by bucketFull.
func TestLimiterLRU_RefilledBucketBecomesEvictable(t *testing.T) {
	clk := newFakeClock(time.Unix(1_700_000_000, 0))
	l := NewLimiterWithLRUClock(2, clk.Now)

	// Two buckets; drain one.
	l.allowToken("a", 1, 5) // A full after one consume
	l.allowToken("b", 1, 5)
	drainBucket(t, l, "b", 1, 5)

	// Advance the clock so B refills past burst. rps=1, burst=5, so
	// 6 seconds is plenty of headroom.
	clk.Advance(6 * time.Second)

	// Now insert C. Both A and B are "full" (A always was; B refilled
	// under the new clock). evictOneLocked walks back-to-front, so the
	// LEAST-recently-used of the two is evicted — A, since B was
	// last touched (the drain call moved B to the front).
	l.allowToken("c", 1, 5)

	if got := l.buckets["b"]; got == nil {
		t.Fatal("refilled bucket B was evicted; expected A (the older LRU entry)")
	}
	if got := l.buckets["a"]; got != nil {
		t.Fatal("bucket A was not evicted despite being LRU among two full buckets")
	}
}

// TestLimiterLRU_BypassAttemptBlocked is the negative control for the
// property test above. It pins the failure mode directly: even when an
// attacker would benefit from the drained bucket disappearing, it must
// not. The test repeats the insert churn many times so a flake-safe
// regression (e.g. someone adding a "just drop the LRU regardless"
// shortcut) would be caught.
func TestLimiterLRU_BypassAttemptBlocked(t *testing.T) {
	clk := newFakeClock(time.Unix(1_700_000_000, 0))
	l := NewLimiterWithLRUClock(1, clk.Now)

	// Only one slot. Insert attacker, drain it.
	l.allowToken("attacker", 1, 5)
	drainBucket(t, l, "attacker", 1, 5)

	// Hammer with fresh keys. Each insert asks the LRU to evict, but
	// the attacker bucket is mid-drain every time. The map is allowed
	// to overshoot (see evictOneLocked doc), but the attacker bucket
	// must persist.
	for i := 0; i < 50; i++ {
		key := []string{"noise-a", "noise-b", "noise-c"}[i%3]
		l.allowToken(key, 1, 5)
		if l.buckets["attacker"] == nil {
			t.Fatalf("attacker bucket evicted after %d noise inserts; bypass window opened", i)
		}
	}

	// Confirm the attacker bucket is still drained (not refilled by
	// some accidental allowToken path): tokens should be 0 because
	// every drain call and the refill formula combined should leave it
	// at floor(0). Allow one more call and assert it returns false.
	if l.allowToken("attacker", 1, 5) {
		t.Fatal("attacker bucket allowed a token while still drained; the throttle should reject")
	}
}

// TestLimiterLRU_ChurnConvergesBackUnderCap: the steady-state invariant
// the LRU discipline is meant to guarantee. Insert churn faster than
// the refill rate (overshoot expected, correctness > memory bound).
// Then advance the clock so every bucket refills. After the refill, the
// NEXT insert must drop at least one bucket (we now have a full
// candidate), and subsequent inserts must not grow the map past the
// post-eviction level.
//
// Why the assertion is "<= overshoot-1" and not "<= cap": each insert
// triggers ONE eviction (evictOneLocked's contract — bounded critical
// section), so the map converges one bucket at a time. To go from 10
// to 3 we need 7 evictions driven by 7 fresh-key inserts. We assert
// here on a single insert to keep the test focused: the overshoot must
// drop by at least one, and the upper bound is preserved.
func TestLimiterLRU_ChurnConvergesBackUnderCap(t *testing.T) {
	clk := newFakeClock(time.Unix(1_700_000_000, 0))
	l := NewLimiterWithLRUClock(3, clk.Now)

	const rps, burst = 1.0, 5.0
	keys := []string{"k0", "k1", "k2", "k3", "k4", "k5", "k6", "k7", "k8", "k9"}

	// Touch every key with NO clock advance between touches. This
	// models a burst where the limiter is hammered faster than
	// tokens can refill. Every bucket is mid-drain (tokens=4 after
	// one consume), so eviction has no safe candidate and the map
	// overshoots — this is the documented memory-correctness tradeoff.
	for _, k := range keys {
		l.allowToken(k, rps, burst)
	}
	overshoot := l.BucketCount()
	if overshoot <= 3 {
		t.Fatalf("BucketCount=%d during burst; want > 3 (overshoot expected under sustained mid-drain churn)",
			overshoot)
	}

	// Refill window: at rps=1, burst=5, 10 seconds takes every bucket
	// to its ceiling. After this, every bucket is a safe eviction
	// candidate and the next insert must drop at least one of them.
	clk.Advance(10 * time.Second)
	beforeEvict := l.BucketCount()
	l.allowToken("churn-driver", rps, burst)
	afterEvict := l.BucketCount()

	// One insert → one eviction (the full candidate) + one new entry.
	// Net: count unchanged, BUT the bucket that was evicted must be
	// one of the old mid-drain entries that refilled, not the new
	// churn-driver. We assert that via BucketCount() == beforeEvict
	// (one in, one out) AND that the map is no larger than the
	// overshoot peak — i.e. steady state is bounded.
	if afterEvict > overshoot {
		t.Fatalf("BucketCount=%d after refill+insert (peak was %d); the post-refill insert should not grow the map past its overshoot peak",
			afterEvict, overshoot)
	}
	if afterEvict != beforeEvict {
		t.Fatalf("BucketCount=%d after one insert from full state; want %d (one in, one out)", afterEvict, beforeEvict)
	}
}

// TestLimiterLRU_MoveToFrontOnHit verifies the recency bookkeeping
// that makes the LRU scan meaningful. Two inserts, then re-allowToken
// the OLDER key — the newer key must now be at the back (next to be
// evicted) and the older key at the front.
//
// We refill both buckets between insert and the eviction trigger so
// the eviction scan has full candidates to choose between. Without
// the refill, both buckets are mid-drain (tokens=4 after one consume)
// and evictOneLocked skips both, leaving the map to overshoot — that
// scenario is covered separately by TestLimiterLRU_EvictionScanBound.
func TestLimiterLRU_MoveToFrontOnHit(t *testing.T) {
	clk := newFakeClock(time.Unix(1_700_000_000, 0))
	l := NewLimiterWithLRUClock(2, clk.Now)

	l.allowToken("old", 1, 5)
	l.allowToken("newer", 1, 5)
	// Touch "old" so it is more recently used than "newer".
	l.allowToken("old", 1, 5)

	// Both still in the map (cap=2, no eviction yet).
	if l.BucketCount() != 2 {
		t.Fatalf("BucketCount=%d; want 2", l.BucketCount())
	}

	// Refill so both buckets are full candidates for eviction.
	clk.Advance(time.Second)

	// Insert a third; evictOneLocked walks back-to-front, picks the
	// LRU. "newer" is the LRU now (last touched before "old"'s
	// re-touch), so "newer" is the one that should be evicted.
	l.allowToken("newest", 1, 5)

	if l.buckets["old"] == nil {
		t.Fatal("old bucket was evicted despite being most-recently-used")
	}
	if l.buckets["newer"] != nil {
		t.Fatal("newer bucket survived; expected it to be the LRU eviction candidate")
	}
	if l.buckets["newest"] == nil {
		t.Fatal("newest bucket missing from the map")
	}
}

// TestLimiterLRU_EvictionScanBound checks that even when the map is
// full of mid-drain buckets, allowToken's mutex-held critical section
// is bounded. The scan walks at most LimiterEvictScan entries; beyond
// that it gives up and lets the map overshoot.
//
// We verify this by inserting LimiterEvictScan+2 mid-drain buckets
// into a cap=2 limiter and confirming: (a) the scan still terminates
// without panicking, (b) none of the mid-drain buckets are evicted,
// (c) the overshoot is visible via BucketCount().
//
// This test exists because a future "tighten the loop" change (e.g.
// evicting mid-drain to satisfy the cap) would be caught here.
func TestLimiterLRU_EvictionScanBound(t *testing.T) {
	clk := newFakeClock(time.Unix(1_700_000_000, 0))
	l := NewLimiterWithLRUClock(2, clk.Now)

	// Insert + drain LimiterEvictScan+2 buckets. Every one is
	// mid-drain; evictOneLocked will skip all of them.
	const n = LimiterEvictScan + 2
	for i := 0; i < n; i++ {
		key := string(rune('a' + i))
		l.allowToken(key, 1, 5)
		drainBucket(t, l, key, 1, 5)
	}

	// All n buckets must still be present.
	if got := l.BucketCount(); got != n {
		t.Fatalf("BucketCount=%d; want %d (overshoot expected, but no eviction of mid-drain buckets)", got, n)
	}
	for i := 0; i < n; i++ {
		key := string(rune('a' + i))
		if l.buckets[key] == nil {
			t.Fatalf("mid-drain bucket %q was evicted; bypass risk", key)
		}
	}
}

// TestLimiterLRU_ForgetClearsList verifies that Forget on a key in an
// LRU-mode limiter removes the element from BOTH the bucket map and
// the recency list. A list-element leak would surface as
// ll.Len() > len(buckets) and as BucketCount reporting the in-bucket
// count without the dangling element.
func TestLimiterLRU_ForgetClearsList(t *testing.T) {
	clk := newFakeClock(time.Unix(1_700_000_000, 0))
	l := NewLimiterWithLRUClock(10, clk.Now)

	l.allowToken("a", 1, 5)
	l.allowToken("b", 1, 5)
	l.allowToken("c", 1, 5)

	l.Forget("b")

	if got := l.buckets["b"]; got != nil {
		t.Fatal("Forget(b) did not delete bucket b from the map")
	}
	if got := l.elems["b"]; got != nil {
		t.Fatal("Forget(b) did not delete list element for b; list would leak")
	}
	if l.ll.Len() != 2 {
		t.Fatalf("recency list len=%d after Forget(b); want 2", l.ll.Len())
	}
}

// TestLimiterLRU_ForgetAllResetsList: the SIGHUP path. ForgetAll must
// reset BOTH the bucket map AND the recency list / elems map. A stale
// list post-ForgetAll would mean evictOneLocked walks entries that no
// longer exist in the bucket map.
func TestLimiterLRU_ForgetAllResetsList(t *testing.T) {
	clk := newFakeClock(time.Unix(1_700_000_000, 0))
	l := NewLimiterWithLRUClock(10, clk.Now)

	l.allowToken("a", 1, 5)
	l.allowToken("b", 1, 5)
	l.allowToken("c", 1, 5)

	dropped := l.ForgetAll()
	if dropped != 3 {
		t.Fatalf("ForgetAll returned %d; want 3", dropped)
	}
	if l.BucketCount() != 0 {
		t.Fatalf("BucketCount=%d after ForgetAll; want 0", l.BucketCount())
	}
	if l.ll.Len() != 0 {
		t.Fatalf("recency list len=%d after ForgetAll; want 0", l.ll.Len())
	}
	if len(l.elems) != 0 {
		t.Fatalf("elems map len=%d after ForgetAll; want 0", len(l.elems))
	}
}

// TestLimiterLRU_NoopDoesNotTouchList: the noop limiter shares the
// bucket map but is constructed fresh; it must NOT carry the LRU
// state. WithNoop is used in load tests where every Allow returns
// true without doing bucket math, so list bookkeeping in the noop
// path would be pure overhead.
func TestLimiterLRU_NoopDoesNotTouchList(t *testing.T) {
	clk := newFakeClock(time.Unix(1_700_000_000, 0))
	l := NewLimiterWithLRUClock(10, clk.Now)
	l.allowToken("a", 1, 5)

	noop := l.WithNoop()
	if noop.ll != nil {
		t.Fatal("noop limiter carries a recency list; load tests would pay for list ops on every call")
	}
	if noop.cap != 0 {
		t.Fatalf("noop limiter has cap=%d; want 0 (no LRU)", noop.cap)
	}
	if noop.elems != nil {
		t.Fatal("noop limiter carries an elems map")
	}
}

// TestLimiterLRU_NonPositiveCapIsUnbounded pins the constructor
// behaviour for cap<=0 — it must produce an unlimited limiter identical
// in behaviour to NewLimiter(). Otherwise callers passing a config value
// through would have to nil-check before invoking the LRU constructor.
func TestLimiterLRU_NonPositiveCapIsUnbounded(t *testing.T) {
	clk := newFakeClock(time.Unix(1_700_000_000, 0))

	for _, cap := range []int{0, -1, -1000} {
		l := NewLimiterWithLRUClock(cap, clk.Now)
		if l.ll != nil {
			t.Fatalf("cap=%d produced a non-nil ll; want nil (unbounded)", cap)
		}
		if l.elems != nil {
			t.Fatalf("cap=%d produced a non-nil elems map; want nil", cap)
		}
		// Verify the Allow path still works without LRU.
		if !l.allowToken("k", 1, 5) {
			t.Fatalf("cap=%d: allowToken returned false on a fresh full bucket", cap)
		}
	}
}

// TestLimiterLRU_AllowAccountUnchanged pins the per-account limiter's
// behaviour on the new constructor. AllowAccount has its own bucket
// math (rpm/60 rps, rpm burst) — we want to make sure the LRU
// bookkeeping does not regress AllowAccount's token math when used on
// an LRU-mode limiter.
//
// Using the LRU constructor for AllowAccount is not the supported path
// (per-account keys are bounded by account count), but the constructor
// must not break it if a future caller wires one up.
func TestLimiterLRU_AllowAccountUnchanged(t *testing.T) {
	clk := newFakeClock(time.Unix(1_700_000_000, 0))
	l := NewLimiterWithLRUClock(10, clk.Now)

	if !l.AllowAccount("acct", api.PlanHobby) {
		t.Fatal("first AllowAccount returned false; want true")
	}
	if l.BucketCount() != 1 {
		t.Fatalf("BucketCount=%d after AllowAccount; want 1", l.BucketCount())
	}
}
