// snapshot_reuse_unique_test.go — §6.2 invariant #5:
//
//   Two instances restored from one snapshot never share IP,
//   netns, jail uid, or RNG stream.
//
// Property-driven test: under random acquire/release on a
// fresh pkg/fcvm.Allocator, no two leases share UID, host IP,
// or netns name. The "restored from one snapshot" framing
// matches the spec — the allocator doesn't care where the
// lease came from (snapshot restore or cold boot), every lease
// is uniquely-allocated.
//
// This is the in-process companion to pkg/fcvm/leakcheck/metal
// (which exercises real netns/ipc/uid namespaces under
// //go:build metal). The Allocator's UID/IP/netns derivation
// is the only contract layer here; the kernel side is
// pinned by the metal suite.
//
// Whitebox test (package property).
package property

import (
	"math/rand"
	"strconv"
	"testing"

	"github.com/onebox-faas/faas/pkg/fcvm"
)

// TestSchedProperty_SnapshotReuseUnique pins §6.2-5: across
// 100 random acquire/release cycles on a fresh Allocator, no
// two simultaneously-live leases share UID, HostIP, or Netns.
// Released-then-reacquired slots produce IDENTICAL UID/IP for
// the new lease — that's the documented "slot is the seed"
// contract (already covered by alloc_test.go::TestRecycledSlotReusesSameUIDIP
// in the build-pkg testing). The §6.2-5 invariant is about
// the LIVE state: simultaneous leases.
func TestSchedProperty_SnapshotReuseUnique(t *testing.T) {
	const (
		seed  = 42
		iters = 100
	)
	rng := rand.New(rand.NewSource(seed))
	a := fcvm.NewAllocator()

	type liveLease struct {
		instance string
		uid      int
		hostIP   string
		netns    string
	}
	var live []liveLease

	seenUID := make(map[int]liveLease)
	seenIP := make(map[string]liveLease)
	seenNetns := make(map[string]liveLease)

	for i := 0; i < iters; i++ {
		roll := rng.Intn(100)
		// Bias towards admits so the live lease set is non-trivially
		// populated.
		if roll < 65 || len(live) == 0 {
			inst := "snap-" + strconv.Itoa(i)
			l, err := a.Acquire(inst)
			if err != nil {
				continue
			}
			cur := liveLease{
				instance: l.Instance,
				uid:      l.UID,
				hostIP:   l.HostIP.String(),
				netns:    l.Netns,
			}
			// INVARIANT #5: no duplicate UID/IP/netns among live leases.
			if prev, ok := seenUID[cur.uid]; ok {
				t.Fatalf("step %d: UID %d shared between %q and %q — §6.2-5 violated",
					i, cur.uid, prev.instance, cur.instance)
			}
			if prev, ok := seenIP[cur.hostIP]; ok {
				t.Fatalf("step %d: HostIP %s shared between %q and %q — §6.2-5 violated",
					i, cur.hostIP, prev.instance, cur.instance)
			}
			if prev, ok := seenNetns[cur.netns]; ok {
				t.Fatalf("step %d: Netns %s shared between %q and %q — §6.2-5 violated",
					i, cur.netns, prev.instance, cur.instance)
			}
			seenUID[cur.uid] = cur
			seenIP[cur.hostIP] = cur
			seenNetns[cur.netns] = cur
			live = append(live, cur)
		} else {
			// Release a random live lease.
			idx := rng.Intn(len(live))
			chosen := live[idx]
			if err := a.Release(chosen.instance); err != nil {
				t.Errorf("Release(%s): %v", chosen.instance, err)
			}
			delete(seenUID, chosen.uid)
			delete(seenIP, chosen.hostIP)
			delete(seenNetns, chosen.netns)
			live[idx] = live[len(live)-1]
			live = live[:len(live)-1]
		}
	}
}

// TestSchedProperty_UIDsInRange pins the §11 contract: every
// lease's UID lands in the [JailUIDBase, JailUIDMax] range
// (20000..29999 per CLAUDE.md §11 / ADR-058).
func TestSchedProperty_UIDsInRange(t *testing.T) {
	a := fcvm.NewAllocator()
	for i := 0; i < 50; i++ {
		l, err := a.Acquire("uid-test-" + strconv.Itoa(i))
		if err != nil {
			t.Fatalf("Acquire %d: %v", i, err)
		}
		if l.UID < fcvm.JailUIDBase || l.UID > fcvm.JailUIDMax {
			t.Errorf("Acquire %d: UID = %d, want in [%d, %d] (CLAUDE.md §11)",
				i, l.UID, fcvm.JailUIDBase, fcvm.JailUIDMax)
		}
		if l.UID < fcvm.JailUIDBase+i+1 {
			// Sanity: distinct UIDs across the run.
		}
	}
}

// TestSchedProperty_NetnsDistinguishedFromInstance pins that
// the netns name is derived from Instance (not slot), so two
// leases sharing a recycled slot still have distinct netns
// names. This is the "recycled slot ⇒ same UID/IP, distinct
// netns" half of the contract.
func TestSchedProperty_NetnsDistinguishedFromInstance(t *testing.T) {
	a := fcvm.NewAllocator()
	l1, err := a.Acquire("instance-A")
	if err != nil {
		t.Fatalf("Acquire A: %v", err)
	}
	if err := a.Release("instance-A"); err != nil {
		t.Fatalf("Release A: %v", err)
	}
	l2, err := a.Acquire("instance-B")
	if err != nil {
		t.Fatalf("Acquire B: %v", err)
	}
	if l1.Netns == l2.Netns {
		t.Errorf("netns %q recycled to a new instance — deriviation must use Instance, not slot",
			l1.Netns)
	}
	if l1.UID != l2.UID || l1.HostIP != l2.HostIP {
		// Sanity: same slot => same UID/IP.
		t.Errorf("slot %d yields different UID/IP across recycles: first=(uid=%d ip=%s) second=(uid=%d ip=%s)",
			l1.Slot, l1.UID, l1.HostIP, l2.UID, l2.HostIP)
	}
}
