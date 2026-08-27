package sched

import (
	"context"
	"testing"
	"time"

	"github.com/onebox-faas/faas/pkg/state"
)

func TestNodeRegistryRefreshAndRemove(t *testing.T) {
	node := state.ComputeNode{ID: "node-a", Name: "a", Active: true}
	reg := NewNodeRegistry([]state.ComputeNode{node})
	if got := len(reg.Snapshot()); got != 1 {
		t.Fatalf("initial snapshot length = %d, want 1", got)
	}
	node.TargetURL = "tcp://node-a:8080"
	reg.Refresh(node)
	if got := reg.Snapshot()[0].TargetURL; got != node.TargetURL {
		t.Fatalf("refreshed target = %q, want %q", got, node.TargetURL)
	}
	node.Active = false
	reg.Refresh(node)
	if got := len(reg.Snapshot()); got != 0 {
		t.Fatalf("inactive refresh length = %d, want 0", got)
	}
	reg.Refresh(state.ComputeNode{ID: "node-a", Active: true})
	reg.Remove("node-a")
	if got := len(reg.Snapshot()); got != 0 {
		t.Fatalf("removed snapshot length = %d, want 0", got)
	}
}

func TestNodeTelemetryCacheReplacesAsOneBatchAndExpires(t *testing.T) {
	cache := NewNodeTelemetryCache()
	base := time.Unix(100, 0)
	resident := int64(128 << 20)
	cache.Replace("node-a", base, base, []NodeTelemetry{{InstanceID: "vm-1", ResidentBytes: &resident}})

	rows := cache.Snapshot(base.Add(time.Second))
	if len(rows) != 1 || rows[0].NodeID != "node-a" || rows[0].Telemetry.InstanceID != "vm-1" {
		t.Fatalf("snapshot = %+v, want one node-a/vm-1 row", rows)
	}
	rows = cache.Snapshot(base.Add(TelemetryFreshness + time.Nanosecond))
	if len(rows) != 0 {
		t.Fatalf("expired snapshot length = %d, want 0", len(rows))
	}
}

func TestNodeUsageCacheRequiresCompleteSnapshot(t *testing.T) {
	cache := NewNodeUsageCache()
	now := time.Unix(200, 0)
	cache.Replace([]string{"a", "b"}, map[string]int64{"a": 10}, now)
	if _, ok := cache.Lookup([]string{"a", "c"}, now); ok {
		t.Fatal("partial cache unexpectedly satisfied a request for an unseen node")
	}
	got, ok := cache.Lookup([]string{"a", "b"}, now.Add(500*time.Millisecond))
	if !ok || got["a"] != 10 || got["b"] != 0 {
		t.Fatalf("complete cache = (%v, %v), want a=10,b=0", got, ok)
	}
	if _, ok := cache.Lookup([]string{"a", "b"}, now.Add(NodeUsageFreshness+time.Nanosecond)); ok {
		t.Fatal("expired usage cache unexpectedly returned a hit")
	}
}

type countingNodeUsageStore struct {
	*state.MemStore
	activeCalls int
	bulkCalls   int
	singleCalls int
}

func (s *countingNodeUsageStore) ActiveComputeNodes(ctx context.Context) ([]state.ComputeNode, error) {
	s.activeCalls++
	return s.MemStore.ActiveComputeNodes(ctx)
}

func (s *countingNodeUsageStore) ComputeNodeUsedMB(ctx context.Context, nodeID string) (int64, error) {
	s.singleCalls++
	return s.MemStore.ComputeNodeUsedMB(ctx, nodeID)
}

func (s *countingNodeUsageStore) ComputeNodeUsedMBByNode(ctx context.Context, nodeIDs []string) (map[string]int64, error) {
	s.bulkCalls++
	return s.MemStore.ComputeNodeUsedMBByNode(ctx, nodeIDs)
}

func TestPlacementUsesRegistryAndOneBulkUsageRead(t *testing.T) {
	base := state.NewMemStore()
	store := &countingNodeUsageStore{MemStore: base}
	second, err := base.CreateComputeNode(context.Background(), state.ComputeNode{
		Name:               "node-b",
		TargetURL:          "tcp://10.0.0.2:50051",
		VPCPUs:             8,
		MemMB:              8192,
		MaxConcurrency:     4,
		AdmissionCeilingMB: 4096,
		Active:             true,
	})
	if err != nil {
		t.Fatalf("CreateComputeNode: %v", err)
	}
	nodes, err := base.ActiveComputeNodes(context.Background())
	if err != nil {
		t.Fatalf("ActiveComputeNodes: %v", err)
	}
	engine, err := NewEngine(context.Background(), store, NewNodeLedger(), nil, nil, "", nil)
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	engine.WithNodeRegistry(NewNodeRegistry(nodes))
	if _, err := engine.choosePlacementLocked(context.Background(), Request{RAMMB: 1}); err != nil {
		t.Fatalf("choosePlacementLocked: %v", err)
	}
	if store.activeCalls != 0 {
		t.Fatalf("ActiveComputeNodes calls = %d, want 0 with registry", store.activeCalls)
	}
	if store.bulkCalls != 1 {
		t.Fatalf("bulk usage calls = %d, want 1", store.bulkCalls)
	}
	if store.singleCalls != 0 {
		t.Fatalf("single usage calls = %d, want 0", store.singleCalls)
	}
	if second.ID == "" {
		t.Fatal("created node has empty id")
	}
}
