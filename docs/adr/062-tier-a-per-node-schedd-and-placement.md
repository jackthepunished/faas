# ADR-062 · Tier A: per-node schedd + schedd-side async placement claim

- **Status:** proposed
- **Date:** 2026-08-01
- **Issue:** Phase 2 / Gate A — multi-host equality for schedd; the last
  mile after Tier 1 (mTLS + OCI) and Tier A (live capacity +
  VMMRouter + per-node vCPU)
- **Decision:** N peer-equal schedds run against one Postgres, each owns
  a slice of apps via `apps.node_id`, and the placement chooser moves
  out of `apid` into a new `pkg/sched/PlacementClaimSubscriber` that
  reacts to `NotifyAppChanged`. apid writes apps with `node_id = NULL`;
  schedd stamps the owner atomically.

## Context

PR #457 (Tier 1) shipped mTLS + OCI storage + per-host egress. PRs
#496/#499/#502 (Tier A) shipped live capacity + VMMRouter + per-node
vCPU. Phase 2 / Gate A closes the last mile:

- **Per-schedd peer equality.** Today a single schedd owns the fleet;
  scaling beyond one box means more schedd instances, but two schedds
  would today race on every wake, park, cron, and prime.
- **App ownership.** Today every schedd is eligible for every app.
  With N schedds, that's an N-fold duplicate fan-out.
- **Gateway-to-owner routing.** Today `gatewayd` dials a single
  schedd socket regardless of where the app's owner lives.

The architectural pivot in mid-implementation: the placement chooser
was originally planned inside `cmd/apid`. The depguard rule
`apid-control-plane-only` (`.golangci.yml:36-58`) forbids `apid`
from importing `pkg/sched` — scheduling is the schedd's job, not
the control plane's. The chooser moved to schedd; apid inserts with
`apps.node_id = NULL` and a new `pkg/sched/PlacementClaimSubscriber`
atomically stamps the owner on the first `NotifyAppChanged`
`kind="created"` event.

## Decision

### Topology

N peer-equal schedds, one per non-`default-local` active compute
node. No leases, no leader election, no advisory locks, no consensus.

### App ownership

Durable `apps.node_id` FK to `compute_nodes(id)` (migration 00083),
set once at claim time, **immutable post-claim** (no rebalance in
this gate). Nullable at insert time so schedd's claim subscriber
can stamp it asynchronously (migration 00084).

### Placement scheduler — schedd-side async claim (NOT in apid)

The depguard rule `apid-control-plane-only` forbids apid from
importing `pkg/sched`. The mid-PR switch away from in-apid
placement honours this:

- `cmd/apid/placement.go` deleted.
- `pkg/state` gains `ListUnplacedApps` (cold-start sweep input) and
  `SetAppNodeID` (atomic `UPDATE … WHERE node_id IS NULL`).
- `pkg/sched.Engine` gains `ClaimUnplaced(ctx, appID)` that
  re-reads the app, runs `choosePlacementLocked`, and stamps the
  owner. Conditional UPDATE serialises N schedds into exactly one
  winner; losers receive `state.ErrConflict` and drop silently.
- `pkg/sched.PlacementClaimSubscriber` consumes
  `NotifyAppChanged` filtered to `kind="created"`. Filters out
  `kind="claimed"` (its own re-emit) to avoid re-entry.
- `cmd/schedd` wires the subscriber as the 6th `subscribeXxx` seam
  and runs a one-shot cold-start sweep at boot (pg_notify is
  fire-and-forget; the sweep closes the missed-event window).

### Endpoint discovery

`compute_nodes` gains `schedd_target_url` (defaulted on
`default-local`). Distinct from `target_url` which is the vmmd
target. Gateway uses this to dial the owner schedd; apid does not
call schedd at runtime.

### Notification filtering

`app_changed`, `deployment_changed`, `snapshot_prime` stay
broadcast; each schedd filters by `app.node_id == OwnerNodeID`
**before any engine call** (closes the duplicate-`Prime` hazard).
The new placement-claim subscriber is the **first** consumer to
filter on payload *content* (`kind="created"` → row exists with
`node_id NULL`) rather than `Engine.OwnerNodeID()`.

### Per-schedd identity

`cfg.NodeName` resolves at startup to `compute_nodes.id` via
`ActiveComputeNodes`. Startup fails if `NodeName` is empty in
multi-node mode, or matches no active row, or matches `default-local`
while any non-`default-local` is active.

### No leader election

Deliberate. Every schedd owns a fixed slice of apps; the
conditional UPDATE is the consensus primitive. Adding leases,
advisory locks, or a leader would be a single point of failure and
contradict the multi-box posture.

### Sticky-warm

Stays internal; not on the wire; not on `Placement`. Unchanged.
Not consumed by `ClaimUnplaced` either (create-time has no warm
hint).

### vCPU placement input

Ledger-only `usedVCPU`. Do not consume `VCPUBusy` from live
capacity reports in this gate. The claim path passes an empty
`usedVCPU` map (the new app contributes 0 vCPU at create-time).

### Proto

Unchanged. ADR-016 (additive-only) is the rule; ownership is
enforced by reading `apps.node_id` / `instances.node_id` server-side
and returning `codes.FailedPrecondition` on mismatch.

## Consequences

- New surfaces:
  - `pkg/sched.Engine.ClaimUnplaced`
  - `pkg/sched/placement_claim.go` + `pkg/sched/placement_claim_test.go`
  - `pkg/state.Store.ListUnplacedApps` + `pkg/state.Store.SetAppNodeID`
  - `cmd/schedd` 6th subscribe seam + cold-start sweep
  - `cmd/apid/handlers.go` emits `NotifyAppChanged kind="created"` after
    every successful `CreateAppIfUnderQuota`
  - `cmd/apid/handlers_decompose.go` emits one `NotifyAppChanged
    kind="created"` per workload after `ApplyProjectPlan`
  - `compute_nodes.schedd_target_url` column (migration 00083)
  - `cmd/gatewayd/scheddrouter.go` per-node dial cache (already shipped)
- New migrations: `00083_apps_node_shard.sql` + `00084_apps_node_claimable.sql`.
- New tests: 5 in `00084_*_test.go`, 7 in `pkg/sched/placement_claim_test.go`.
- Migration 00083's test flipped `is_nullable="NO"` → `"YES"` to match
  the post-00084 contract. Flagged in the PR description.

## Open follow-ups (deliberately deferred)

- App rebalance across nodes.
- Consuming `VCPUBusy` from live capacity reports in placement (v1.1
  ADR if telemetry justifies it).
- Off-host Postgres backup (issue #250).
- Cross-node snapshot layer de-duplication (Tier 1 Phase 3
  follow-up).
- Active-passive control-plane HA is orthogonal; covered in the
  rewritten `docs/runbooks/gate-a.md`.

## Rejected alternatives

- **In-apid placement chooser** (original plan). Rejected by
  `apid-control-plane-only` depguard: scheduling is schedd's job.
- **Advisory-lock-based claim.** Rejected: adds a coordination
  surface for no win; the conditional UPDATE is simpler.
- **Leader-elected schedd.** Rejected: contradicts the multi-box
  posture; the entire point is that any schedd can fail without
  losing reachability.
- **Pre-claim at createApp via a synchronous schedd RPC.** Rejected:
  makes apid depend on schedd reachability at write time; the
  notify-driven async path keeps the control plane decoupled.