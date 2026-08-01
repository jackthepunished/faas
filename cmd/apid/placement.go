package main

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/onebox-faas/faas/pkg/api"
	"github.com/onebox-faas/faas/pkg/sched"
	"github.com/onebox-faas/faas/pkg/state"
)

// PlacementScheduler picks the durable owner compute_node for a
// new app at CreateApp time. Phase 2 / Gate A — apid is the only
// writer to `apps` and the natural place for the placement
// decision; the schedd side consumes the persisted apps.node_id
// via the ownership guard in pkg/scheddgrpc.
//
// Constructed once at server boot (cmd/apid/server.go) and shared
// across handlers. Holds a state.Store for the per-node used-MB
// reads; the Store interface is the live one, so the unit tests
// can inject a MemStore without Postgres.
//
// ADR-058 / ADR-025 axis 5: this chooser runs at app-create time,
// before any instance of the new app has woken. Per-node vCPU
// headroom is therefore not consulted here — at create-time the
// new app contributes zero vCPU, and the schedd's per-instance
// NodeLedger is the runtime gate (pkg/sched/admission.go) that
// refuses to admit the first instance if the chosen node's
// vcpu_budget is full. The live capacity VCPUBusy signal is
// signed but not consumed by placement; a v1.1 ADR can revisit
// if telemetry shows it matters. Sticky-warm (PreferredNodeID)
// is also intentionally not threaded through here — the chooser
// operates at create-time, before any instance has woken, so
// there is no warm hint to bias. Warm-affinity stays inside the
// schedd Engine's Wake path (pkg/sched/engine.go).
type PlacementScheduler struct {
	store state.Store
	log   *slog.Logger
}

// NewPlacementScheduler wires the scheduler against the live
// state.Store. Returns nil when store is nil (tests that don't
// exercise placement — server_test.go's legacy paths — get a no-op
// and the placement call site is skipped).
func NewPlacementScheduler(store state.Store, log *slog.Logger) *PlacementScheduler {
	if store == nil {
		return nil
	}
	if log == nil {
		log = slog.Default()
	}
	return &PlacementScheduler{store: store, log: log}
}

// Choose resolves an owner compute_node for a brand-new app.
//
// Inputs:
//   - appID: the freshly-minted apps.id (used only for the
//     sched.Request log tag; the chooser is pure on the data
//     inputs below).
//   - ramMB: the app's effective ram_mb (already validated against
//     the plan cap by the handler). The chooser treats this as
//     the billable footprint; +8 MB overhead (spec §4.7) is part
//     of the per-node headroom check inside sched.ChoosePlacement.
//
// Output:
//   - state.ComputeNode: the chosen owner. The caller stamps
//     `app.NodeID = chosen.ID` before persisting.
//   - *api.Problem: a capacity problem (RFC 7807) when no node
//     has headroom. The handler surfaces this directly to the
//     customer via WriteProblem — the customer sees the same
//     "Briefly at capacity" envelope the pre-Phase-2 chooser
//     emitted. Other errors (DB outage, store misconfig) are
//     surfaced as a generic 500.
//
// Notes on placement inputs:
//
//   - Active nodes: state.ActiveComputeNodes(ctx) — excludes
//     active=false rows (the operator drain switch).
//   - usedMB per node: state.ComputeNodeUsedMB(ctx, nodeID) —
//     summed from the live instances table (Σ ram_mb + 8 MB
//     overhead). One round-trip per node; for a fleet of N < 10
//     this stays under 5 ms total.
//   - usedVCPU per node: empty map. The vCPU ledger lives inside
//     the schedd's in-process NodeLedger (pkg/sched/admission.go);
//     apid does not call schedd at runtime (CLAUDE.md component
//     ownership: apid is the only writer to apps, schedd is the
//     only writer to instances / vCPU reservations). At app-create
//     time the new app contributes 0 vCPU anyway, so the per-node
//     vCPU headroom check at placement is degenerate — every
//     active node passes the vCPU floor and the chooser only
//     gates on RAM headroom.
//
// The chooser does NOT consult any sticky-warm hint: at
// create-time there's no instance to be sticky about. (Warm-
// affinity is the schedd Engine's job, not the apid
// placement's.)
func (p *PlacementScheduler) Choose(ctx context.Context, appID string, ramMB int) (state.ComputeNode, *api.Problem) {
	if p == nil {
		return state.ComputeNode{}, api.NewProblem(500, api.CodeInternal, "internal", "apid: placement scheduler not configured")
	}
	if ramMB <= 0 {
		return state.ComputeNode{}, api.ErrCapacity(fmt.Sprintf(
			"placement: app RAM must be positive (got %d)", ramMB))
	}
	nodes, err := p.store.ActiveComputeNodes(ctx)
	if err != nil {
		return state.ComputeNode{}, api.NewProblem(500, api.CodeInternal, "internal",
			fmt.Sprintf("list active compute_nodes: %s", err.Error()))
	}
	if len(nodes) == 0 {
		return state.ComputeNode{}, api.ErrCapacity("no active compute_nodes registered — add one via POST /v1/compute-nodes before creating apps")
	}
	usedMB := make(map[string]int64, len(nodes))
	for _, n := range nodes {
		used, err := p.store.ComputeNodeUsedMB(ctx, n.ID)
		if err != nil {
			return state.ComputeNode{}, api.NewProblem(500, api.CodeInternal, "internal",
				fmt.Sprintf("compute_node %s usedMB: %s", n.ID, err.Error()))
		}
		usedMB[n.ID] = used
	}
	// usedVCPU is intentionally empty here — see the chooser doc
	// above. sched.ChoosePlacement's VCPUBudget gate is bypassed
	// when r.VCPU=0 (admission.go precondition r.VCPU > 0), so
	// passing an empty map with VCPU=0 routes the chooser through
	// the RAM-headroom path only. The schedd's per-instance
	// NodeLedger is the load-bearing vCPU gate at runtime.
	placement, err := sched.ChoosePlacement(nodes, usedMB, map[string]int64{}, sched.Request{
		Instance: "create-" + appID,
		AppID:    appID,
		RAMMB:    ramMB,
	})
	if err != nil {
		// sched.ChoosePlacement surfaces a generic "no compute_node
		// fits" string; rewrite into the canonical ErrCapacity
		// envelope so the customer sees a stable RFC 7807 shape.
		return state.ComputeNode{}, api.ErrCapacity(err.Error())
	}
	for _, n := range nodes {
		if n.ID == placement.NodeID {
			p.log.Info("placement chosen",
				"app", appID, "node_id", n.ID, "node_name", n.Name,
				"used_mb", usedMB[n.ID], "ceiling_mb", n.AdmissionCeilingMB,
				"ram_mb", ramMB)
			return n, nil
		}
	}
	// unreachable: ChoosePlacement returned a NodeID that wasn't in
	// the input set, which would be a sched.ChoosePlacement bug.
	return state.ComputeNode{}, api.NewProblem(500, api.CodeInternal, "internal",
		fmt.Sprintf("placement returned unknown node_id %q", placement.NodeID))
}