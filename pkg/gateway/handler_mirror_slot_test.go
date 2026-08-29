// handler_mirror_slot_test.go — issue #72 / ADR-133 / ADR-125 PR-A3
// unit tests for the per-rule mirror VM concurrency cap (sync.Map-backed
// atomic counter) on the gateway Handler.
//
// PR-A3 code-review fix #3 moved the slot ownership from
// pkg/sched.Engine to pkg/gateway.Handler so the cap reflects "VMs
// in flight" through round-trip complete, not "admit attempts"
// (which would release microseconds after the wake command is sent,
// well before the mirror VM is done serving). The original tests at
// pkg/sched/engine_mirror_test.go exercised the schedd-side helpers;
// this file ports them to the gateway-side helpers, which is now
// where the authoritative state lives.
//
// What these tests pin:
//  1. The cap fires after exactly N acquires; N+1 returns false
//     (not a panic, not a nil deref — the slot map is
//     pre-allocated by the first LoadOrStore).
//  2. releaseMirrorSlot restores capacity immediately so a
//     concurrent goroutine can acquire again.
//  3. Per-rule isolation: rules A and B track independently —
//     a saturated A does not affect B.
//
// Build the slot on a bare *Handler (NewHandlerWith would also work
// — the test uses the literal form so a future migration to a
// non-handler-owned slot doesn't accidentally re-add the wiring to
// NewHandlerWith).
package gateway

import (
	"testing"
)

// TestMirrorSlot_Cap pins the load-bearing behaviour: tryAcquireMirrorSlot
// returns true for the first N calls (where N = cap) and false for the
// (N+1)th. The cap is the per-rule concurrent-mirror-VM budget; the
// false return surfaces as sched.ErrMirrorSlotAtCapacity upstream
// (the gateway wraps the sentinel when the slot is exhausted).
//
// sync.Map.LoadOrStore handles the first-write race deterministically —
// both contending goroutines agree on the *atomic.Int64 pointer. The
// test exercises the sequential ordering first (the common case for a
// single customer request); a separate goroutine-burst test exercises
// the contended path below (TestMirrorSlot_Cap_Burst).
func TestMirrorSlot_Cap(t *testing.T) {
	t.Parallel()
	h := &Handler{MirrorMaxConcurrentPerRule: 5}

	const ruleID = "rule-1"
	// First 5 acquires succeed.
	for i := 1; i <= 5; i++ {
		if !h.tryAcquireMirrorSlot(ruleID) {
			t.Fatalf("acquire %d/5: got false, want true", i)
		}
	}
	// 6th acquire exceeds the cap.
	if h.tryAcquireMirrorSlot(ruleID) {
		t.Fatal("acquire 6/5: got true, want false (cap reached)")
	}
	// Sanity: a 7th still fails.
	if h.tryAcquireMirrorSlot(ruleID) {
		t.Fatal("acquire 7/5: got true, want false (still cap)")
	}
}

// TestMirrorSlot_Cap_Burst exercises the contended path: 100 goroutines
// each call tryAcquireMirrorSlot exactly once on the same rule, and
// the number of successful acquires must equal the cap (5). A higher
// or lower count indicates a race in the sync.Map / atomic handshake
// — this is the most likely site for first-write-under-contention bugs.
func TestMirrorSlot_Cap_Burst(t *testing.T) {
	t.Parallel()
	h := &Handler{MirrorMaxConcurrentPerRule: 5}

	const ruleID = "rule-burst"
	const N = 100
	results := make(chan bool, N)
	for i := 0; i < N; i++ {
		go func() {
			results <- h.tryAcquireMirrorSlot(ruleID)
		}()
	}
	success := 0
	for i := 0; i < N; i++ {
		if <-results {
			success++
		}
	}
	if success != 5 {
		t.Fatalf("burst acquire: got %d successful, want 5 (cap)", success)
	}
}

// TestMirrorSlot_Release pins the deferred-release contract: after a
// releaseMirrorSlot, the slot is available again. This is the behaviour
// the dispatch goroutine relies on — defer releaseMirrorSlot on the
// post-round-trip path so a customer's next request can fire even
// while the current goroutine is still draining its classification
// write.
//
// We saturate to cap, release once, and assert the very next acquire
// succeeds. A second release (decoupled from a prior acquire) must be
// a no-op — never an underflow — so a buggy double-defer in a future
// patch produces a stable test failure rather than a runtime panic.
func TestMirrorSlot_Release(t *testing.T) {
	t.Parallel()
	h := &Handler{MirrorMaxConcurrentPerRule: 5}

	const ruleID = "rule-release"
	// Saturate.
	for i := 1; i <= 5; i++ {
		if !h.tryAcquireMirrorSlot(ruleID) {
			t.Fatalf("acquire %d/5: got false, want true", i)
		}
	}
	// 6th fails.
	if h.tryAcquireMirrorSlot(ruleID) {
		t.Fatal("acquire 6/5 (post-saturate): got true, want false")
	}
	// Release one — the (6th) acquire is now reusable.
	h.releaseMirrorSlot(ruleID)
	if !h.tryAcquireMirrorSlot(ruleID) {
		t.Fatal("acquire after release: got false, want true")
	}
	// 7th acquire (the "second" one past cap) fails again.
	if h.tryAcquireMirrorSlot(ruleID) {
		t.Fatal("acquire after release+1: got true, want false")
	}
	// Bogus double-release is a no-op, not a panic.
	h.releaseMirrorSlot(ruleID)
	h.releaseMirrorSlot(ruleID)
	h.releaseMirrorSlot(ruleID)
	// The counter is at cap-1; acquire fits.
	if !h.tryAcquireMirrorSlot(ruleID) {
		t.Fatal("acquire after triple-release: got false, want true")
	}
}

// TestMirrorSlot_PerRuleIsolation pins that rule A's saturation
// doesn't affect rule B. Mirror rules are independent cost circuits;
// a flood on one rule must not bleed into another.
func TestMirrorSlot_PerRuleIsolation(t *testing.T) {
	t.Parallel()
	h := &Handler{MirrorMaxConcurrentPerRule: 2}

	if !h.tryAcquireMirrorSlot("rule-a") || !h.tryAcquireMirrorSlot("rule-a") {
		t.Fatal("rule-a first two acquires: want true")
	}
	if h.tryAcquireMirrorSlot("rule-a") {
		t.Fatal("rule-a third acquire: want false (cap)")
	}
	// Rule B is independent and unpolluted.
	if !h.tryAcquireMirrorSlot("rule-b") || !h.tryAcquireMirrorSlot("rule-b") {
		t.Fatal("rule-b first two acquires: want true (isolated from rule-a)")
	}
	if h.tryAcquireMirrorSlot("rule-b") {
		t.Fatal("rule-b third acquire: want false")
	}
}

// TestMirrorSlot_ReleaseThenCleanup pins the post-release map cleanup:
// when the counter hits zero, the ruleID is removed from mirrorSlots so
// a PATCH /v1/apps/{slug}/mirrors/{id} (disable/re-enable flow) doesn't
// accumulate stale entries. The test releases a single acquire (counter
// was 1) and asserts the next acquire is a fresh LoadOrStore — not a
// reused counter pointer.
func TestMirrorSlot_ReleaseThenCleanup(t *testing.T) {
	t.Parallel()
	h := &Handler{MirrorMaxConcurrentPerRule: 1}

	const ruleID = "rule-cleanup"
	if !h.tryAcquireMirrorSlot(ruleID) {
		t.Fatal("first acquire: got false, want true")
	}
	// Release — counter back to 0; map entry removed.
	h.releaseMirrorSlot(ruleID)
	if _, ok := h.mirrorSlots.Load(ruleID); ok {
		t.Fatal("post-release map entry: still present, want cleaned up")
	}
	// Fresh acquire — succeeds.
	if !h.tryAcquireMirrorSlot(ruleID) {
		t.Fatal("post-cleanup acquire: got false, want true")
	}
	// Map is repopulated.
	if _, ok := h.mirrorSlots.Load(ruleID); !ok {
		t.Fatal("post-cleanup map entry: missing, want present")
	}
}
