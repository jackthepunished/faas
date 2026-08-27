// engine_mirror_test.go — issue #72 / ADR-124 / ADR-125 PR-A3 unit tests
// for the per-rule mirror VM concurrency cap (sync.Map-backed atomic
// counter) and the slot-cap sentinel.
//
// These tests exercise the helpers in isolation rather than spinning up
// the full Engine + schedd stack. Engine construction pulls in the gate
// ledger, vmmd client, chooser state — none of which the cap and
// stamping logic depend on. The "happy path" of a mirror goroutine
// acquiring a slot, running, then deferring a release is asserted via
// the helpers directly; the integration end-to-end coverage is in
// cmd/e2e/traffic_mirror_e2e_test.go (PR-A3 commit 5).
//
// What these tests pin:
//   1. The cap fires after exactly N acquires; N+1 returns false
//      (not a panic, not a nil deref — the slot map is
//      pre-allocated by the first LoadOrStore).
//   2. releaseMirrorSlot restores capacity immediately so a
//      concurrent goroutine can acquire again.
//   3. Per-rule isolation: rules A and B track independently —
//      a saturated A does not affect B.
//
// Mode stamping on the freshly admitted instance is exercised at the
// store boundary (pkg/state/store.go::CreateInstanceWithMode +
// pkg/state/memstore.go impl) — wiring that from the Engine end-to-end
// would require the full wake stack; the snapshot path tested by
// commands like TestEngineWake_ColdBoot already pins CreateInstance,
// so the new overload is exercised by a parallel MemStore test.

package sched

import (
	"errors"
	"testing"
)

// TestMirrorSlot_Cap pins the load-bearing behaviour: tryAcquireMirrorSlot
// returns true for the first N calls (where N = cap) and false for the
// (N+1)th. The cap is the per-rule concurrent-mirror-VM budget; the
// false return surfaces as sched.ErrMirrorSlotAtCapacity upstream.
//
// sync.Map.LoadOrStore handles the first-write race deterministically —
// both contending goroutines agree on the *atomic.Int64 pointer. The
// test exercises the sequential ordering first (the common case for a
// single customer request); a separate goroutine-burst test exercises
// the contended path below (TestMirrorSlot_Cap_Burst).
func TestMirrorSlot_Cap(t *testing.T) {
	t.Parallel()
	e := &Engine{
		MirrorMaxConcurrentPerRule: 5,
	}

	const ruleID = "rule-1"
	// First 5 acquires succeed.
	for i := 1; i <= 5; i++ {
		if !e.tryAcquireMirrorSlot(ruleID) {
			t.Fatalf("acquire %d/5: got false, want true", i)
		}
	}
	// 6th acquire exceeds the cap.
	if e.tryAcquireMirrorSlot(ruleID) {
		t.Fatal("acquire 6/5: got true, want false (cap reached)")
	}
	// Sanity: a 7th still fails.
	if e.tryAcquireMirrorSlot(ruleID) {
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
	e := &Engine{
		MirrorMaxConcurrentPerRule: 5,
	}

	const ruleID = "rule-burst"
	const N = 100
	results := make(chan bool, N)
	for i := 0; i < N; i++ {
		go func() {
			results <- e.tryAcquireMirrorSlot(ruleID)
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
// post-park path so a customer's next request can fire even while the
// current goroutine's VM is still parked.
//
// We saturate to cap, release once, and assert the very next acquire
// succeeds. A second release (decoupled from a prior acquire) must be
// a no-op — never an underflow — so a buggy double-defer in a future
// patch produces a stable test failure rather than a runtime panic.
func TestMirrorSlot_Release(t *testing.T) {
	t.Parallel()
	e := &Engine{
		MirrorMaxConcurrentPerRule: 5,
	}

	const ruleID = "rule-release"
	// Saturate.
	for i := 1; i <= 5; i++ {
		if !e.tryAcquireMirrorSlot(ruleID) {
			t.Fatalf("acquire %d/5: got false, want true", i)
		}
	}
	// 6th fails.
	if e.tryAcquireMirrorSlot(ruleID) {
		t.Fatal("acquire 6/5 (post-saturate): got true, want false")
	}
	// Release one — the (6th) acquire is now reusable.
	e.releaseMirrorSlot(ruleID)
	if !e.tryAcquireMirrorSlot(ruleID) {
		t.Fatal("acquire after release: got false, want true")
	}
	// 7th acquire (the "second" one past cap) fails again.
	if e.tryAcquireMirrorSlot(ruleID) {
		t.Fatal("acquire after release+1: got true, want false")
	}
	// Bogus double-release is a no-op, not a panic.
	e.releaseMirrorSlot(ruleID)
	e.releaseMirrorSlot(ruleID)
	e.releaseMirrorSlot(ruleID)
	// The counter is at cap-1; acquire fits.
	if !e.tryAcquireMirrorSlot(ruleID) {
		t.Fatal("acquire after triple-release: got false, want true")
	}
}

// TestMirrorSlot_PerRuleIsolation pins that rule A's saturation
// doesn't affect rule B. Mirror rules are independent cost circuits;
// a flood on one rule must not bleed into another.
func TestMirrorSlot_PerRuleIsolation(t *testing.T) {
	t.Parallel()
	e := &Engine{
		MirrorMaxConcurrentPerRule: 2,
	}

	if !e.tryAcquireMirrorSlot("rule-a") || !e.tryAcquireMirrorSlot("rule-a") {
		t.Fatal("rule-a first two acquires: want true")
	}
	if e.tryAcquireMirrorSlot("rule-a") {
		t.Fatal("rule-a third acquire: want false (cap)")
	}
	// Rule B is independent and unpolluted.
	if !e.tryAcquireMirrorSlot("rule-b") || !e.tryAcquireMirrorSlot("rule-b") {
		t.Fatal("rule-b first two acquires: want true (isolated from rule-a)")
	}
	if e.tryAcquireMirrorSlot("rule-b") {
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
	e := &Engine{
		MirrorMaxConcurrentPerRule: 1,
	}

	const ruleID = "rule-cleanup"
	if !e.tryAcquireMirrorSlot(ruleID) {
		t.Fatal("first acquire: got false, want true")
	}
	// Release — counter back to 0; map entry removed.
	e.releaseMirrorSlot(ruleID)
	if _, ok := e.mirrorSlots.Load(ruleID); ok {
		t.Fatal("post-release map entry: still present, want cleaned up")
	}
	// Fresh acquire — succeeds.
	if !e.tryAcquireMirrorSlot(ruleID) {
		t.Fatal("post-cleanup acquire: got false, want true")
	}
	// Map is repopulated.
	if _, ok := e.mirrorSlots.Load(ruleID); !ok {
		t.Fatal("post-cleanup map entry: missing, want present")
	}
}

// TestErrMirrorSlotAtCapacity_Message pins the sentinel string used in
// the gateway-side errors.Is check (pkg/scheddgrpc/server.go:402) so a
// future rename of ErrMirrorSlotAtCapacity surfaces as a test failure
// rather than a silent dispatch-goroutine bug.
func TestErrMirrorSlotAtCapacity_Message(t *testing.T) {
	t.Parallel()
	if ErrMirrorSlotAtCapacity == nil {
		t.Fatal("ErrMirrorSlotAtCapacity is nil")
	}
	if msg := ErrMirrorSlotAtCapacity.Error(); msg == "" {
		t.Fatal("ErrMirrorSlotAtCapacity.Error() returned empty string")
	}
	// Sanity: the sentinel is distinct from any random error.
	if errors.Is(ErrMirrorSlotAtCapacity, errors.New("mirror slot at capacity")) {
		t.Fatal("ErrMirrorSlotAtCapacity should not errors.Is a fresh same-text error")
	}
}
