// LeaderResolver is the gate's view of cluster leadership. PR-A
// ships the interface; PR-B wires a thread-safe implementation
// backed by `pkg/gateway/leader.ElectLeader` + the `public_ip`
// column on `compute_nodes` (added in PR-B's additive migration).
//
// The interface is intentionally narrow — three return values +
// one error — because the gate's logic is fixed:
//  1. If `name == ""` (no active peer) → OutcomeLeaderUnreachable
//     (503 Retry-After: 60).
//  2. If `isMe` (the local box IS the leader) → OutcomeSameBox,
//     fall through to local apidProxy.
//  3. Else (a different node is the leader) → transparent relay
//     (bearer) or 503 (cookie), per writeGate logic in PR-B.
//
// The interface does NOT expose the leader URL — the gate
// constructs it from `leaderPublicHost` (set once at boot from
// `compute_nodes.public_ip`) and the public egress DNS name
// (operator-managed). This separation lets the gate be tested
// without mocking full URL builders.
//
// # Concurrency
//
// `Current(ctx)` is on the request hot path. The production
// implementation MUST cache the result for
// `pkg/api/limits.go::StandbyWriteLeaderURLCacheTTLSeconds` (5 s)
// and refresh on `compute_node_changed` events — that work lives
// in PR-B's leader_url_publisher. The interface itself is
// goroutine-safe by contract; the fake in writegate_test.go is
// guarded by a sync.RWMutex.
//
// # Re-election on every call vs cached
//
// The interface returns the CACHED answer; PR-B's
// `LeaderURLPublisher` is responsible for keeping the cache fresh
// on `compute_node_changed`. The gate does NOT re-elect on every
// request — that would put a synchronous PG round-trip on every
// mutating request, which is the explicit failure mode Tier A8
// (PR #738) shipped to avoid. The 5 s TTL is the operator-visible
// tradeoff: a `compute_node_changed` event takes ≤ 5 s to
// propagate through the gate, which is well under the DNS
// failover window (`HADNSRecordStaleSeconds = 30`).
package writegate

import "context"

// LeaderResolver answers "who is the leader, and is it me?"
// for the writeGate's request classification.
//
// Implementations MUST be goroutine-safe (the gate is on every
// mutating request path). The interface deliberately exposes
// primitives rather than a rich struct: callers branch on
// `isMe` first, then read `name` for the loop-guard sentinel,
// never needing `url` for classification (the URL is built
// per-hop from operator-managed DNS).
type LeaderResolver interface {
	// Current returns the cached leader state.
	//
	//   - `name` is `compute_nodes.name` of the elected leader
	//     (operator-readable, the natural DNS label). Empty
	//     when no peer is active.
	//   - `isMe` is true when the local box IS the elected
	//     leader. False when a different node is the leader,
	//     or when `name == ""`.
	//   - `err` is non-nil only on cache-internal failures
	//     (rare — the resolver never blocks on PG; PR-B's
	//     publisher refreshes asynchronously). Callers MUST
	//     treat err as OutcomeError.
	//
	// The `ctx` is used to abort the resolver's internal read
	// (e.g. a slow DNS lookup in test fakes). The production
	// implementation respects ctx only when the cache is cold;
	// cache hits never block.
	Current(ctx context.Context) (name string, isMe bool, err error)
}
