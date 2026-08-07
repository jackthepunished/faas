// Tests for the lex-min leader election (ADR-083 / Tier A8).
// Pure-function tests for the slice-in / Leader-out core, plus
// the store-backed election. No fixtures beyond an in-memory fake
// LeaderStore.

package leader

import (
	"context"
	"errors"
	"testing"
	"time"
)

// fixedNow returns a deterministic wall-clock time so test
// assertions on `Leader.Elected` are stable across timezones.
func fixedNow() time.Time {
	return time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
}

// fakeStore is an in-memory LeaderStore. The election algorithm
// is pure, so the fake is just a slice holder + an injectable
// error for the store-failure path.
type fakeStore struct {
	nodes []ComputeNode
	err   error
}

func (f *fakeStore) ListActiveComputeNodes(_ context.Context) ([]ComputeNode, error) {
	if f.err != nil {
		return nil, f.err
	}
	out := make([]ComputeNode, len(f.nodes))
	copy(out, f.nodes)
	return out, nil
}

// 3-node fixture used by every "kill the leader, re-elect" test.
func threeNodes() []ComputeNode {
	return []ComputeNode{
		{Name: "node-c", NodeID: "uuid-c", Active: true},
		{Name: "node-a", NodeID: "uuid-a", Active: true},
		{Name: "node-b", NodeID: "uuid-b", Active: true},
	}
}

func TestElectLeaderFromNodes_LexMin(t *testing.T) {
	got := ElectLeaderFromNodes(threeNodes(), fixedNow())
	want := Leader{Name: "node-a", NodeID: "uuid-a", Elected: fixedNow()}
	if got != want {
		t.Fatalf("lex-min over %v = %+v, want %+v", threeNodes(), got, want)
	}
}

func TestElectLeaderFromNodes_SingleNode(t *testing.T) {
	nodes := []ComputeNode{{Name: "node-only", NodeID: "uuid-only", Active: true}}
	got := ElectLeaderFromNodes(nodes, fixedNow())
	if got.Name != "node-only" || got.NodeID != "uuid-only" {
		t.Fatalf("single active node lost: %+v", got)
	}
}

func TestElectLeaderFromNodes_Empty(t *testing.T) {
	if got := ElectLeaderFromNodes(nil, fixedNow()); !got.IsZero() {
		t.Fatalf("empty input must return zero-value Leader, got %+v", got)
	}
	if got := ElectLeaderFromNodes([]ComputeNode{}, fixedNow()); !got.IsZero() {
		t.Fatalf("empty slice must return zero-value Leader, got %+v", got)
	}
}

func TestElectLeaderFromNodes_AllInactive(t *testing.T) {
	nodes := []ComputeNode{
		{Name: "node-a", NodeID: "uuid-a", Active: false},
		{Name: "node-b", NodeID: "uuid-b", Active: false},
	}
	if got := ElectLeaderFromNodes(nodes, fixedNow()); !got.IsZero() {
		t.Fatalf("all-inactive must return zero-value Leader, got %+v", got)
	}
}

// Mixed active/inactive: drained nodes never win. This is the
// core safety property of the ADR-064 drain-before-shutdown
// pattern.
func TestElectLeaderFromNodes_MixedFiltersInactive(t *testing.T) {
	nodes := []ComputeNode{
		{Name: "node-a", NodeID: "uuid-a", Active: false},
		{Name: "node-b", NodeID: "uuid-b", Active: true},
		{Name: "node-c", NodeID: "uuid-c", Active: true},
	}
	got := ElectLeaderFromNodes(nodes, fixedNow())
	if got.Name != "node-b" {
		t.Fatalf("lex-min over active subset = %q, want %q", got.Name, "node-b")
	}
}

// Empty Name with Active=true is filtered out — guards against a
// future schema migration that flips a row's Name to ''. The
// partial index `idx_compute_nodes_active` does not cover this
// case.
func TestElectLeaderFromNodes_EmptyNameFiltered(t *testing.T) {
	nodes := []ComputeNode{
		{Name: "", NodeID: "uuid-x", Active: true},
		{Name: "node-b", NodeID: "uuid-b", Active: true},
	}
	got := ElectLeaderFromNodes(nodes, fixedNow())
	if got.Name != "node-b" {
		t.Fatalf("empty Name must be filtered, got %q", got.Name)
	}
}

// Kill-the-leader re-election: drain `node-a` (the lex-min winner),
// re-elect, assert `node-b` wins. Mirrors the §14 M8 manual smoke
// step 4.
func TestElectLeaderFromNodes_KillLeaderReElects(t *testing.T) {
	before := threeNodes()
	if got := ElectLeaderFromNodes(before, fixedNow()); got.Name != "node-a" {
		t.Fatalf("baseline lex-min = %q, want %q", got.Name, "node-a")
	}
	// Operator flips active=false on node-a (the ADR-064 / ADR-066
	// drain pattern). The election now sees {b, c}.
	after := []ComputeNode{
		{Name: "node-a", NodeID: "uuid-a", Active: false},
		{Name: "node-b", NodeID: "uuid-b", Active: true},
		{Name: "node-c", NodeID: "uuid-c", Active: true},
	}
	got := ElectLeaderFromNodes(after, fixedNow().Add(time.Second))
	if got.Name != "node-b" {
		t.Fatalf("post-drain lex-min = %q, want %q", got.Name, "node-b")
	}
	if got.Elected.Equal(fixedNow()) {
		t.Fatalf("Elected must be advanced on re-election, got %v", got.Elected)
	}
}

// Stable tie-break: two nodes with the same Name must produce a
// deterministic result. The unique constraint on
// `compute_nodes.name` prevents this in prod, but the election
// itself should not panic.
func TestElectLeaderFromNodes_TieBreakDeterministic(t *testing.T) {
	nodes := []ComputeNode{
		{Name: "node-a", NodeID: "uuid-a1", Active: true},
		{Name: "node-a", NodeID: "uuid-a2", Active: true},
	}
	got1 := ElectLeaderFromNodes(nodes, fixedNow())
	got2 := ElectLeaderFromNodes(nodes, fixedNow())
	if got1 != got2 {
		t.Fatalf("tie-break must be deterministic: %+v vs %+v", got1, got2)
	}
	// sort.Slice is NOT stable on equal keys, so we accept
	// either NodeID — but it must not panic and must be one of
	// the two.
	if got1.NodeID != "uuid-a1" && got1.NodeID != "uuid-a2" {
		t.Fatalf("tie-break returned unexpected NodeID %q", got1.NodeID)
	}
}

func TestElectLeader_StoreBacked(t *testing.T) {
	store := &fakeStore{nodes: threeNodes()}
	got, err := ElectLeader(context.Background(), store)
	if err != nil {
		t.Fatalf("ElectLeader: %v", err)
	}
	if got.Name != "node-a" {
		t.Fatalf("store-backed election = %q, want %q", got.Name, "node-a")
	}
}

func TestElectLeader_StoreErrorPropagates(t *testing.T) {
	store := &fakeStore{err: errors.New("postgres gone")}
	got, err := ElectLeader(context.Background(), store)
	if err == nil {
		t.Fatalf("store error must propagate, got Leader=%+v", got)
	}
	if !got.IsZero() {
		t.Fatalf("store error must return zero-value Leader, got %+v", got)
	}
}

func TestElectLeader_ContextCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	store := &fakeStore{nodes: threeNodes()}
	got, err := ElectLeader(ctx, store)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled context must surface, got err=%v", err)
	}
	if !got.IsZero() {
		t.Fatalf("canceled context must return zero-value Leader, got %+v", got)
	}
}

func TestLeader_IsZero(t *testing.T) {
	if !(Leader{}).IsZero() {
		t.Fatalf("zero-value Leader must report IsZero")
	}
	if (Leader{Name: "node-a"}).IsZero() {
		t.Fatalf("non-empty Leader must not report IsZero")
	}
}