// leader_store_adapter.go — Tier A9 / ADR-084 adapter from
// pkg/state.PgStore to pkg/gateway/leader.LeaderStore for
// the gatewayd-internal daemon.
//
// cmd/gatewayd-public has its own equivalent
// (cmd/gatewayd-public/store_adapter.go, PR-A fix #4). The
// duplication is intentional: the two daemons are separate
// `package main` binaries and each owns its own wiring; a
// shared adapter would force one of them to import the
// other. The struct shape is identical (mirror discipline:
// change one, change the other).
//
// # Why the adapter is needed
//
// pkg/gateway/leader.LeaderStore expects ListActiveComputeNodes
// returning the narrow leader.ComputeNode (Name + NodeID +
// Active). pkg/state.PgStore exposes ActiveComputeNodes
// returning the wide state.ComputeNode (14 fields). The
// adapter projects wide → narrow so the orchestrator doesn't
// need to know about the per-node RAM/VCPU/region/etc that
// the redirect layer doesn't read.
//
// The resolver itself is built on top of the store via
// writegate.NewCachedLeaderResolver; this file is just the
// bridge.
package main

import (
	"context"

	"github.com/onebox-faas/faas/pkg/gateway/leader"
	"github.com/onebox-faas/faas/pkg/state"
)

// computeNodeLister is the narrow surface the adapter needs.
// pkg/state.PgStore satisfies it (so does state.MemStore).
// We keep the surface narrow so test stubs don't have to
// implement the entire state.Store interface (hundreds of
// methods).
type computeNodeLister interface {
	ActiveComputeNodes(ctx context.Context) ([]state.ComputeNode, error)
}

// leaderStoreAdapter implements leader.LeaderStore by
// calling ActiveComputeNodes and projecting wide → narrow.
type leaderStoreAdapter struct {
	store computeNodeLister
}

// newLeaderStoreAdapter wraps a computeNodeLister in a
// leader.LeaderStore. Returns nil if store is nil (lets
// runWithDeps skip election wiring in single-node tests).
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

// Compile-time guard: leaderStoreAdapter must implement
// leader.LeaderStore. If the interface drifts, this line
// breaks the build of cmd/gatewayd-internal.
var _ leader.LeaderStore = leaderStoreAdapter{}
