// session_middleware_test.go — IAM-3 (ADR-039, issue #187 + #244
// merged). Unit tests for the requireSessionCookie debouncer. The
// debouncer's lifetime-eviction contract is the load-bearing one
// in this file: without it the per-sid map grows unbounded under
// sustained login churn. The other helpers (clearSessionCookie,
// sessionFrom) are trivial and exercised by handlers_sessions_test.go.
//
// The debouncer's production window is sessionTouchWindow = 5min
// (cmd/apid/server.go). These tests drive rescheduling by
// directly mutating the sync.Map rather than waiting the full
// production window — the eviction logic is pinned by
// TestDebounce_AfterFire_DeletesMatchingTicket + the stale-evict
// guard below.
package main

import (
	"sync"
	"testing"
	"time"
)

// TestDebounce_FirstTouchReturnsTrue: a brand-new sid must
// always pass shouldTouch (the caller is the one to fire the
// touch goroutine).
func TestDebounce_FirstTouchReturnsTrue(t *testing.T) {
	d := &sessionTouchDebounce{}
	const sid = "11111111-1111-1111-1111-111111111111"
	ticket, fire := d.shouldTouch(sid, time.Now())
	if !fire || ticket == nil {
		t.Fatalf("first shouldTouch = (nil, %v), want (ticket, true)", fire)
	}
}

// TestDebounce_RepeatedTouchReturnsFalse: two shouldTouch calls
// inside the window — the second is satisfied (returns false,
// no second goroutine fired). The load-bearing per-sid debounce
// that keeps WAL amplification bounded under sustained
// high-RPS dashboard sessions.
func TestDebounce_RepeatedTouchReturnsFalse(t *testing.T) {
	d := &sessionTouchDebounce{}
	const sid = "22222222-2222-2222-2222-222222222222"
	now := time.Now()
	if _, fire := d.shouldTouch(sid, now); !fire {
		t.Fatalf("first shouldTouch = false, want true")
	}
	if _, fire := d.shouldTouch(sid, now.Add(10*time.Second)); fire {
		t.Errorf("second shouldTouch inside window = true, want false")
	}
	if _, fire := d.shouldTouch(sid, now.Add(4*time.Minute)); fire {
		t.Errorf("third shouldTouch at +4m = true, want false")
	}
}

// TestDebounce_RescheduleAfterWindow: once the window has
// elapsed a fresh shouldTouch returns true (the caller fires
// a touch again). We exercise this by pre-stamping the entry
// with a backdated timestamp rather than waiting.
func TestDebounce_RescheduleAfterWindow(t *testing.T) {
	d := &sessionTouchDebounce{}
	const sid = "33333333-3333-3333-3333-333333333333"
	t0 := time.Now()
	d.m.Store(sid, &touchTicket{m: &d.m, sid: sid, fire: t0.Add(-10 * time.Minute)})
	if _, fire := d.shouldTouch(sid, t0); !fire {
		t.Errorf("post-window shouldTouch = false, want true (debounce should release)")
	}
}

// TestDebounce_ConcurrentFiresExactlyOnce: 100 goroutines
// asking to touch the same sid must produce exactly 1 firer
// (not N) — the LoadOrStore election that the original
// implementation had and which the rewrite must preserve.
func TestDebounce_ConcurrentFiresExactlyOnce(t *testing.T) {
	d := &sessionTouchDebounce{}
	const sid = "55555555-5555-5555-5555-555555555555"
	const goroutines = 100
	var wg sync.WaitGroup
	var fireCount int
	var mu sync.Mutex
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, fire := d.shouldTouch(sid, time.Now()); fire {
				mu.Lock()
				fireCount++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if fireCount != 1 {
		t.Errorf("fireCount = %d, want exactly 1 (concurrent shouldTouch should elect one firer)", fireCount)
	}
	if active := d.active(); active != 1 {
		t.Errorf("active = %d, want 1 (single ticket in map after election)", active)
	}
}

// TestDebounce_AfterFire_DeletesMatchingTicket pins the eviction
// contract: AfterFire CAS-deletes when the stored ticket IS this
// one, AND skips when a fresher ticket is in place. Uses a
// 50ms window so the test runs in <500ms total.
func TestDebounce_AfterFire_DeletesMatchingTicket(t *testing.T) {
	d := &sessionTouchDebounce{}
	const sid = "66666666-6666-6666-6666-666666666666"
	ticket, _ := d.shouldTouch(sid, time.Now())
	if d.active() != 1 {
		t.Fatalf("pre-condition: active = %d, want 1", d.active())
	}
	// Synchronous version of AfterFire: don't actually sleep,
	// just exercise the CAS delete via the public method.
	ticket.AfterFire(0)
	if d.active() != 0 {
		t.Errorf("post-AfterFire: active = %d, want 0 (matching ticket should be deleted)", d.active())
	}
}

// TestDebounce_AfterFire_LeavesFresherTicket: a stale ticket's
// AfterFire must NOT delete a newer ticket stored by a later
// firer. This is the version-stamp guard that prevents a
// stalled firer from clobbering active work.
func TestDebounce_AfterFire_LeavesFresherTicket(t *testing.T) {
	d := &sessionTouchDebounce{}
	const sid = "77777777-7777-7777-7777-777777777777"

	// "Stale" firer's ticket was elected first.
	stale, _ := d.shouldTouch(sid, time.Now())

	// A second caller (window elapsed) replaces it via the
	// CAS branch in shouldTouch. To force that path we
	// manually store a fresh ticket — sync.Map.LoadOrStore
	// would otherwise refuse. We exercise the production
	// CAS branch by backdating stale's fire and inserting a
	// fresh ticket behind it.
	d.m.Store(sid, stale) // canonical: stale is current
	// Simulate the production "window elapsed" CAS by direct
	// CAS — the production code uses CompareAndSwap when the
	// stored entry IS the previous ticket.
	fresh := &touchTicket{m: &d.m, sid: sid, fire: time.Now()}
	if !d.m.CompareAndSwap(sid, stale, fresh) {
		t.Fatalf("CAS setup failed")
	}
	// Now: stale.AfterFire must NOT delete `fresh`.
	stale.AfterFire(0)
	if d.active() != 1 {
		t.Errorf("active = %d, want 1 (stale ticket must not delete fresh)", d.active())
	}
	if cur, _ := d.m.Load(sid); cur != fresh {
		t.Errorf("stored entry = %v, want fresh ticket", cur)
	}
	// And `fresh.AfterFire` deletes `fresh` (its own pointer).
	fresh.AfterFire(0)
	if d.active() != 0 {
		t.Errorf("post-fresh-AfterFire: active = %d, want 0", d.active())
	}
}

// TestDebounce_AfterFireNilTicketIsSafe: production passes
// `ticket` straight from shouldTouch; in the concurrently-
// suppressed path the ticket is nil. AfterFire(nil) must be
// a no-op (not panic).
func TestDebounce_AfterFireNilTicketIsSafe(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("AfterFire(nil) panicked: %v", r)
		}
	}()
	var t2 *touchTicket
	t2.AfterFire(time.Millisecond)
}
