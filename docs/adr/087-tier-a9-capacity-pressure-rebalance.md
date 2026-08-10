# ADR-087 · Tier A9: capacity-pressure-triggered cross-node app rebalance

- **Status:** accepted v1.0
- **Date:** 2026-08-10 (proposed + accepted)
- **Issue:** #297 Tier A9 (follow-up to ADR-064 Tier A4 / ADR-066 Tier A5)
- **Decision:** Add a sibling watcher to the dead-node rebalancer
  (ADR-064, `pkg/sched/rebalancer.go`) that fires on **sustained
  per-app wake-side `AtCapacity` rejection** from a healthy (not
  drained) owner node. Each pressured app has its `apps.node_id`
  atomically flipped to a peer with admission headroom, gated by:
  - per-app cooldown (`api.RebalanceCooldownSeconds`, default 60s);
  - per-app sweep persistence (`api.PressureMigrationPolicy`,
    default `migrate_after_2_sustained_sweeps`);
  - per-app live-instance migration policy (closed set
    {`skip_live`, `migrate_after_1`, `migrate_after_2`}).

  The cheap path (reassign parked-only apps) runs on the first
  sustained sweep. The expensive path (live-instance migration via
  the four-phase handoff, ADR-066) only runs on the second
  sustained sweep with the default policy. Cold-start sweep
  (`Engine.RebalancePressuredApps(ctx, "")`) catches apps that
  breached the threshold while schedd was down.

## Context

ADR-062 / Tier A shipped the per-node schedd and ownership guards
(`apps.node_id` FK, `apps.reassigned_at`, `authorizeApp`,
`choosePlacementLocked`). ADR-064 (Tier A4, dead-node rebalance)
closed the "node drained, apps left pinned" gap. ADR-066 (Tier A5,
live-instance migration) closed the "node has live instances that
need to ride along" gap.

The remaining gap is **capacity pressure on a healthy node**:

- A customer's app is durably pinned to a single compute node via
  `apps.node_id` (NOT NULL post-migration 00090).
- `pkg/scheddgrpc/ownership.go:54` (`authorizeApp`) refuses
  cross-owner wakes with `codes.FailedPrecondition`.
- `pkg/sched/engine.go:1921` (`choosePlacementLocked`) pins
  placement to the schedd's owner fleet.
- The dead-node rebalancer only fires on `compute_nodes.active=false`
  — a node at 100% capacity is still `active=true`.

The customer's take: when box 1 is full, the wake path returns 503
to that customer's app even though box 2 has headroom. The
operator's take: the customer sees a regression that didn't exist on
the single-box deployment. Tier A9 closes the gap.

## Decision

### Trigger

`pkg/sched/pressure_aggregator.go` is an in-process, per-app
sliding-window counter of `WakeResult{AtCapacity: true}` events
across all 8 engine return sites. `IncAtCapacity(appID, now)` is
called at every `AtCapacity=true` return; `PressuredApps(threshold,
now)` returns the deterministic app-id list whose count over the
last 60s window is ≥ threshold.

`pkg/sched/pressure_rebalancer.go` is a watcher that polls every
`api.PressureReassessmentIntervalSeconds` (default 30s). On each
tick it queries `aggregator.PressuredApps(threshold, now)` and calls
`Engine.RebalancePressuredApps(ctx, appID)` for each app in sorted
order. The watcher is the 5th long-running goroutine on schedd —
sibling to the rebalancer, router_watcher, instance_stats poller,
and the migrating-watchdog.

The trigger is **in-process**, not `pg_notify`-driven. Rationale:
AtCapacity is a per-call outcome, not a row-write event. A
`pg_notify` consumer would need to write every AtCapacity event to
a table first, doubling the write path. The in-process aggregator
is strictly cheaper and race-free (single schedd-internal mutex).

### Eligibility

Same as ADR-064: `apps.status ∈ {active, evicted_cold}`. A
`running` / `waking` / `cold_booting` instance on the full node is
NOT picked up by the cheap path; the policy-gated live-migration
helper picks them up only on the second sustained sweep (default
policy).

### Atomicity

`Store.ReassignAppOwner(ctx, appID, fromNodeID, toNodeID) error`
(`pkg/state/pgstore.go:2056`):

```sql
update apps
   set node_id = $3, reassigned_at = now()
 where id = $1 and node_id = $2
   and status in ('active', 'evicted_cold')
```

`RowsAffected() == 0` → `state.ErrConflict`. The `fromNodeID`
predicate makes concurrent sweeps race-safe exactly-one-wins-per-app.

### Cooldown

`api.RebalanceCooldownSeconds` (default 60s, env
`FAAS_REBALANCE_COOLDOWN_SECONDS`) suppresses repeated reassignments
of the same app. The pressure rebalancer reuses the existing
`apps.reassigned_at` column (added migration 00092).

### Migration policy

`api.PressureMigrationPolicy` is a closed-set string in the
engine-bound config:

| Value | Behaviour |
|---|---|
| `skip_live` | Parked-only reassign. Apps with `running` / `waking` / `cold_booting` instances on the owner stay pinned; the customer's wake fails until the instances drain. Cheapest; safest for transient pressure. |
| `migrate_after_1` | First sustained sweep reassigns through the cheap path; if any live instances exist on the owner, also fire the four-phase live handoff (ADR-066) for the first eligible instance. |
| `migrate_after_2` (default) | First sustained sweep: cheap path only. Second sustained sweep (within window + cooldown): also fire the four-phase live handoff for the first eligible instance. Gates flap on transient pressure. |

The policy is parsed once at schedd start and stamped through
`Engine.WithPressureMigrationPolicy`. The helper
`maybeMigrateLiveInstancesFor(ctx, app, peer)` reads the live
instance count for `app.ID` on `e.ownerNodeID` and routes the first
eligible instance through `Engine.MigrateLiveInstances` (ADR-066).

### Admission

`findPeerWithHeadroom(ctx, app)` filters
`store.ActiveComputeNodes` to `peerNodeID != e.ownerNodeID`,
computes `peer.AdmissionCeilingMB - store.ComputeNodeUsedMB(peer)`,
and selects the first peer (sorted by name ASC, deterministic) with
`headroom >= app.RAMMB + PerVMOverheadMB`. If no peer qualifies,
the rebalancer logs `outcome="no_headroom"` and skips the app — the
next sweep retries.

### `app_changed` emission

`Store.ReassignAppOwner` does NOT emit `app_changed`.
The engine-side `RebalancePressuredApps` emits via
`e.notif.Notify(ctx, db.NotifyAppChanged, ...)` immediately after
the `UPDATE` commits, with payload:

```json
{"kind":"pressure_rebalanced","app_id":"...","from_node":"...","to_node":"..."}
```

`cmd/gatewayd-internal/backend.go:275-276` (the existing
`FlushRoutes` consumer) picks this up and invalidates the per-app
target cache, so subsequent gateway requests hit the new owner.

### Cold-start sweep

A schedd that was down while sustained pressure built up recovers
via a one-shot goroutine at startup:
`Engine.RebalancePressuredApps(ctx, "")` — empty `appID` means
"every app currently above the threshold". Mirrors the post-00091
cold-start sweep at `cmd/schedd/main.go` and the dead-node
rebalancer's cold-start sweep at
`Engine.RebalanceOrphanedApps(ctx, "")`.

## Migrations

None. The pressure rebalancer reuses `apps.reassigned_at`
(migration 00092) and `apps.node_id` (migration 00090).

## Metrics

- `schedd_app_at_capacity_total{app, kind}` — CounterVec (kind ∈
  {wake, admit, scaleup, floor}). Incremented at every
  `WakeResult{AtCapacity: true}` return. The `{app}` label is
  bounded by the closed set of live app IDs (the per-call
  `IncAtCapacity` only fires while the app is hot; the
  `appAtCapacityTotal` row is short-lived per-app, the TSDB
  churns but the active series count is bounded by
  `Σ(apps where node_id == e.ownerNodeID)`).
- `schedd_pressure_reassignments_total{outcome}` — CounterVec
  (outcome ∈ {migrated, peer_live_migrated, conflict,
  no_headroom, no_eligibility, no_peer}). `migrated` is the §12
  dashboard panel; `peer_live_migrated` is the tripwire (the
  first time the policy fires the four-phase live handoff).
  `no_headroom` is the tripwire for sustained full-cluster
  pressure (call the operator).

Pre-instantiated in `NewOpsMetrics` so the rows surface in
/metrics from the moment schedd starts. Single-registry pattern:
registered on every daemon (mirrors `rebalanceDecisions`), only
schedd increments in production.

## Limits

- `api.PressureAtCapacityThresholdPerMin` (default 5; env
  `FAAS_PRESSURE_THRESHOLD_PER_MIN`) — events per 60s window
  before the app is considered pressured. A spike of 5/min on a
  customer who usually averages 1/min is a real signal; a steady
  5/min is a steady-state condition that operator should
  investigate (the customer's app is over-provisioned for the
  fleet).
- `api.PressureReassessmentIntervalSeconds` (default 30; env
  `FAAS_PRESSURE_REASSESSMENT_SECONDS`) — sweep cadence. 30s
  matches the existing rebalancer / router_watcher / cron /
  watchdog 1s/30s tick family.
- `api.PressureMigrationPolicy` (default `migrate_after_2`; env
  `FAAS_PRESSURE_MIGRATION_POLICY`) — closed-set, validated at
  cmd/schedd/main.go startup.

## Failure modes

- **All peers full.** `findPeerWithHeadroom` returns no candidate;
  metric `pressureReassignmentsTotal{outcome="no_headroom"}`
  fires. The app stays pinned; the customer sees 503 until
  capacity returns. Operator-actionable via the `NoHeadroom`
  alert (sum over 5m on `no_headroom` rate > 0).
- **Cooldown conflict.** First reassign succeeds; second sweep
  within 60s sees `apps.reassigned_at < now() - 60s` → drops with
  `outcome="cooldown"`. The aggregator counters accumulate
  harmlessly; the third sweep retries.
- **Peered-schedd app on the owner.** If a peer schedd's app is
  the one being pressured (the gateway routed to a remote schedd
  via the gateway-internal split), the engine observes
  `app.NodeID != e.ownerNodeID` and silently drops. No false
  reassign.
- **Live-migration conflict.** The four-phase handoff (ADR-066)
  has its own rebalance cooldown; a `peer_live_migrated` outcome
  paired with the migration handoff's `liveMigrationDecisions.
  {outcome="conflict"}` is the expected mid-flight regression
  path, not a Tier A9-specific bug.

## Open follow-ups (deliberately deferred)

- **Per-app `overflow_node_id`** (option E in the earlier write-up)
  — customer-facing knob for explicit spill target. Requires a new
  column + new apid/schedd surface. Separate slice.
- **Hetzner / DO API auto-provisioning** (option D) — separate
  slice; requires IaC surface the team hasn't built yet.
- **Per-tier pressure thresholds** — global default is fine for v1.
- **Multi-peer peer-selection policy** beyond first-with-headroom
  (name-ASC sort) — sufficient for v1.

## Rejected alternatives

- **pg_notify-driven trigger.** Requires writing every AtCapacity
  event to a table first, doubling the write path. The in-process
  aggregator is strictly cheaper and race-free.
- **Advisory locks around the whole pressured set.** The
  `WHERE node_id = $from` predicate already provides exactly-one-
  wins-per-app. A second-tier defence would add nothing.
- **Skip the cheap path; always live-migrate.** Cost-prohibitive
  on transient pressure (a 1-second spike would trigger a 90s
  four-phase handoff). The two-sweep policy gate is the right
  trade-off.
- **Auto-provision a new compute node on `no_headroom`.**
  Out of scope; requires IaC surface (option D).

## Implementation

- `pkg/sched/pressure_aggregator.go` (new) +
  `pkg/sched/pressure_aggregator_test.go`.
- `pkg/sched/pressure_rebalancer.go` (new) +
  `pkg/sched/pressure_rebalancer_test.go`.
- `pkg/sched/engine.go` — struct fields, `WithPressureConfig` +
  `WithPressureAggregator` setters, `IncAtCapacity` calls at the 8
  AtCapacity return sites, `RebalancePressuredApps` +
  `findPeerWithHeadroom` + `maybeMigrateLiveInstancesFor` methods,
  `IncrementPressureSweepCounter` accessor.
- `pkg/sched/pressure_rebalance_engine_test.go` (new).
- `pkg/wire/metrics.go` — `appAtCapacityTotal` +
  `pressureReassignmentsTotal` + accessors.
- `pkg/api/limits.go` — three new tunables.
- `cmd/schedd/main.go` — env-parse, aggregator wiring, watcher
  goroutine, cold-start sweep.
- `docs/faas_implementation_spec.md` — limits table.

## Acceptance

- `make test` — all new tests pass.
- `make lint` — golangci-lint clean.
- `make spec-check` — ADR-087 cross-linked from
  `docs/adr/025-decoupled-control-plane-and-compute.md` and the
  spec.md limits table is updated.
- `make leakcheck` — unaffected (schedd-internal, no netns/TAP).
- Live: stand up two schedd nodes, deploy an app with
  `MaxConcurrency=2`, warm 2 instances on schedd-1, drive 10 wake
  attempts in <1s, observe
  `appAtCapacityTotal{app, kind="wake"}` = 8, then on the next
  30s tick observe `pressureReassignmentsTotal{outcome="migrated"}`
  = 1, then on the second sweep observe
  `pressureReassignmentsTotal{outcome="peer_live_migrated"}` = 1.
  Gateway `GET /v1/apps/{id}` returns the new `owner_node_id`.
