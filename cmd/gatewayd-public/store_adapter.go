// Adapter bridging pkg/state.Store to pkg/gateway/leader.LeaderStore
// (Tier A8 / ADR-083 / code-review fix #4).
//
// pkg/gateway/leader.LeaderStore expects ListActiveComputeNodes
// returning a narrow 3-field leader.ComputeNode; pkg/state.Store
// exposes ActiveComputeNodes returning a 14-field state.ComputeNode.
// The election only ever reads Name/Active — the wide struct carries
// per-node RAM/VCPU/region/etc that the orchestrator doesn't need.
//
// Thin struct adapter. Wired in cmd/gatewayd-public/main.go after
// pgStore is constructed; pgStore satisfies computeNodeLister via
// its ActiveComputeNodes method.

package main

import (
	"context"

	"github.com/onebox-faas/faas/pkg/gateway/leader"
	"github.com/onebox-faas/faas/pkg/state"
)

// computeNodeLister is the narrow surface the adapter needs.
// pkg/state.PgStore satisfies it (so does state.MemStore). We
// keep the surface narrow so test stubs don't have to implement
// the entire state.Store interface (hundreds of methods).
type computeNodeLister interface {
	ActiveComputeNodes(ctx context.Context) ([]state.ComputeNode, error)
}

// leaderStoreAdapter implements leader.LeaderStore by calling
// ActiveComputeNodes and projecting the wide state.ComputeNode
// onto the narrow leader.ComputeNode (Name + NodeID + Active).
type leaderStoreAdapter struct {
	store computeNodeLister
}

// newLeaderStoreAdapter wraps a computeNodeLister in a
// leader.LeaderStore. Returns nil if `store` is nil (lets the
// orchestrator skip election entirely when no store is wired —
// the stand-alone dev path).
func newLeaderStoreAdapter(store computeNodeLister) leader.LeaderStore {
	if store == nil {
		return nil
	}
	return leaderStoreAdapter{store: store}
}

// ListActiveComputeNodes is the leader.LeaderStore surface.
// Projects {ID, Name, Active} onto the narrow leader.ComputeNode.
func (a leaderStoreAdapter) ListActiveComputeNodes(ctx context.Context) ([]leader.ComputeNode, error) {
	nodes, err := a.store.ActiveComputeNodes(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]leader.ComputeNode, len(nodes))
	for i, n := range nodes {
		out[i] = leader.ComputeNode{
			Name:   n.Name,
			NodeID: n.ID,
			Active: n.Active,
		}
	}
	return out, nil
}
