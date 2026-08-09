// Tests for leaderStoreAdapter (Tier A8 / ADR-083 / code-review
// fix #4).

package main

import (
	"context"
	"errors"
	"testing"

	"github.com/onebox-faas/faas/pkg/gateway/leader"
	"github.com/onebox-faas/faas/pkg/state"
)

// fakeStore satisfies the narrow computeNodeLister interface.
// Other state.Store methods don't exist on this fake — the
// adapter only ever calls ActiveComputeNodes.
type fakeStore struct {
	nodes []state.ComputeNode
	err   error
}

func (f *fakeStore) ActiveComputeNodes(_ context.Context) ([]state.ComputeNode, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.nodes, nil
}

func TestNewLeaderStoreAdapter_NilStore(t *testing.T) {
	if got := newLeaderStoreAdapter(nil); got != nil {
		t.Errorf("nil store: want nil adapter, got %T", got)
	}
}

func TestLeaderStoreAdapter_ProjectsToLeaderNode(t *testing.T) {
	s := &fakeStore{nodes: []state.ComputeNode{
		{ID: "uuid-1", Name: "node-a", Active: true, VPCPUs: 64, MemMB: 65536},
		{ID: "uuid-2", Name: "node-b", Active: true, VPCPUs: 32, MemMB: 32768},
		{ID: "uuid-3", Name: "node-c", Active: true /* VPCPUs/MemMB intentionally left 0 */},
	}}
	a := newLeaderStoreAdapter(s)
	got, err := a.ListActiveComputeNodes(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("len = %d, want 3", len(got))
	}
	// Spot-check the projection: Name + NodeID + Active only;
	// VPCPUs/MemMB/etc. are dropped.
	if got[0].Name != "node-a" || got[0].NodeID != "uuid-1" || !got[0].Active {
		t.Errorf("got[0] = %+v, want Name=node-a NodeID=uuid-1 Active=true", got[0])
	}
	if got[2].Name != "node-c" || got[2].NodeID != "uuid-3" || !got[2].Active {
		t.Errorf("got[2] = %+v, want Name=node-c NodeID=uuid-3 Active=true", got[2])
	}
}

func TestLeaderStoreAdapter_PropagatesStoreError(t *testing.T) {
	want := errors.New("db down")
	a := newLeaderStoreAdapter(&fakeStore{err: want})
	_, err := a.ListActiveComputeNodes(context.Background())
	if !errors.Is(err, want) {
		t.Errorf("err = %v, want %v", err, want)
	}
}

// Compile-time guard: leaderStoreAdapter must satisfy
// leader.LeaderStore so cmd/gatewayd-public can hand it to
// DNSHandoff.LeaderStore directly.
var _ leader.LeaderStore = leaderStoreAdapter{}
