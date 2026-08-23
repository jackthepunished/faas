// alloc_branches_test.go — branch coverage for the remaining
// gaps in pkg/fcvm/alloc.go left by alloc_test.go.
//
// alloc_test.go already pins:
//   - UID uniqueness across MaxSlots
//   - bridge/network address reservations
//   - iface name length (≤15 chars)
//   - acquire/release/recycle without collision
//   - duplicate-instance rejection
//   - unknown-release error
//   - exhaustion past MaxSlots
//   - concurrent acquire with no collisions (property test)
//
// What this file adds:
//   - Acquire("") precondition guard (empty instance id rejected
//     before touching the slot pool)
//   - SetHostIPBase: multi-host deployments use 10.101.0.0 base
//     instead of the legacy 10.100.0.0
//   - SetHostIPBase: bridge + network reservations are computed
//     relative to the NEW base (operator-provided /16)
//   - Recycled slot: a freed slot handed out by a later Acquire
//     yields the SAME UID/IP (slot number is the seed for both);
//     this pins the contract operators rely on for leak detection
//   - NewAllocator: byInstance map is empty on a fresh allocator
//     (zero-leak invariant)
//   - InUse: tracks the live count after mixed acquire/release
//
// Whitebox test (package fcvm) matching alloc_test.go.
package fcvm

import (
	"net/netip"
	"testing"
)

// TestAcquireRejectsEmptyInstance pins the precondition guard at
// the very top of Acquire: an empty instance id is rejected before
// the allocator state is touched. This is a defensive guard —
// production callers should never pass "" but a buggy caller that
// does should not silently corrupt the lease pool.
func TestAcquireRejectsEmptyInstance(t *testing.T) {
	a := NewAllocator()
	_, err := a.Acquire("")
	if err == nil {
		t.Fatal("Acquire(\"\"): want error, got nil")
	}
	// The error must mention the offending input so operators can
	// diagnose the bad caller without a stack walk.
	if !contains(err.Error(), "empty") {
		t.Errorf("Acquire(\"\") err = %v, want substring 'empty'", err)
	}
	// The failed Acquire must not bump InUse — empty id was rejected
	// before the slot pool was touched.
	if a.InUse() != 0 {
		t.Errorf("Acquire(\"\") leaked a slot: InUse=%d, want 0", a.InUse())
	}
}

// TestSetHostIPBaseOverridesBridgeBase exercises the multi-host
// (Mega-PR-B / ADR-110) contract: a deployment whose per-host /16
// is 10.101.0.0 must see host IPs in 10.101.0.2/16 starting at
// .0.2, with .0.0 (network) and .0.1 (bridge) reserved.
//
// SetHostIPBase is process-global; we restore the legacy 10.100.0.0
// base in t.Cleanup so other tests aren't affected.
func TestSetHostIPBaseOverridesBridgeBase(t *testing.T) {
	defer setHostIPBaseForTest(t, netip.MustParseAddr("10.100.0.0"))()

	// Sanity-check the legacy base before the override. If this
	// fails, hostIPBase was already mutated by another test that
	// forgot to restore.
	if got := hostIPForSlot(0); got.String() != "10.100.0.2" {
		t.Fatalf("setup error: hostIPForSlot(0) under legacy base = %s, want 10.100.0.2", got)
	}

	newBase := netip.MustParseAddr("10.101.0.0")
	SetHostIPBase(newBase)
	got := hostIPForSlot(0)
	if got.String() != "10.101.0.2" {
		t.Errorf("post-SetHostIPBase slot 0 = %s, want 10.101.0.2", got)
	}

	// Bridge + network reservations follow the new base: .1
	// (bridge) and .0 (network) must NOT be handed out.
	bridge := netip.MustParseAddr("10.101.0.1")
	network := netip.MustParseAddr("10.101.0.0")
	prefix := netip.MustParsePrefix("10.101.0.0/16")
	for slot := 0; slot < 100; slot++ {
		ip := hostIPForSlot(slot)
		if ip == bridge || ip == network {
			t.Errorf("slot %d leaked reserved address %s under new base", slot, ip)
		}
		if !prefix.Contains(ip) {
			t.Errorf("slot %d host IP %s escaped %s under new base", slot, ip, prefix)
		}
	}
}

// TestRecycledSlotReusesSameUIDIP pins the §6.2-5 contract: the
// slot number IS the seed for UID, host IP, and veth names. A
// released slot handed out by a later Acquire MUST yield the SAME
// UID/IP — the slot number is unique while live, but the resource
// derivation is purely deterministic, not monotonic.
func TestRecycledSlotReusesSameUIDIP(t *testing.T) {
	a := NewAllocator()

	l1, err := a.Acquire("first")
	if err != nil {
		t.Fatalf("Acquire first: %v", err)
	}
	if err := a.Release("first"); err != nil {
		t.Fatalf("Release first: %v", err)
	}
	l2, err := a.Acquire("second")
	if err != nil {
		t.Fatalf("Acquire second: %v", err)
	}

	if l1.Slot != l2.Slot {
		t.Fatalf("recycled slot: first=%d second=%d, want equal", l1.Slot, l2.Slot)
	}
	if l1.UID != l2.UID {
		t.Errorf("recycled UID: first=%d second=%d, want equal (slot is the seed)",
			l1.UID, l2.UID)
	}
	if l1.HostIP != l2.HostIP {
		t.Errorf("recycled host IP: first=%s second=%s, want equal",
			l1.HostIP, l2.HostIP)
	}
	if l1.VethHost != l2.VethHost || l1.VethPeer != l2.VethPeer {
		t.Errorf("recycled veth names diverged: first=%q/%q second=%q/%q",
			l1.VethHost, l1.VethPeer, l2.VethHost, l2.VethPeer)
	}
	// The Instance field MUST differ — the new lease is for a new
	// VM, even if its slot is recycled. The netns name derives from
	// Instance, not slot.
	if l1.Instance == l2.Instance {
		t.Errorf("recycled lease Instance = %q; should be the new owner", l1.Instance)
	}
	if l1.Netns == l2.Netns {
		t.Errorf("recycled netns %q leaked to %q", l1.Netns, l2.Netns)
	}
}

// TestNewAllocatorStartsEmpty pins the zero-leak invariant for a
// fresh allocator: no leaked slots, no leftover byInstance entries.
func TestNewAllocatorStartsEmpty(t *testing.T) {
	a := NewAllocator()
	if a.InUse() != 0 {
		t.Errorf("NewAllocator InUse = %d, want 0", a.InUse())
	}
	if len(a.free) != MaxSlots {
		t.Errorf("NewAllocator free slots = %d, want %d", len(a.free), MaxSlots)
	}
	if len(a.byInstance) != 0 {
		t.Errorf("NewAllocator byInstance = %d, want 0 (no leaks on init)",
			len(a.byInstance))
	}
}

// TestInUseTracksLiveCountAfterMixedOps exercises InUse as the
// canonical "live leases" counter — the metric pkg/sched consumes
// for the §6.2-1 (max_concurrency) admission control.
func TestInUseTracksLiveCountAfterMixedOps(t *testing.T) {
	a := NewAllocator()
	for i := 0; i < 50; i++ {
		if _, err := a.Acquire(stringerInt(i)); err != nil {
			t.Fatalf("Acquire %d: %v", i, err)
		}
	}
	if a.InUse() != 50 {
		t.Errorf("InUse after 50 acquires = %d, want 50", a.InUse())
	}
	// Release every other instance.
	for i := 0; i < 50; i += 2 {
		if err := a.Release(stringerInt(i)); err != nil {
			t.Fatalf("Release %d: %v", i, err)
		}
	}
	if a.InUse() != 25 {
		t.Errorf("InUse after 25 releases = %d, want 25", a.InUse())
	}
	// Acquire 25 more — they reuse freed slots.
	for i := 100; i < 125; i++ {
		if _, err := a.Acquire(stringerInt(i)); err != nil {
			t.Fatalf("Acquire %d: %v", i, err)
		}
	}
	if a.InUse() != 50 {
		t.Errorf("InUse after second wave = %d, want 50", a.InUse())
	}
}

// TestLeaseForSlotUIDRange walks the first and last slot through
// the derived UID range (20000..29999). Allocator.Acquire normally
// only calls leaseForSlot with slots in 0..MaxSlots-1; this pins
// the boundary cases directly.
func TestLeaseForSlotUIDRange(t *testing.T) {
	low := leaseForSlot("a", 0)
	if low.UID != JailUIDBase {
		t.Errorf("slot 0 UID = %d, want %d", low.UID, JailUIDBase)
	}
	if low.GID != JailUIDBase {
		t.Errorf("slot 0 GID = %d, want %d", low.GID, JailUIDBase)
	}

	high := leaseForSlot("z", MaxSlots-1)
	if high.UID != JailUIDMax {
		t.Errorf("slot MaxSlots-1 UID = %d, want %d", high.UID, JailUIDMax)
	}
	if high.GID != JailUIDMax {
		t.Errorf("slot MaxSlots-1 GID = %d, want %d", high.GID, JailUIDMax)
	}
}

// --- helpers ---

// contains is a tiny strings.Contains replacement so this file
// doesn't pull in the strings package just for one check (matches
// the all-in-one-import style elsewhere in pkg/fcvm/*_test.go).
func contains(s, substr string) bool {
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

// setHostIPBaseForTest sets hostIPBase to addr for the duration
// of t, and returns a cleanup that restores the previous value
// (preserves whatever state preceded the call — does NOT enforce
// a baseline). Used by the SetHostIPBase test to restore the
// global after the assertion runs.
//
// Usage: defer setHostIPBaseForTest(t, want)()
func setHostIPBaseForTest(t *testing.T, want netip.Addr) func() {
	t.Helper()
	hostIPBaseMu.Lock()
	prev := hostIPBase
	hostIPBase = want
	hostIPBaseMu.Unlock()
	return func() {
		hostIPBaseMu.Lock()
		hostIPBase = prev
		hostIPBaseMu.Unlock()
	}
}

// stringerInt formats an int without dragging in strconv just for
// one call site; reused only by the InUse mixed-ops test.
func stringerInt(i int) string {
	if i == 0 {
		return "0"
	}
	var b [20]byte
	pos := len(b)
	for i > 0 {
		pos--
		b[pos] = byte('0' + i%10)
		i /= 10
	}
	return string(b[pos:])
}
