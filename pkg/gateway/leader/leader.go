// Package leader implements the lex-min leader election used by
// `gatewayd-public` for the Tier A8 active-passive HA topology
// (ADR-083, §14 M8 row "Gate-A runbook (2nd box active-passive)").
//
// The election is **pure** — given the same `[]ComputeNode` slice
// it returns the same `Leader`. The package holds no state of its
// own; every state transition (drain, DNS handoff, gauge flip)
// lives in `cmd/gatewayd-public`. The store is the existing
// `pkg/state` surface that already powers `pkg/sched/placement.go`
// (no new SQL — the `idx_compute_nodes_active` partial index
// covers the read path).
//
// # Algorithm
//
//  1. Filter the input to nodes where `Active == true`. A drained
//     node (`active = false`) never wins the election; this is the
//     precondition for ADR-064's "let the node drain before
//     shutting down" pattern.
//  2. If the filtered slice is empty, return `Leader{}` (no
//     winner) and a nil error. Callers must treat empty as
//     "no active peers" — the metric `StandbyState` stays at
//     `Warming` and the alert rule `FaasStandbyStateWarmingTooLong`
//     fires after 60 s.
//  3. If exactly one node is active, that node wins.
//  4. Otherwise, the lex-min over `Name` wins. `Name` is
//     operator-readable and stable across reboots (UUIDs are not
//     operator-readable, and the natural DNS label is
//     `faas-<name>.example.com`).
//
// # Concurrency
//
// The function is called from `cmd/gatewayd-public/main.go` on
// boot and on every `compute_node_changed` pg_notify event. It
// holds no locks; callers serialize by holding their own
// leaderState mutex. The store interface accepts `context.Context`
// for cancellation; the election itself does no I/O.
package leader

import (
	"context"
	"sort"
	"time"
)

// Leader is the result of an election. Zero-value (empty Name +
// NodeID) means "no active peer".
type Leader struct {
	// Name is `compute_nodes.name`. Operator-readable, stable
	// across reboots, the natural DNS label.
	Name string
	// NodeID is `compute_nodes.id` (UUID). Used for ledger /
	// placement surfaces that key on UUID.
	NodeID string
	// Elected is the wall-clock time at which this election was
	// committed. Zero on no-winner.
	Elected time.Time
}

// ComputeNode is the minimum surface the election needs from
// `compute_nodes`. Mirrors `pkg/state.ComputeNode` without taking
// a hard dependency on it (the test fixture builds a slice
// directly).
type ComputeNode struct {
	Name   string
	NodeID string
	Active bool
}

// LeaderStore is the surface `ElectLeader` reads from. The
// production implementation is `pkg/state.PGStore` (already wired
// into `gatewayd-public`); tests pass an in-memory fake.
type LeaderStore interface {
	ListActiveComputeNodes(ctx context.Context) ([]ComputeNode, error)
}

// ElectLeader returns the lex-min active node. Pure function;
// given the same store contents it returns the same Leader. The
// store is consulted once per call.
//
// Returns:
//   - (Leader{}, nil) on empty filtered input (no active peer).
//   - (Leader, nil) on a successful election.
//   - (Leader{}, err) only if the store fails — callers MUST treat
//     this as "election aborted, retry on next pg_notify" and
//     NOT bump the dns_flipped counter.
func ElectLeader(ctx context.Context, store LeaderStore) (Leader, error) {
	if err := ctx.Err(); err != nil {
		return Leader{}, err
	}
	nodes, err := store.ListActiveComputeNodes(ctx)
	if err != nil {
		return Leader{}, err
	}
	return ElectLeaderFromNodes(nodes, time.Now().UTC()), nil
}

// ElectLeaderFromNodes is the slice-in / Leader-out core of the
// election. Exported separately so the test fixture and the
// `cmd/gatewayd-public` cache can reuse it without faking the
// store surface.
//
// now is the wall-clock time the election was committed (passed
// in for deterministic tests).
func ElectLeaderFromNodes(nodes []ComputeNode, now time.Time) Leader {
	active := make([]ComputeNode, 0, len(nodes))
	for _, n := range nodes {
		if n.Active && n.Name != "" {
			active = append(active, n)
		}
	}
	if len(active) == 0 {
		return Leader{}
	}
	// Stable sort by Name — defensive against an unordered
	// underlying scan. Lex-min tie-break is naturally stable on
	// unique names.
	sort.Slice(active, func(i, j int) bool {
		return active[i].Name < active[j].Name
	})
	winner := active[0]
	return Leader{
		Name:    winner.Name,
		NodeID:  winner.NodeID,
		Elected: now,
	}
}

// IsZero reports whether the Leader has no winner. Convenience
// for `if l.IsZero() { /* no active peer */ }` at call sites.
func (l Leader) IsZero() bool {
	return l.Name == ""
}