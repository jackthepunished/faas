package fcvm

import (
	"fmt"
	"net/netip"
	"sync"

	"github.com/onebox-faas/faas/pkg/api"
)

// Invariant §6.2-5: two instances (including two restored from the SAME snapshot)
// never share an IP, netns, jail uid, or RNG stream. This allocator is the single
// authority for that. Every per-instance resource is derived from one unique
// slot, so two live instances cannot collide by construction — the property test
// in alloc_test.go proves it under concurrency.

const (
	// Jail uid/gid range (spec §4.4, §11). uid == gid per instance.
	JailUIDBase = 20000
	JailUIDMax  = 29999
	// MaxSlots is the number of simultaneously-live instances the box supports.
	// The uid range is the binding constraint (10000); tenant RAM caps real
	// concurrency far below this (47600/128 ≈ 372).
	MaxSlots = JailUIDMax - JailUIDBase + 1
)

// hostIPBase is the /16 the veth host-side addresses live in (spec §7,
// 10.100.x.y/16). Slot 0 maps to hostIPBase + hostIPOffset so the bridge address
// (10.100.0.1) and network address are never handed to an instance.
//
// Mega-PR-B (issue #911 / ADR-110 Tier-1 BLOCKING Commit 1) lifts the
// bridge CIDR from a Go const into per-host config (pkg/api.DefaultHostBridgeCIDR
// + cmd/vmmd/config.go::ComputeNodeConfig.HostBridgeCIDR). Single-host
// dev keeps the legacy 10.100.0.0 base; multi-host deployments call
// Allocator.SetHostIPBase before Acquire so per-instance host IPs land
// in the operator's chosen /16.
var (
	// hostIPBase is the per-host bridge network address the veth
	// host-side allocation starts from (spec §7, 10.100.x.y/16).
	// Slot 0 maps to hostIPBase + hostIPOffset so the bridge address
	// (hostIPBase + 1) and the network address (hostIPBase + 0) are
	// reserved for the bridge itself.
	//
	// Mega-PR-B Commit 1 (issue #911 / ADR-110 Tier-1 BLOCKING) lifts
	// the bridge CIDR from a Go const into per-host config
	// (pkg/api.DefaultHostBridgeCIDR + cmd/vmmd/config.go::ComputeNode
	// Config.HostBridgeCIDR). Single-host dev keeps the legacy
	// 10.100.0.0 base; multi-host deployments call Allocator.SetHostIP
	// Base before Acquire so per-instance host IPs land in the
	// operator's chosen /16.
	//
	// hostIPBaseMu guards hostIPBase + hostIPOffset. Acquire/Release
	// take the read lock for the duration of one hostIPForSlot call;
	// SetHostIPBase takes the write lock for the duration of the swap.
	// Not strictly required by the single-call-site discipline (vmmd
	// main calls SetHostIPBase exactly once at boot, before any
	// Acquire), but the package-level mutable makes the race
	// detector the only tripwire without this — the lock makes it
	// silent-by-construction even if a future test or external caller
	// races the two operations. Mega-PR-B review M1.
	hostIPBaseMu sync.RWMutex
	hostIPBase   = netip.MustParseAddr("10.100.0.0")
	hostIPOffset = uint32(2)
)

// SetHostIPBase overrides the per-instance veth host-side address base
// (Mega-PR-B Commit 1). Pass the network address of the per-host /16
// (e.g. 10.101.0.0 for a 10.101.0.0/16 deployment). The bridge IP
// (hostIPBase + 1) and the network address (hostIPBase + 0) are
// reserved — slot 0 maps to hostIPBase + hostIPOffset so the allocator
// never hands them out. The setter is additive; legacy callers that
// don't invoke it keep the v1 single-host 10.100.0.0 base.
//
// Cheap to call repeatedly — the RWMutex write-lock path is fast and
// the swap is a single word (netip.Addr is a 16-byte value but Go's
// interface boxing doesn't apply here, so it's an atomic-style
// assignment under the lock).
//
// Safe for concurrent calls with Acquire/Release. vmmd main calls
// SetHostIPBase exactly once at boot, before any Acquire; the lock
// documents the contract but does not depend on it.
func SetHostIPBase(addr netip.Addr) {
	hostIPBaseMu.Lock()
	defer hostIPBaseMu.Unlock()
	hostIPBase = addr
}

// Lease is the set of unique resources bound to one running instance. It is
// returned by Allocator.Acquire and must be handed back via Allocator.Release
// (by instance id) on teardown or the slot leaks.
type Lease struct {
	Instance string     // caller's instance id (e.g. a UUID); names the netns
	Slot     int        // unique while live; the root of every other field
	UID      int        // jailer --uid
	GID      int        // jailer --gid (== UID)
	HostIP   netip.Addr // routable veth host-side address, 10.100.x.y
	Netns    string     // network namespace name, fc-<instance>
	VethHost string     // host-side veth (≤15 chars, derived from slot)
	VethPeer string     // netns-side veth (≤15 chars, derived from slot)
	// Plan is the apps row's owning plan tier (issue #301, ADR-044).
	// Stamped at alloc time so every downstream consumer (Boot,
	// Restore, Destroy, Kill) reads the same plan without a separate
	// map lookup. Empty for pre-issue-301 callers (legacy 2-level
	// hierarchy); see ParentCgroupFor for the empty fallback.
	Plan api.Plan
	// IsBuilder selects the dedicated faas-cp-build.slice cgroup for an
	// ephemeral builder VM. Builder memory must not be charged to vmmd's
	// supervisor cgroup or to tenant RAM.
	IsBuilder bool
	// BuildTimeoutSec is the guest build wall-clock budget carried from
	// builderd. vmmd uses it to size builder teardown headroom; zero keeps
	// the platform default for legacy callers and ordinary app VMs.
	BuildTimeoutSec int
	// MemoryMaxMiB is the requested VM memory fence carried into the jailer
	// command. The jailer creates the per-VM cgroup as root, so memory.max is
	// set there before it drops privileges; vmmd's post-boot CPU fence remains
	// a separate write.
	MemoryMaxMiB int
}

// Allocator hands out unique Leases and recycles slots on release. Safe for
// concurrent use — vmmd may wake many instances at once.
type Allocator struct {
	mu         sync.Mutex
	free       []int          // stack of free slot numbers
	byInstance map[string]int // instance id -> slot, for Release + double-acquire guard
}

// NewAllocator returns an allocator with all MaxSlots free.
func NewAllocator() *Allocator {
	free := make([]int, MaxSlots)
	for i := range free {
		// Hand out low slots first for readable uids/IPs in dev; order is not
		// load-bearing.
		free[i] = MaxSlots - 1 - i
	}
	return &Allocator{free: free, byInstance: make(map[string]int)}
}

// InUse reports how many slots are currently leased.
func (a *Allocator) InUse() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.byInstance)
}

// Acquire leases a unique slot for instance. It errors if the instance already
// holds a lease (a bug — Release first) or the box is at MaxSlots.
func (a *Allocator) Acquire(instance string) (Lease, error) {
	if instance == "" {
		return Lease{}, fmt.Errorf("fcvm: acquire: empty instance id")
	}
	a.mu.Lock()
	defer a.mu.Unlock()

	if _, dup := a.byInstance[instance]; dup {
		return Lease{}, fmt.Errorf("fcvm: acquire: instance %q already holds a lease", instance)
	}
	if len(a.free) == 0 {
		return Lease{}, fmt.Errorf("fcvm: acquire: no free slots (all %d in use)", MaxSlots)
	}

	slot := a.free[len(a.free)-1]
	a.free = a.free[:len(a.free)-1]
	a.byInstance[instance] = slot
	return leaseForSlot(instance, slot), nil
}

// Release returns instance's slot to the free pool. It is idempotent-safe to call
// once per acquired instance; releasing an unknown instance is a no-op error the
// caller may ignore, surfaced for leak detection during tests.
func (a *Allocator) Release(instance string) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	slot, ok := a.byInstance[instance]
	if !ok {
		return fmt.Errorf("fcvm: release: instance %q holds no lease", instance)
	}
	delete(a.byInstance, instance)
	a.free = append(a.free, slot)
	return nil
}

// leaseForSlot deterministically derives every resource from the slot. Given a
// unique slot the outputs are unique; that is the whole invariant.
func leaseForSlot(instance string, slot int) Lease {
	return Lease{
		Instance: instance,
		Slot:     slot,
		UID:      JailUIDBase + slot,
		GID:      JailUIDBase + slot,
		HostIP:   hostIPForSlot(slot),
		Netns:    "fc-" + instance,
		VethHost: fmt.Sprintf("vh%d", slot),
		VethPeer: fmt.Sprintf("vp%d", slot),
	}
}

// hostIPForSlot maps a slot into 10.100.0.0/16 starting at .0.2.
// Takes the read lock so a concurrent SetHostIPBase serializes
// against the load; reads see either the old or new value, never a
// torn one. Mega-PR-B review M1.
func hostIPForSlot(slot int) netip.Addr {
	hostIPBaseMu.RLock()
	base := hostIPBase
	hostIPBaseMu.RUnlock()
	v := base.As4()
	n := uint32(v[0])<<24 | uint32(v[1])<<16 | uint32(v[2])<<8 | uint32(v[3])
	n += hostIPOffset + uint32(slot)
	return netip.AddrFrom4([4]byte{byte(n >> 24), byte(n >> 16), byte(n >> 8), byte(n)})
}
