# ADR-088 · Tier A10: per-app `overflow_node` preference

- **Status:** accepted v1.0
- **Date:** 2026-08-10
- **Issue:** #297 Tier A10 (follow-up to ADR-087 Tier A9 / ADR-064 Tier A4 / ADR-066 Tier A5)
- **Decision:** Add a per-app `overflow_node` field (`compute_nodes.name` resolved
  to UUID server-side) that the Tier A9 capacity-pressure rebalancer
  consults **before** falling back to the first-peer-with-headroom
  selection. **Unavailable overflow target falls through to the
  default fallback** — never refused — and is observed via the new
  metric `overflowTargetSpillHitsTotal{outcome="unavailable"}`.

## Context

After Tier A9 (ADR-087, shipped 2026-08-10), an app whose owner is
sustained-at-capacity gets rebalanced to the **first** peer with
admission headroom (sorted by name ASC). That's fine for the engine,
but doesn't help a customer who already knows which peer they want
for a specific app — say, an east-coast push tier or a peer whose
fleet they've warmed snapshots for. There's no surface today to
declare that preference.

The dead-node rebalancer (ADR-064, `pkg/sched/rebalancer.go`) handles
`compute_nodes.active=false` events only. The pressure rebalancer
(ADR-087, `pkg/sched/pressure_rebalancer.go`) handles sustained
per-app AtCapacity events only. Neither asks the customer whether
they have an explicit spill preference.

The customer's take: a customer with two acq'd compute nodes wants
their high-traffic funnel pinned to a specific peer when its owner
fills up — not random-order first-fit. The operator's take: the
default first-fit is fine for v0; v1 needs an ergonomic preference
knob. Tier A10 ships the knob.

## Decision

### Surface

A new optional field `overflow_node` on `CreateAppRequest` and
`UpdateAppRequest`. Wire type is `string` — the human-readable
`compute_nodes.name` — not a UUID, matching the operator convention.
apid resolves the name to the UUID via the existing
`Store.ComputeNodeByName(ctx, name)` precedent at
`cmd/apid/compute_nodes.go:250`. Tri-state semantics:

| Value | Meaning |
|---|---|
| `nil` (omitted) | Don't change the field. |
| `""` (empty string) | Clear the preference (back to default fallback). |
| non-empty | Resolve server-side; reject if name is unknown or node is `active=false`. |

On Create, the same tri-state applies — `overflow_node` is settable
at create time.

### Schema (migration 00165)

```sql
alter table apps add column if not exists overflow_node uuid;
alter table apps add constraint apps_overflow_node_chk
  check (overflow_node is null or overflow_node <> '00000000-0000-0000-0000-000000000000');
alter table apps add constraint apps_overflow_node_fkey
  foreign key (overflow_node) references compute_nodes(id) on delete set null;
create index apps_overflow_node_idx on apps (overflow_node)
  where overflow_node is not null;
```

- `ON DELETE SET NULL` (not RESTRICT) so draining a node doesn't
  strand apps whose only preference was that node.
- `apps_overflow_node_chk` mirrors `apps_node_id_nonempty_chk` from
  migration 00090 — tripwire against a buggy INSERT path producing
  an "uninitialised" `00000000-...` sentinel.
- Partial index — the engine's hot path is "find pressured apps
  with an explicit preference"; a NULL preference is the common
  case. Indexing only the non-NULL tail keeps the index narrow.

### Validation

apid enforces:

1. **Resolution**: `Store.ComputeNodeByName(ctx, name)` →
   `404 invalid_overflow_node` on `ErrNotFound`.
2. **Active check**: `row.Active == true`, else `422 invalid_overflow_node`.`
3. **Empty-uuid rejection**: handled by the DB CHECK.

### Engine behavior (`pkg/sched/engine.go::RebalancePressuredApps`)

After the existing eligibility and cooldown checks, **before**
`findPeerWithHeadroom`, the engine consults `app.OverflowNode`:

- **If `app.OverflowNode == nil`** or has been SET-NULL'd by an
  `ON DELETE` cascade → existing A9 fallback path runs unchanged.
- **If `app.OverflowNode` references a peer that is `active=true`
  with admission headroom ≥ `app.RAMMB + PerVMOverheadMB`** → use
  that peer (bypass `findPeerWithHeadroom`). Observe
  `overflowTargetSpillHitsTotal{outcome="used"}`.
- **If `app.OverflowNode` references a peer that is `active=false`
  OR has no admission headroom** → observe
  `overflowTargetSpillHitsTotal{outcome="unavailable"}` and fall
  through to `findPeerWithHeadroom`.

The fallback observation is what differentiates "the customer
specified a target and we used it" from "the customer specified a
target but we couldn't, so we picked the first peer with headroom."
Operators use this to detect a stuck `overflow_node` value during
incidents.

### Hot path invariant

A peer's admission headroom is read exclusively from local DB rows
(`Store.ActiveComputeNodes` + `Store.ComputeNodeUsedMB`). There is
no live gRPC probe to peers — the engine trusts the local PG
aggregate. This matches the Tier A9 architecture and reuses the
existing `ComputeNodeUsedMB` (Σ RAMMB+8 over live instances on the
peer).

### Atomicity & cooldown

- Atomicity: `Store.ReassignAppOwner(ctx, appID, fromNodeID, toNodeID)`
  (existing — Tier A4). `RowsAffected() == 0` → `ErrConflict`. The
  `fromNodeID` predicate makes concurrent sweeps race-safe
  exactly-one-wins-per-app.
- Cooldown: `apps.reassigned_at < now() - RebalanceCooldownSeconds`
  (existing — Tier A4). Reassignment stamp applies to overflow-
  target reroutes identically to fallback reroutes.

## Migrations

- `00165_apps_overflow_node.sql` — adds `apps.overflow_node uuid
  NULL` + empty-uuid CHECK + FK with `ON DELETE SET NULL` + partial
  index `apps_overflow_node_idx` (NULL excluded).

## Metrics

- New CounterVec `overflowTargetSpillHitsTotal{outcome}` with a
  pre-instantiated closed set:
  - `outcome="used"` — engine used the customer's preferred peer.
  - `outcome="unavailable"` — preference was set but the peer was
    inactive or full; engine fell through to fallback.
- Extends `pressureReassignmentsTotal{outcome}` (Tier A9) only if
  needed for direct correlation — not required because
  `overflowTargetSpillHitsTotal` is a finer-grained cut.

## Limits

No new tunable in `pkg/api/limits.go`. A10's behaviour is "if set,
prefer; if unset, behave exactly like A9." The metric labels cover
the new behaviour; operators tune via operational means (or via
the customer's `overflow_node` value).

## Failure modes

1. **`overflow_node` refers to a peer that goes `active=false`**
   after the customer's app is created. The apid enforces active on
   PATCH but does NOT scan every minute. An operator-scheduled
   drain leaves the column as-is; the engine observes `unavailable`
   on the next pressure sweep and falls through. No new
   observability gap: the `active=false` event is already a
   `compute_nodes.active` row state the engine can read.
2. **`overflow_node` cycle**: app declares `overflow_node = X`,
   owner reassigns to `X`, customer then declares
   `overflow_node = owner` (back-edges the cycle). Engine treats
   `overflow_node == e.ownerNodeID` as a no-op and falls through
   to the A9 fallback — no infinite loop.
3. **Operator deletes the preferred compute_node** (a future
   endpoint; today only `active=false` is reachable from the
   admin). `ON DELETE SET NULL` cascades the preference to NULL
   cleanly. The next pressure sweep sees NULL and uses the A9
   fallback.
4. **Engine fall-through flood**: every pressured app with an
   unavailable target causes a `unavailable` observation. The
   metric is intentionally cheap (no GPU pressure) — operators
   can detect a fleet-wide operational outage via
   `overflowTargetSpillHitsTotal{outcome="unavailable"} >> 0`.

## Open follow-ups

- Customer-readable `GET /v1/compute-nodes?active=true` endpoint so
  the customer can discover valid `overflow_node` names. Today the
  CLI flag is just `--overflow-node <name>` and the customer must
  hard-code or pre-coordinate with the operator.
- Per-app `overflow_nodes` list (multi-peer spill chain) — explicitly
  out of scope for A10.
- Operator-side force-reroute (e.g. "force every app off box-1
  during incident") — out of scope for A10.

## Rejected alternatives

1. **Multi-peer ordered list `overflow_nodes text[]`** — richer, but
   v1 customers have a single spill target. Adds DB complexity
   (column type, partial index shape) for marginal value. Defer to
   v2.
2. **UUID on the wire (no server-side resolution)** — matches the
   `apps.node_id` peer-join invariant but breaks the operator
   convention and makes CLI usage painful. Resolution cost is one
   indexed lookup on `compute_nodes.name` (the unique index
   already exists).
3. **Force-reroute-the-unavailable-target (no fallback)** —
   strictly stronger contract, but refuses the spill instead of
   relying on `findPeerWithHeadroom`. Under partial fleet failure
   this strands apps at 503 even when other peers have headroom.
   Fall-through observability is the load-bearing compromise.

## Implementation

- Migration `00165_apps_overflow_node.sql` + test.
- `state.App.OverflowNode *string` adjacent to `NodeID`.
- `State.UpdateApp(ctx, id, params)` extended with optional
  `OverflowNode *string` — or a focused
  `State.UpdateAppOverflowNode(ctx, id, name)` helper.
- `api.CreateAppRequest` + `api.UpdateAppRequest` + `api.AppResponse`
  gain `OverflowNode *string \`json:"overflow_node,omitempty"\``.
- `pkg/apid/openapi.yaml` mirrors the three field additions.
- SDK Go mirror.
- `cmd/apid/handlers_ext.go::validateUpdateApp` +
  `cmd/apid/handlers.go::buildApp` resolve name → UUID via
  `ComputeNodeByName`.
- `pkg/sched/engine.go::findOverflowPeerWithHeadroom` (new helper)
  + branch in `RebalancePressuredApps`.
- `pkg/wire/metrics.go` adds `overflowTargetSpillHitsTotal`
  CounterVec with closed-set pre-instantiation.
- `cmd/gregale/commands2.go::cmdApp` adds `--overflow-node <name>`
  flag (PATCH-only on the CLI surface for now).

## Acceptance

1. Customer `PATCH /v1/apps/{slug}` with
   `{overflow_node: "box-eu-1"}` resolves server-side; the field
   round-trips on `AppResponse.overflow_node` as the resolved UUID.
2. Server returns `422 invalid_overflow_node` when the name
   doesn't resolve OR the node is `active=false`.
3. Empty string `""` clears the preference.
4. Engine `RebalancePressuredApps` consults the preference; uses
   the named peer if headroom; observes
   `overflowTargetSpillHitsTotal{outcome="unavailable"}` and falls
   through otherwise.
5. `ON DELETE` of a `compute_node` sets the preference to NULL via
   the FK cascade.
6. Migration is replay-safe (a second MigrateUp returns nil) and
   down-symmetric (the down body drops the column + CHECK + FK +
   partial index; the re-applied up body round-trips).
7. CLI `gregale app <slug> --overflow-node ""` clears;
   `--overflow-node "box-eu-1"` sets.
8. All existing Tier A9 tests still pass — the A10 branch is
   wired in *after* the existing A9 helpers so the A9 fallback
   path is unconditionally exercised when `OverflowNode == nil`.
