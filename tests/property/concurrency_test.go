// Package property holds cross-package property-based tests
// that pin the structural invariants of the Gregale platform.
// Today the package covers:
//
//   - Tier A8 / ADR-083 §14 M8 acceptance item 5: under
//     random `compute_node_changed` events on a two-host
//     cluster, `pkg/gateway/leader.ElectLeader` always
//     returns exactly one leader (never two, never zero
//     when ≥ 1 active peer exists).
//
// The test reuses the lex-min election from
// pkg/gateway/leader; the fuzz driver is hand-rolled (not
// go-fuzz) because the operation set is small and the
// invariant is closed-form.
//
// # Invariants pinned
//
//  1. If `len(activeNodes) >= 1`, ElectLeader returns a
//     Leader with non-empty Name (never Leader{}).
//  2. The elected Leader's Name is the lex-min over the
//     active subset.
//  3. After draining the elected leader (active=false on
//     the winner), ElectLeader returns a new Leader whose
//     Name is the lex-min over the surviving active subset.
//  4. Concurrent election cycles against the same store
//     return the same Leader (the election is pure).
package property

import (
	"context"
	"math/rand"
	"sync"
	"testing"
	"time"

	"github.com/onebox-faas/faas/pkg/gateway/leader"
)

// propCluster is a small fixed universe of compute nodes the
// fuzz driver mutates. Mirrors pkg/sched/ledger_property_test.go's
// propApp shape (a fixed-size fixture so the fuzz loop is
// deterministic + replayable).
type propCluster struct {
	nodes []leader.ComputeNode
}

func (c *propCluster) drain(name string) {
	for i := range c.nodes {
		if c.nodes[i].Name == name {
			c.nodes[i].Active = false
			return
		}
	}
}

func (c *propCluster) reactivate(name string) {
	for i := range c.nodes {
		if c.nodes[i].Name == name {
			c.nodes[i].Active = true
			return
		}
	}
}

func (c *propCluster) activeSubset() []leader.ComputeNode {
	out := make([]leader.ComputeNode, 0, len(c.nodes))
	for _, n := range c.nodes {
		if n.Active && n.Name != "" {
			out = append(out, n)
		}
	}
	return out
}

// storeAdapter implements leader.LeaderStore by holding a
// pointer to the cluster's current state. The pointer is
// captured by value at construction time; the cluster
// mutations after construction are visible to the store
// because both reference the same backing slice.
type storeAdapter struct{ cluster *propCluster }

func (s storeAdapter) ListActiveComputeNodes(_ context.Context) ([]leader.ComputeNode, error) {
	return s.cluster.activeSubset(), nil
}

// TestActivePassiveElectionUnique is the property test that
// closes the §14 M8 acceptance item 5. The fuzz driver
// applies 200 random compute_node_changed events against a
// 5-node cluster and asserts:
//
//   - ElectLeader returns exactly one Leader per call.
//   - The Leader's Name is the lex-min over the active subset.
//   - The Leader's Name is non-empty whenever the active
//     subset is non-empty.
//   - Repeated calls against an unchanged cluster return the
//     same Leader (the election is pure).
//   - Concurrent calls against the same cluster all see the
//     same Leader.
func TestActivePassiveElectionUnique(t *testing.T) {
	const (
		nFuzzOps    = 200
		nNodes      = 5
		nConcurrent = 8
	)
	cluster := &propCluster{
		nodes: []leader.ComputeNode{
			{Name: "node-a", NodeID: "uuid-a", Active: true},
			{Name: "node-b", NodeID: "uuid-b", Active: true},
			{Name: "node-c", NodeID: "uuid-c", Active: true},
			{Name: "node-d", NodeID: "uuid-d", Active: true},
			{Name: "node-e", NodeID: "uuid-e", Active: true},
		},
	}
	if len(cluster.nodes) != nNodes {
		t.Fatalf("cluster size mismatch: got %d, want %d", len(cluster.nodes), nNodes)
	}
	store := storeAdapter{cluster: cluster}

	rng := rand.New(rand.NewSource(42)) // deterministic seed for replay

	var lastLeader leader.Leader

	for i := 0; i < nFuzzOps; i++ {
		// Pick a random op: drain or reactivate.
		var op func()
		switch rng.Intn(3) {
		case 0, 1:
			// Drain a random node (most ops are drains —
			// mirrors the operator-driven reality).
			name := cluster.nodes[rng.Intn(len(cluster.nodes))].Name
			op = func() { cluster.drain(name) }
		case 2:
			// Reactivate a random node.
			name := cluster.nodes[rng.Intn(len(cluster.nodes))].Name
			op = func() { cluster.reactivate(name) }
		}
		op()

		got, err := leader.ElectLeader(context.Background(), store)
		if err != nil {
			t.Fatalf("op %d: ElectLeader err: %v", i, err)
		}
		active := cluster.activeSubset()
		if len(active) == 0 {
			if !got.IsZero() {
				t.Errorf("op %d: empty active subset must return zero Leader, got %+v", i, got)
			}
			continue
		}
		// Lex-min over the active subset.
		wantName := active[0].Name
		for _, n := range active[1:] {
			if n.Name < wantName {
				wantName = n.Name
			}
		}
		if got.Name != wantName {
			t.Errorf("op %d: lex-min = %q, want %q (active subset %v)",
				i, got.Name, wantName, active)
		}
		if got.NodeID == "" {
			t.Errorf("op %d: NodeID empty, want non-empty", i)
		}
		if got.Elected.IsZero() {
			t.Errorf("op %d: Elected zero, want non-zero", i)
		}
		// Idempotence: re-running the election against an
		// unchanged cluster must return the same Leader. The
		// election is pure w.r.t. Name + NodeID; the
		// timestamp is wall-clock metadata that varies.
		got2, err := leader.ElectLeader(context.Background(), store)
		if err != nil {
			t.Fatalf("op %d (idempotence): ElectLeader err: %v", i, err)
		}
		if got2.Name != got.Name || got2.NodeID != got.NodeID {
			t.Errorf("op %d: idempotence violated: %+v vs %+v", i, got, got2)
		}
		lastLeader = got
	}

	if lastLeader.IsZero() {
		t.Fatalf("final state: no leader (cluster = %+v)", cluster.nodes)
	}
}

// TestActivePassiveElectionConcurrent spins nConcurrent
// goroutines that all call ElectLeader against the same
// store. The property: every goroutine sees the same Leader.
// Mirrors the steady-state where every active box computes
// the election on every pg_notify event.
func TestActivePassiveElectionConcurrent(t *testing.T) {
	cluster := &propCluster{
		nodes: []leader.ComputeNode{
			{Name: "node-a", NodeID: "uuid-a", Active: true},
			{Name: "node-b", NodeID: "uuid-b", Active: true},
			{Name: "node-c", NodeID: "uuid-c", Active: true},
		},
	}
	store := storeAdapter{cluster: cluster}

	const nGoroutines = 16
	var wg sync.WaitGroup
	results := make([]leader.Leader, nGoroutines)
	errs := make([]error, nGoroutines)
	for i := 0; i < nGoroutines; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			l, err := leader.ElectLeader(context.Background(), store)
			results[i] = l
			errs[i] = err
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("goroutine %d: %v", i, err)
		}
	}
	// Every goroutine must have seen the same leader. The
	// election is pure w.r.t. content (Name + NodeID); the
	// timestamp is wall-clock metadata that varies between
	// concurrent calls — that's expected.
	for i := 1; i < nGoroutines; i++ {
		if results[i].Name != results[0].Name || results[i].NodeID != results[0].NodeID {
			t.Errorf("goroutine %d saw %+v, want Name=%q NodeID=%q (concurrent elections must agree)",
				i, results[i], results[0].Name, results[0].NodeID)
		}
	}
	if results[0].IsZero() {
		t.Fatalf("leader is zero: results[0]=%+v", results[0])
	}
	if results[0].Name != "node-a" {
		t.Errorf("lex-min over %v = %q, want %q", cluster.nodes, results[0].Name, "node-a")
	}
}

// TestActivePassiveElectionDrainAndReactivate walks through
// the §14 M8 manual smoke step by step: drain the leader,
// re-elect, re-activate, re-elect. Asserts the leader flips
// monotonically with the active subset.
func TestActivePassiveElectionDrainAndReactivate(t *testing.T) {
	cluster := &propCluster{
		nodes: []leader.ComputeNode{
			{Name: "node-a", NodeID: "uuid-a", Active: true},
			{Name: "node-b", NodeID: "uuid-b", Active: true},
			{Name: "node-c", NodeID: "uuid-c", Active: true},
		},
	}
	now := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)

	// Initial: node-a is the lex-min leader.
	got := leader.ElectLeaderFromNodes(cluster.activeSubset(), now)
	if got.Name != "node-a" {
		t.Fatalf("initial leader = %q, want %q", got.Name, "node-a")
	}

	// Drain node-a (the operator's UPDATE compute_nodes
	// SET active=false WHERE name='node-a').
	cluster.drain("node-a")
	got = leader.ElectLeaderFromNodes(cluster.activeSubset(), now.Add(time.Second))
	if got.Name != "node-b" {
		t.Fatalf("post-drain leader = %q, want %q", got.Name, "node-b")
	}

	// Reactivate node-a — lex-min flips back.
	cluster.reactivate("node-a")
	got = leader.ElectLeaderFromNodes(cluster.activeSubset(), now.Add(2*time.Second))
	if got.Name != "node-a" {
		t.Fatalf("post-reactivate leader = %q, want %q", got.Name, "node-a")
	}

	// Drain all three — no leader.
	for _, n := range cluster.nodes {
		cluster.drain(n.Name)
	}
	got = leader.ElectLeaderFromNodes(cluster.activeSubset(), now.Add(3*time.Second))
	if !got.IsZero() {
		t.Fatalf("all-drained leader = %+v, want zero", got)
	}
}