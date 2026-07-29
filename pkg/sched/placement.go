// placement.go — schedd's compute-node placement chooser (issue #97 / ADR-025
// axis 3, scale-out worktree).
//
// schedd is the single leader of placement (ADR-025: single-leader CP, no
// consensus). ChoosePlacement is the pure-function core: given the live fleet
// (every active compute_node row from pkg/state) plus a snapshot of how much
// RAM each node is currently holding (from ComputeNodeUsedMB), pick the node
// that should host the next wake. The Engine wraps this with a thin layer
// that fetches the live data; the chooser itself is unit-testable in
// isolation (placement_test.go).
//
// Why a pure function:
//   - The decision is O(N) over the active set with a deterministic tie-break
//     (lexicographic name, secondary on Region/Zone from migration 00067).
//     No distributed state, no leader election, no eventual consistency —
//     single schedd process owns placement.
//   - The single-box path (one 'default-local' row with the legacy
//     47,600 MB ceiling) degenerates to "always default-local" without a
//     special case: ChoosePlacement with one active node returns that node.
//   - Testable without Postgres or KVM: the test table is a literal slice
//     of ComputeNode + a map of used_mb, exactly what ComputeNodeUsedMB
//     returns from PG/MemStore.
//
// Affinity (ADR-025, sticky-warm):
//   - r.PreferredNodeID is a hint, not a gate. If the preferred node still
//     has headroom, the chooser returns it (warm snapshot + page cache
//     benefit per ADR-009). If not, falls through to least-loaded.
//   - Hint source is pkg/sched.WarmAffinity (in-memory TTL cache); the
//     engine reads LastWarmNode(AppID) before calling ChoosePlacement.
//   - Cold-boot path (ADR-005) is preserved: an empty hint falls through
//     to the same least-loaded path a fresh install would take.

package sched

import (
	"fmt"

	"github.com/onebox-faas/faas/pkg/api"
	"github.com/onebox-faas/faas/pkg/state"
)

// Placement is the chosen compute node for one admit. Carries the dial
// target so the wake loop doesn't need a second lookup against the
// compute_nodes table.
type Placement struct {
	NodeID    string
	Name      string
	TargetURL string // wire.ParseTarget-compatible (unix://|tcp://|dns://)
	// CeilingMB is the per-node RAM admission ceiling
	// (compute_node.admission_ceiling_mb). The chooser already verified
	// the request fits; downstream code reads this to log context.
	CeilingMB int
	// UsedMB is the live Σ(ram_mb + PerVMOverheadMB) on the chosen node
	// AT THE TIME OF THE CHOICE, BEFORE this request is added. It is
	// informational — the engine's per-node ledger keeps the canonical
	// post-admit count. Tests use it to assert the tie-break.
	UsedMB int64
}

// ChoosePlacement returns the node with the most free RAM headroom that
// still fits the request, or a *api.Problem if no node can. Tie-break order:
// (headroom DESC, region ASC, zone ASC, name ASC). Pure function: no
// Engine/Ledger coupling, no DB access.
//
// Inputs:
//
//   - nodes: every active compute_node from ActiveComputeNodes. Inactive
//     rows are filtered here (placement skips drained nodes; an operator
//     flips Active=false to drain without deleting the row).
//   - usedMB: live Σ(ram_mb + PerVMOverheadMB) per node ID, from
//     ComputeNodeUsedMB. The map may be sparse (a node with no
//     instances is just absent); missing keys are treated as 0.
//
// The request's billable RAM is api.BillableRAMMB(r.RAMMB) — the +8 MB
// overhead (spec §4.7) is part of the per-node headroom check, mirroring
// the per-instance accounting the ledger enforces.
//
// Sticky-warm affinity (r.PreferredNodeID): when set and the preferred node
// has headroom, return it directly. When set but the preferred node is
// saturated or absent, fall through to the least-loaded path. Affinity
// never overrides the headroom invariant (ADR-005).
func ChoosePlacement(nodes []state.ComputeNode, usedMB map[string]int64, r Request) (Placement, error) {
	if r.RAMMB <= 0 {
		return Placement{}, api.ErrCapacity(fmt.Sprintf("placement: request RAM must be positive (got %d)", r.RAMMB))
	}
	billable := int64(api.BillableRAMMB(r.RAMMB))

	// First pass: filter to candidates that fit, capture the warm
	// hint if it fits. We keep this single-pass because N is small
	// (single-digit fleet for v1.0, see cmd/apid/compute_nodes.go
	// comments) and a separate filter pass would duplicate work.
	var (
		candidates []state.ComputeNode
		warmFit    *state.ComputeNode
	)
	for i := range nodes {
		n := nodes[i]
		if !n.Active {
			continue
		}
		if n.AdmissionCeilingMB <= 0 {
			continue
		}
		used := usedMB[n.ID]
		if used+billable > int64(n.AdmissionCeilingMB) {
			continue // this node can't fit the request
		}
		candidates = append(candidates, n)
		if r.PreferredNodeID != "" && n.ID == r.PreferredNodeID {
			// Capture by value (small struct) so the warmFit
			// pointer doesn't alias a loop variable that the
			// sort below mutates.
			nCopy := n
			warmFit = &nCopy
		}
	}

	if warmFit != nil {
		return Placement{
			NodeID:    warmFit.ID,
			Name:      warmFit.Name,
			TargetURL: warmFit.TargetURL,
			CeilingMB: warmFit.AdmissionCeilingMB,
			UsedMB:    usedMB[warmFit.ID],
		}, nil
	}

	if len(candidates) == 0 {
		return Placement{}, api.ErrCapacity(fmt.Sprintf(
			"placement: no active compute_node fits %d MB billable across %d candidates (per-node ceilings: see compute_nodes.admission_ceiling_mb)",
			billable, len(nodes)))
	}

	// Single best candidate — short-circuit the sort.
	if len(candidates) == 1 {
		n := candidates[0]
		return Placement{
			NodeID:    n.ID,
			Name:      n.Name,
			TargetURL: n.TargetURL,
			CeilingMB: n.AdmissionCeilingMB,
			UsedMB:    usedMB[n.ID],
		}, nil
	}

	// Pick by (headroom DESC, region ASC, zone ASC, name ASC).
	//
	// Region/Zone are *string; treat nil and "" identically so a
	// pre-00067 row (nil pointers) sorts the same as an operator-
	// inserted row with empty strings. The seeded default-local row
	// is backfilled to ('local','local') in migration 00067 so the
	// single-box deploy has a deterministic ordering.
	best := candidates[0]
	for _, n := range candidates[1:] {
		if betterCandidate(n, usedMB[n.ID], best, usedMB[best.ID]) {
			best = n
		}
	}
	return Placement{
		NodeID:    best.ID,
		Name:      best.Name,
		TargetURL: best.TargetURL,
		CeilingMB: best.AdmissionCeilingMB,
		UsedMB:    usedMB[best.ID],
	}, nil
}

// betterCandidate returns true if `n` should replace `best` per the
// tie-break ordering in ChoosePlacement. Pure helper so placement_test.go
// can exercise the comparator without spinning up Engine + Ledger.
//
// The ordering is the load-bearing contract — placement_test.go pins it.
// Changing this function changes where hot apps land; never edit without
// reading the test cases.
func betterCandidate(n state.ComputeNode, nUsed int64, best state.ComputeNode, bestUsed int64) bool {
	nHead := int64(n.AdmissionCeilingMB) - nUsed
	bestHead := int64(best.AdmissionCeilingMB) - bestUsed
	if nHead != bestHead {
		return nHead > bestHead
	}
	// Region/Zone are nullable strings; collapse nil → "" so the
	// comparator sees a single shape. Tied on headroom → prefer
	// lower region, then lower zone, then lower name. The seeded
	// default-local row is backfilled to ('local','local') in
	// migration 00067, so single-box deploys see a deterministic
	// ordering with no operator-added rows competing.
	nRegion := derefRegion(n.Region)
	bestRegion := derefRegion(best.Region)
	if nRegion != bestRegion {
		return nRegion < bestRegion
	}
	nZone := derefRegion(n.Zone)
	bestZone := derefRegion(best.Zone)
	if nZone != bestZone {
		return nZone < bestZone
	}
	return n.Name < best.Name
}

func derefRegion(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
