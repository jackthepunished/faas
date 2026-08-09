# ADR-058 · schedd honours live capacity + per-node vCPU budget (Tier A)

- **Status:** accepted v1.0 (2026-08-09). Tier A1 (`Engine.applyLiveCapacityMB`
  with ledger-floor) and Tier A2 (per-node `compute_nodes.vcpu_budget`,
  `Request.VCPUBudget`, chooser `usedVCPU` map) both landed; this flip retires
  ADR-025 v1.1's "Tier 1 Phase 2" pre-requisite together with ADR-053.
- **Date:** 2026-08-01
- **Issue:** multi-box audit, Tier A gaps (engine chooser never reads live
  capacity table; vCPU reported but not enforced in placement)
- **Decision:** Bind schedd's chooser to the live capacity publisher and
  move the vCPU admission gate from box-wide to per-node. Two adjacent
  load-bearing fixes that share a wake path and a fleet-shape model.

## Context

The multi-box rollout (Tier 1) shipped the per-node live-capacity table
and the per-node `CapacityReport` stream in ADR-053 + ADR-052, but two
audit gaps remain on the consumer side. Both live inside schedd's
admission/placement pipeline and would let a fleet of N boxes silently
make wrong placement decisions or over-admit on one box.

### Tier A1 — engine chooser never reads the live capacity table

`pkg/sched/capacity.go:20-21` documents `applyLiveCapacityMB` as the
publisher→chooser binding. `pkg/sched/engine.go:178-208` carries the
field. The function does not exist (`git grep applyLiveCapacity` returns
only the docstring and the field reference). The chooser at
`pkg/sched/engine.go::choosePlacementLocked` calls only
`e.store.ComputeNodeUsedMB(ctx, n.ID)` — the stale Σ(ram_mb + 8) sum of
`instances` rows. On a multi-box fleet, a node whose actual cgroup
`memory.current` exceeds `AdmissionCeilingMB` is still chooser-eligible
and the next admit lands there. This is the inverse of invariant §6.2-2:
the live table has fresher truth than the store sum, but the chooser
silently ignores it.

### Tier A2 — vCPU is reported, not enforced in placement

`CapacityReport.VCPUBusy` is filled (`cmd/vmmd/capacity_publisher.go:369`
as `live * 2`) and shipped on the wire, but admission enforces vCPU
only as a **box-wide** check against `api.VCPUSlots = 160` at
`pkg/sched/admission.go:175-178` (pre-A2). `totalUsedVCPU_locked` at
admission.go:217-223 summed across all nodes. On a 5-node fleet, one box
could admit 160 vCPU and starve the other four. `compute_nodes.vpcpus`
exists on the schema (migration 00024) but nothing read it for
admission. The per-node RAM ceiling (also from 00024) is already
enforced via `Request.NodeCeilingMB`; the vCPU side needs the same
treatment.

## Decision

### Tier A1 — bind chooser to the live publisher, ledger-floored

Add `Engine.applyLiveCapacityMB(ctx, nodeID) int64` in `pkg/sched/engine.go`
above `choosePlacementLocked`. Returns `max(live.UsedMB, ledger.ResidentRAMForNode)`
when the report is fresh (`CapacityFreshness = 5s`), else returns 0 as a
sentinel that the caller turns into a fall-through to the store sum.

The ledger-floor is the load-bearing correctness rule: a vmmd that lies
about being empty cannot bias the chooser toward itself past its
**actual** resident bytes (its own instances table). The live report wins
when it reports a higher number than the ledger — the typical case is
"vmmd's cgroup sees guest RAM the schedd instance table doesn't yet
know about" (a guest in COLD_BOOTING mid-`CreateInstance`).

The chooser falls through to `store.ComputeNodeUsedMB` when:

- the live table has no entry for the node (new node, no report yet);
- the live entry is stale (`time.Now() - LastSeen > CapacityFreshness`);
- `applyLiveCapacityMB` returns 0 (the ledger is empty AND the live
  report claims 0 — the "fresh zero" path, identical to the store).

The legacy error path inside the chooser loop is preserved: a transient
store failure on a single node logs a warning and uses 0, so a single
node's failure does not block placement on others.

### Tier A2 — per-node vCPU budget

**Schema.** New migration `00123_compute_nodes_vcpu_budget.sql` adds
`vcpu_budget INT NOT NULL DEFAULT 160 CHECK (vcpu_budget > 0)` to
`compute_nodes`. Default 160 matches `api.VCPUSlots` and the legacy
single-box posture. The migration is replay-safe (PR #377 / ADR-041):
`ADD COLUMN IF NOT EXISTS` makes a second MigrateUp a no-op. The
default-local row seeded by migration 00024 backfills via the column
default with no operator action.

**State.** Add `VCPUBudget int` to `state.ComputeNode`
(`pkg/state/types.go`). All `pgstore` SELECT/INSERT/UPSERT statements
that project the row carry the new column. `state.MemStore.seedDefaultLocalNodeLocked`
seeds `VCPUBudget: api.VCPUSlots` on the synthetic default-local row
so the in-memory test fleet also behaves correctly.

**Admission.** Replace the box-wide `api.VCPUSlots` gate with a per-node
check paralleling the RAM gate. The `Request` struct gains `VCPUBudget int`
alongside `NodeCeilingMB`. The check is the inverse of the RAM pattern:

```go
ceiling := r.VCPUBudget
if ceiling <= 0 {
    ceiling = api.VCPUSlots // safe fallback for un-registered nodes
}
if node.usedVCPU+r.VCPU > ceiling {
    return api.ErrCapacity(fmt.Sprintf(
        "vCPU headroom: node %q busy %d + %d requested exceeds the %d per-node vCPU budget",
        r.NodeID, node.usedVCPU, r.VCPU, ceiling))
}
```

`totalUsedVCPU_locked` (the box-wide sum) becomes informational. The
per-node lookup inside `Admit` is the load-bearing enforcement; the
public `UsedVCPU` accessor still exists for telemetry/observability.

**Placement.** `ChoosePlacement` gains `usedVCPU map[string]int64` as a
4th parameter. The candidate filter loop adds a vCPU fit check parallel
to the RAM one: skip if `n.VCPUBudget <= 0` (defensive — the migration
default is 160) or `usedVCPU[n.ID]+int64(r.VCPU) > int64(n.VCPUBudget)`.
`betterCandidate` extends from 4 to 6 args with a secondary vCPU
headroom tie-break: `(headroom DESC, vcpu_headroom DESC, region ASC,
zone ASC, name ASC)`. `Placement` carries `VCPUBudget int` for symmetry
with `CeilingMB`.

**Engine wiring.** `Engine.choosePlacementLocked` builds `usedVCPU` from
`e.ledger.UsedVCPUForNode(n.ID)` (Tier A2 accessor) paralleling the
`applyLiveCapacityMB` map construction. The Engine threads
`VCPUBudget: placement.VCPUBudget` into the wake and admit `Request`s
at three call sites (Wake, Prime, SeedLedger recover).

## Backwards compatibility

Single-box installs are bit-for-bit unchanged:

- The synthetic default-local row carries `vcpu_budget=160` (the
  migration default + the memstore seed). The pre-A2 box-wide
  `api.VCPUSlots` gate is identically enforced through the per-node
  fallback (`r.VCPUBudget <= 0 → api.VCPUSlots`).
- The chooser with one node and no live report falls through to the
  store sum (legacy path). With a live report (the production case on
  a healthy vmmd), the chooser picks that one node because the
  tie-break is degenerate.
- The placement comparator's vCPU tie-break never fires in a single-node
  fleet (only one candidate).

Multi-box installs gain:

- The live capacity table is honoured per-node. A vmmd reporting
  high `UsedMB` becomes chooser-rejected even if its `instances` table
  is empty (e.g. a guest whose `CreateInstance` write hasn't landed yet).
- Per-node vCPU budgets. A fleet of N nodes with `vcpu_budget=160` can
  collectively admit `160 * N` vCPU (not capped at 160 globally).

## Why ledger-floor on the live report

A live report claiming `UsedMB=0` from a vmmd whose instances table has
residents must not bias the chooser toward that node. The ledger-floor
(`max(live, ledger)`) closes the gap: the ledger is the local truth
authoritative for instances schedd has admitted, and the live report is
the truth about cgroup pressure we haven't yet recorded. Picking the
max means both directions of skew are bounded — under-reporting
collapses to the ledger; over-reporting collapses to the live number
(vmmd's cgroup really is at that pressure and the next admit lands
elsewhere, which is correct).

The trade-off is a brief window where ledger > live (e.g. a guest was
parked: ledger released, cgroup freed). The next `ReportCapacity`
cycle (~1s) closes the window. ADR-005's "wake must always work"
invariant is unaffected — a request that fits at the store level still
fits at the ledger floor.

## Verification

Unit tests (no KVM):

- `pkg/sched/capacity_engine_test.go` — `applyLiveCapacityMB` floor
  semantics: live=100/ledger=200 → 200; live=200/ledger=100 → 200;
  stale → sentinel; nil receiver → sentinel.
- `pkg/sched/placement_live_test.go` — end-to-end through
  `choosePlacementLocked`: a fresh live report forces the chooser to
  pick the node the report says has headroom; a stale entry falls back
  to the store sum and the resident accounting surfaces correctly.
- `pkg/sched/placement_vcpu_test.go` — per-node vCPU fit check,
  `vcpu_budget=0` exclusion, vCPU headroom secondary tie-break,
  legacy single-box posture preserved bit-for-bit.
- `pkg/sched/admission_vcpu_test.go` — per-node enforcement,
  `VCPUBudget=0` fallback, RAM + vCPU as independent gates.
- `migrations/00123_compute_nodes_vcpu_budget_test.go` — column shape
  + CHECK constraint + default + replay-safety + down→up round-trip.

Integration:

- `make test` — unit suite, must pass.
- `make test-metal` (Lima nested KVM on Apple Silicon) — exercises
  `pkg/fcvm` + the resume hook + the capacity publisher end-to-end.
- `make leakcheck` — no leaked netns/TAPs/cgroups.
- `make lint` + `make spec-check` — golangci-lint + the spec/limit
  drift gates.

## Operational notes

- The bootstrap upsert (`cmd/vmmd/main.go`) and the synthetic default-local
  memstore seed both set `vcpu_budget=160` — no operator action needed
  on existing boxes.
- Operators tuning a heterogeneous fleet set per-row `vcpu_budget` via
  `PUT /v1/compute-nodes/{id}` (post-API) or directly in
  `compute_nodes`. The financial model §1's 8× CPUOvercommit ratio
  (160 vCPU from 20 physical cores) is the recommended starting point
  for a reference control-plane node.
- A `vcpu_budget=0` row is rejected by the CHECK constraint at the DB
  layer; a `Request.VCPUBudget=0` falls back to `api.VCPUSlots` at the
  ledger layer (defensive net for un-registered nodes / test seams).

## Related ADRs

- ADR-025 axis 5 (placement primitives + sticky-warm hint).
- ADR-053 (per-node live-capacity table + node_signature).
- ADR-052 (mTLS + handler-layer peer binding for vmmd→schedd transport).
- ADR-041 (migration replay-safety pattern).