// pgbackend_pernode_test.go — per-node sub-cursor tests for the
// multi-box picker (placement scheduler PR, ADR-025 axis 3).
//
// The pre-multi-box picker used a single atomic.Uint64 round-robin
// across all healthy entries for an app — fine when every instance
// lived on the same node, broken in a multi-box fleet where we want
// to (a) round-robin within the node that has the most healthy entries
// and (b) prefer the warm-affinity node when the warm hint matches.
//
// These tests pin both contracts at the unit level. They use a
// FakeScheduler (gateway.NewFakeScheduler) but vary its returned
// NodeID per Admit call via a small shim, since the public
// FakeScheduler pins a single NodeID.

package gateway_test

import (
	"context"
	"strconv"
	"sync/atomic"
	"testing"

	"github.com/onebox-faas/faas/pkg/gateway"
)

// rotatingScheduler is a controllable scheduler that emits a
// different NodeID per call (minted from a closure). Used to seed
// the per-app targetSet with heterogeneous nodes so the picker has
// to make a real per-node choice rather than degenerate to the
// single-node fast path.
type rotatingScheduler struct {
	nextNodeID func() string // returns the node id for the next Admit call
	calls      atomic.Int64  // admitted count; ids are i-1..i-N
	method     int32         // raw wake method for Admit
}

func (r *rotatingScheduler) AdmitInstance(context.Context, string) (string, string, string, int32, bool, error) {
	idx := r.calls.Add(1)
	nodeID := r.nextNodeID()
	// Scheduler signature: (instanceID, nodeID, wakeID, method, atCapacity, err).
	return "i-" + strconv.FormatInt(idx, 10), nodeID, "wake-" + strconv.FormatInt(idx, 10), r.method, false, nil
}

// TestPGBackend_PickRotatesWithinWinningNode seeds two nodes with
// different healthy counts (a has 3, b has 1) and asserts the picker
// round-robins WITHIN a — never returning the b node's instance
// until a is exhausted. This is the load-bearing per-node sub-cursor
// behavior: without it, the gateway would hammer the smallest node.
func TestPGBackend_PickRotatesWithinWinningNode(t *testing.T) {
	// First 3 admits → node A; last admit → node B.
	admitIdx := atomic.Int64{}
	sched := &rotatingScheduler{
		nextNodeID: func() string {
			n := admitIdx.Add(1)
			if n <= 3 {
				return "node-A"
			}
			return "node-B"
		},
		method: gateway.WireWakeColdBoot,
	}
	b := gateway.NewPGBackend(&fakeRouter{byID: map[string]gateway.App{}}, sched, nil)

	// Seed 4 admits (3 on A, 1 on B). HealthyCount is total.
	for i := 0; i < 4; i++ {
		if _, _, _, err := b.Admit(context.Background(), "app-1", 5); err != nil {
			t.Fatalf("Admit #%d: %v", i+1, err)
		}
	}
	if got := b.HealthyCount("app-1"); got != 4 {
		t.Fatalf("HealthyCount = %d, want 4", got)
	}

	// 8 picks: every one must be from node-A (it has 3 healthy,
	// wins on count). Pick must never reach node-B until node-A is
	// exhausted, which never happens here. The sub-cursor keeps
	// rotating across the 3 A instances.
	seenA := map[string]bool{}
	for i := 0; i < 8; i++ {
		t1, ok := b.Pick("app-1")
		if !ok {
			t.Fatalf("Pick #%d = !ok", i)
		}
		if t1.NodeID != "node-A" {
			t.Errorf("Pick #%d returned NodeID = %q, want node-A (winning node by healthyCount)", i, t1.NodeID)
		}
		seenA[t1.InstanceID] = true
	}
	// Round-robin across the 3 A instances.
	if len(seenA) < 2 {
		t.Errorf("Pick rotated %d distinct A instances in 8 picks, want ≥2 (round-robin)", len(seenA))
	}
}

// TestPGBackend_PickPrefersWarmAffinityNode seeds two nodes with
// equal healthy counts but configures a warm hint that pins
// node-B. The picker must prefer B even though A wins on lex order
// — this is the warm-bonus path in pkg/gateway/pgbackend.go.
func TestPGBackend_PickPrefersWarmAffinityNode(t *testing.T) {
	admitIdx := atomic.Int64{}
	sched := &rotatingScheduler{
		nextNodeID: func() string {
			n := admitIdx.Add(1)
			// Alternate: A, B, A, B → 2 on each.
			if n%2 == 1 {
				return "node-A"
			}
			return "node-B"
		},
		method: gateway.WireWakeColdBoot,
	}
	b := gateway.NewPGBackend(&fakeRouter{byID: map[string]gateway.App{}}, sched, nil)
	b.WithWarmHint(func(appID string) (string, bool) {
		if appID == "app-1" {
			return "node-B", true
		}
		return "", false
	})

	for i := 0; i < 4; i++ {
		if _, _, _, err := b.Admit(context.Background(), "app-1", 5); err != nil {
			t.Fatalf("Admit #%d: %v", i+1, err)
		}
	}

	// 12 picks: every one must be from node-B (warm hint overrides
	// tie-break). node-A should never be returned while node-B is
	// non-empty.
	for i := 0; i < 12; i++ {
		t1, ok := b.Pick("app-1")
		if !ok {
			t.Fatalf("Pick #%d = !ok", i)
		}
		if t1.NodeID != "node-B" {
			t.Errorf("Pick #%d returned NodeID = %q, want node-B (warm hint)", i, t1.NodeID)
		}
	}
}

// TestPGBackend_PickColdPathHonorsLexOrder seeds two nodes with equal
// counts and no warm hint. The picker must use lex order on the
// stable nodeOrder — node-A sorts before node-B.
func TestPGBackend_PickColdPathHonorsLexOrder(t *testing.T) {
	admitIdx := atomic.Int64{}
	sched := &rotatingScheduler{
		nextNodeID: func() string {
			// Insert order matters: B comes first so nodeOrder
			// has B before A. A still wins on lex tie-break.
			n := admitIdx.Add(1)
			if n == 1 {
				return "node-B"
			}
			return "node-A"
		},
		method: gateway.WireWakeColdBoot,
	}
	b := gateway.NewPGBackend(&fakeRouter{byID: map[string]gateway.App{}}, sched, nil)
	// No WithWarmHint set → picker uses healthyCount + lex tie-break.

	for i := 0; i < 2; i++ {
		if _, _, _, err := b.Admit(context.Background(), "app-1", 5); err != nil {
			t.Fatalf("Admit #%d: %v", i+1, err)
		}
	}

	// 6 picks: all from node-A (lex order wins on tied counts).
	for i := 0; i < 6; i++ {
		t1, ok := b.Pick("app-1")
		if !ok {
			t.Fatalf("Pick #%d = !ok", i)
		}
		if t1.NodeID != "node-A" {
			t.Errorf("Pick #%d returned NodeID = %q, want node-A (lex tie-break)", i, t1.NodeID)
		}
	}
}

// TestPGBackend_PickSingleNodeFastPath seeds a single node with
// multiple instances and asserts Pick returns each instance in turn
// via the legacy single-cursor path (no per-node map allocation).
// This is the one-box degenerate case — must keep working without
// the per-node machinery allocating a map for one entry.
func TestPGBackend_PickSingleNodeFastPath(t *testing.T) {
	sched := gateway.NewFakeScheduler("node-A") // mints i-1, i-2, i-3 by default
	b := gateway.NewPGBackend(&fakeRouter{byID: map[string]gateway.App{}}, sched, nil)

	for i := 0; i < 3; i++ {
		if _, _, _, err := b.Admit(context.Background(), "app-1", 5); err != nil {
			t.Fatalf("Admit #%d: %v", i+1, err)
		}
	}
	if got := b.HealthyCount("app-1"); got != 3 {
		t.Fatalf("HealthyCount = %d, want 3", got)
	}
	seen := map[string]bool{}
	for i := 0; i < 3; i++ {
		t1, ok := b.Pick("app-1")
		if !ok {
			t.Fatalf("Pick #%d = !ok", i)
		}
		if t1.NodeID != "node-A" {
			t.Errorf("Pick #%d NodeID = %q, want node-A", i, t1.NodeID)
		}
		seen[t1.InstanceID] = true
	}
	if len(seen) != 3 {
		t.Errorf("distinct picks = %d, want 3 (round-robin across the single node)", len(seen))
	}
}

// TestPGBackend_PickAfterEvictNodeEntry prunes the sub-cursor when
// the last entry for a node is evicted. After the eviction, the
// picker's nodeOrder must no longer mention the drained node —
// subsequent picks must come from the surviving node only.
func TestPGBackend_PickAfterEvictNodeEntry(t *testing.T) {
	admitIdx := atomic.Int64{}
	sched := &rotatingScheduler{
		nextNodeID: func() string {
			// 2 on A (i-1, i-2), 1 on B (i-3).
			n := admitIdx.Add(1)
			if n <= 2 {
				return "node-A"
			}
			return "node-B"
		},
		method: gateway.WireWakeColdBoot,
	}
	b := gateway.NewPGBackend(&fakeRouter{byID: map[string]gateway.App{}}, sched, nil)

	for i := 0; i < 3; i++ {
		if _, _, _, err := b.Admit(context.Background(), "app-1", 5); err != nil {
			t.Fatalf("Admit #%d: %v", i+1, err)
		}
	}

	// Drain node-A: evict i-1 and i-2 (the two A instances).
	b.EvictInstance("app-1", "i-1")
	b.EvictInstance("app-1", "i-2")

	// Only node-B's i-3 should be reachable now.
	if got := b.HealthyCount("app-1"); got != 1 {
		t.Fatalf("HealthyCount after draining A = %d, want 1", got)
	}
	for i := 0; i < 4; i++ {
		t1, ok := b.Pick("app-1")
		if !ok {
			t.Fatalf("Pick #%d = !ok", i)
		}
		if t1.InstanceID != "i-3" {
			t.Errorf("Pick #%d returned InstanceID = %q, want i-3 (the surviving node-B entry)", i, t1.InstanceID)
		}
	}
}
