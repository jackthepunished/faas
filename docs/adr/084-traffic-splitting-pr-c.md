# ADR-084 · Traffic splitting — picker signal, largest-remainder redistribution, wake fan-out (issue #556 / PR-C)

- **Status:** accepted
- **Superseded (in part, PR-E):** prose referred to the monolithic
  `cmd/gatewayd/` daemon split by ADR-070 into `gatewayd-public` (TLS-only
  edge) and `gatewayd-internal` (routing + wake + proxy). Body is preserved
  verbatim; readers should substitute "gatewayd-internal" for the
  routing/wake/proxy path and "gatewayd-public" for the certmagic/TLS path.
  `cmd/gatewayd/<file>.go` citations in this body are stale; see PR-E for
  the new file locations.
- **Date:** 2026-08-08
- **Issue:** #556 (PR-A #732, PR-B #768, PR-C current)
- **Supersedes:** none
- **Related:** ADR-016 (additive proto field discipline); ADR-025 (placement scheduler per-node sub-cursor); ADR-070 (gatewayd-public / gatewayd-internal split); ADR-082 (per-app customer SLO surface); issue #556 acceptance items #1 (proportional rebalance), #2 (wake fan-out), #3 (DeploymentID wire propagation); memory [[proto-versions-check-strip-after-regen]]; memory [[gateway-target-set-picker]]

## Context

PR-A shipped the persistence layer (`deployments.traffic_percent` +
partial index). PR-B shipped the gateway picker (weighted stride +
binary search on cumulative weights, multi-bucket `targetSet`s).
Together they correctly **route** traffic once instances exist, but
the operator-facing contract from issue #556 —

> "I want to flip 25% of traffic to the canary and have it actually
> land."

— is not reachable from PR-A/PR-B alone. Three latent defects and one
missing capability block it. PR-C fixes all four in one cluster.

### Defects surfaced by the explore agent

**S1 — `updateDeploymentTraffic` emits no `pg_notify`.**
`cmd/apid/handlers_ext.go:1168-1236` writes the store, emits an
audit row, returns. The PR-B gateway refresh subscriber
(`cmd/gatewayd-internal/backend.go:298-321`) is dead code on this path
because nothing notifies. `faas traffic set` is silently a no-op for
the running gateway until an unrelated `deployment_changed` fires
(new deploy, supersede, rollback).

**S2 — `UpdateDeploymentTraffic` is the hard error itself.**
`pkg/state/pgstore.go:4089-4117`: step (4) stamps target =
`newPercent`, zeroes all siblings, then step (5) asserts Σ = 100.
Since Σ is structurally `newPercent`, **any `newPercent != 100` rolls
back the transaction with `ErrTrafficPercentSumInvalid`** (translated
to 409 at the handler). `MemStore` mirrors the trap. So `faas traffic
set --percent 25` returns 409 today — the canary use case is
unreachable.

**S3 — `TestPg_UpdateDeploymentTraffic_ZerosSiblings` is vacuous.**
`pkg/state/pgstore_test.go:3023-3067`: on error it `return`s, on
success it does `_ = updated` and ends. Comment at 3064 (*"Shouldn't
reach here"*) admits it. No meaningful regression test exists.

**S4 — `admitAndDispatchForDeployment` returns incomplete `WakeResult`.**
`pkg/sched/engine.go:916-994` ends with `WakeResult{InstanceID: ins.ID}`
— no NodeID / Method / WakeID / Port / DeploymentID. Also never runs
Phase 3 (vmmd boot) or Phase 4 (commit). It's a floor-trigger-only
path. **Wake-fan-out must not route through this function.**

### Missing capability

**M1 — wake-fan-out.** Even if the picker picks a deployment with no
instances, it has no way to wake them. PR-B's cold-bucket fallback
returns the largest non-empty sibling instead, which silently
defeats the operator's intent (the operator said "send 25% to B";
the gateway says "B is empty, sending to A").

## Decision

### D1 — Picker returns a signal, not a side-effect

`PGBackend.Pick` widens from `(Target, bool)` to `PickResult`:

```go
type PickResult struct {
    Target     Target
    OK         bool
    Picked     string // deploymentID the request was routed to (may be "")
    ColdBucket string // empty unless the picked bucket was cold (no instances)
}
```

The cold-bucket fallback at `pgbackend.go:557-587` sets `ColdBucket`
to the picked deploymentID (not the fallback) so the handler knows
which deployment to wake. Handler sees `ColdBucket != ""`, calls
`Backend.Admit(ctx, appID, ColdBucket, max)` outside the read lock,
then re-picks.

#### Rationale

The picker's hot path holds a read lock over the target-set map
(`b.pickers.RLock()`). Triggering a wake inline inside the read-lock
critical section would deadlock the existing write-lock holder (the
admit critical section itself) and pin a `*Engine.Admit` call inside
what must remain a sub-microsecond hot-path function. Returning a
signal decouples the two phases:

1. **Phase 1 (read-locked):** "the route I would have chosen is X."
2. **Phase 2 (write-locked, separate `Backend.Admit`):** "wake X."

This also bounds retry — the handler does **at most one** admit per
request; sustained cold-bucket hits route to the warmest sibling
rather than stampeding the cold one. The atomic sub-cursor (`atomic.Uint64`)
that guarantees per-deployment round-robin is never observed in a
half-updated state by an inline admit — the cursor advance happens
in Phase 2's Admit critical section.

### D2 — Largest-remainder redistribution (Hamilton's method)

Given target `T` and prior live weights `{w_1, …, w_n}` summing to
100, residual `R = 100 - T`. Each sibling `i` receives:

```
share_i = floor(w_i * R / Σ_{prior-target}) + (i ranks in top R mod n by fraction DESC, ID ASC)
```

where `fraction_i = (w_i * R / Σ_{prior-target}) - floor(w_i * R / Σ_{prior-target})`.

Output is guaranteed to sum to 100 by construction. Worked example
for the canary use case:

| Prior | Operation | New |
|---|---|---|
| `{A: 100}` | set A=25 | `{A: 25}` (sole live, residual 75, no siblings) |
| `{A: 50, B: 30, C: 20}` | set A=25 | residual 75, shares {0, 45, 30}, Σ=100 |
| `{A: 50, B: 50}` | set A=0 | residual 100, equal fractions, tie-break by ID ASC → `{A: 0, B: 100}` |

#### Rationale

Three properties make largest-remainder the right pick for this
contract:

1. **Deterministic.** Same input always yields the same output. Pure
   function of the prior weights + the target. Property test can
   pin any combination without flakiness.
2. **Sum-safe by construction.** No post-hoc rounding pass needed —
   the algorithm's invariant is `Σ floor + (R mod n) == R`. Σ=100 is
   enforced in Go, consistent with PR-A's decision not to use a
   DEFERRABLE trigger (the cost of defensive SQL on a hot update path
   is not worth it).
3. **Integer-arithmetic.** No floats near money or quota. The
   `fraction_i` ranking step uses `float64` only for the comparison;
   the stored weights are integers (`int`). Pinning the tie-break
   (fraction DESC, ID ASC) eliminates the rounding-bias asymmetry
   that hurts naive "first by weight" approaches.

The alternative — operator-supplied `--redistribute` with explicit
sibling weights — was deferred to PR-D. The plan's headline use
case (set canary to N%) is reachable without it; explicit
"zero-the-siblings" semantics is a separate, narrower UX.

### D3 — Wake-fan-out is signal-and-retry, bounded at 1

`Handler.ServeHTTP` after `Pick`:

```go
res := h.backend.Pick(appID)
if res.ColdBucket != "" {
    // Phase 2: wake the cold bucket we landed on.
    if _, _, _, err := h.backend.Admit(ctx, appID, res.ColdBucket, max); err != nil {
        h.log.Warn("apid: wake-fan-out admit failed", "err", err, "deployment_id", res.ColdBucket)
    }
    res = h.backend.Pick(appID) // retry once
}
```

#### Rationale

- **Why one retry, not unbounded?** Sustained cold-bucket hits are a
  signal that the operator's traffic shape exceeds the fleet's
  capacity. One retry gives the bucket a chance to wake; subsequent
  retries just amplify load on the failing path. The next
  `deployment_changed` notify (refresh the weight table) lands within
  ~1s; the picker will then route to a sibling if the cold bucket
  still has no instances.
- **Why not inline-admit inside Pick?** See D1 — read-lock
  constraint + atomic cursor safety.
- **Why bounded by maxConcurrency, not a separate pool?** The existing
  `Backend.Admit` already respects the per-app ledger cap (§6.2
  invariant 1). Wake-fan-out inherits this for free.
- **Interaction with the rollout window.** During the rollout of
  PR-C's notify + rebalance, the cold-bucket fallback and wake-fan-out
  overlap briefly: a 50/50 split where one bucket has no instances
  routes to the largest non-empty sibling (PR-B legacy), then on the
  next request the wake-fan-out wakes the cold bucket. Within ~5s
  (one cold boot + one notify) the picker honours the operator's
  stated ratio. The behavior degrades gracefully, not catastrophically.

### D4 — Notify emit on traffic change (fixes S1)

After the audit emit at `cmd/apid/handlers_ext.go:1234`:

```go
if s.notif != nil {
    payload, _ := json.Marshal(map[string]string{
        "app_id":        app.ID,
        "deployment_id": updated.ID,
        "kind":          "traffic",
    })
    if err := s.notif.Notify(r.Context(), db.NotifyDeploymentChanged, payload); err != nil {
        log.Warn("apid: notify deployment_changed (traffic) failed", "err", err)
    }
}
```

The existing subscriber at `cmd/gatewayd-internal/backend.go:298-321`
parses `{app_id, deployment_id}` already; the new `kind` field is
additive and ignored. Future audit pipelines can distinguish weight
changes (`kind:"traffic"`) from status changes (`kind:"status"`,
pre-existing shape from PR-A rollback path).

### D5 — Bonus cleanups (cheap, same blast radius)

These ride along because they share the call sites being touched:

- **Implicit-100% synthesis narrowing.** `pgbackend.go:717-720` only
  synthesizes the legacy "100% on the lone bucket" entry when
  `len(picker.weights) == 0`. Previously, it would also fire if the
  bucket was missing from the weight table — clobbering a real table
  that lacked the bucket id. PR-C narrows the predicate.
- **Stale-set pruning in `RefreshDeploymentWeights`.** After
  rebuilding `weights`/`cum`, delete `picker.sets[id]` for `id`s no
  longer in the table AND whose `entries` slice is empty. Without
  this, `HealthyCount` over-counts and the picker carries phantom
  buckets indefinitely.
- **Phase-1 fast path reads instance row.** `engine.go:844`: read
  `deployment_id` off the looked-up `instances` row instead of
  `LiveDeployment` (singular) which silently returns the newest row
  under a 25/75 split.
- **`admitAndDispatchForDeployment` populates full WakeResult.**
  `engine.go:993`: defensive — no caller reads it today, but a
  future PR-D that wires wake-fan-out through this path won't fall
  over a half-built result.
- **Vacuous `TestPg_UpdateDeploymentTraffic_ZerosSiblings`.** Rewrite
  (or delete + replace with the 4 new pinned tests). The old test
  pinned nothing; the new ones pin the proportional contract.
- **Gate-order doc comment.** `handlers_ext.go:1150-1163` says
  "plan gate BEFORE range check"; the code does the opposite. Flip
  the comment — behavior unchanged, less confusion.

### D6 — No migration, no schema change

Proportional rebalance is arithmetic inside the existing transaction.
The partial index from PR-B's `00162_deployments_live_traffic_idx.sql`
already covers `LiveDeployments`. `Σ=100` is enforced in Go,
consistent with PR-A's decision not to use a DEFERRABLE trigger.

### D7 — CLI: no new flag in PR-C

The original plan listed `--redistribute`. PR-C's behavior change
makes the default proportional, so a `--redistribute` flag is
redundant for the headline use case. The flag is **deferred to PR-D**
if a "explicitly zero siblings" semantics is ever requested.

## Consequences

- The headline canary use case is reachable: `faas traffic set
  --deployment <depB> --percent 25` lands within ~1s, traffic splits
  25/75, the canary wakes on first request.
- Existing pre-PR-C apps (one live deployment, no traffic table rows)
  see byte-identical `Pick` behavior — `TestPGBackend_PickSingleDeployment_ByteIdenticalToLegacy`
  still pins this.
- The gateway refresh subscriber (`backend.go:298-321`) is now
  load-bearing on every `faas traffic set` — the dead-code path is
  alive.
- `pkg/state.pgstore` and `pkg/state.memstore` must agree on the
  redistribution algorithm. The `redistribute` helper is shared;
  the existing test suite already exercises both.
- A stale `deployments` row referencing a deployment id that's been
  deleted is now picked up by the stale-set pruning in
  `RefreshDeploymentWeights`. Before PR-C it could phantom-count.

## Rollback

Revert the PR-C commit cluster. The arithmetic-only nature of the
rebalance means the rollback is clean — no schema migration to
unwind, no partial index to drop. The notify emit reverts in the
same commit; the subscriber becomes dead code again but stays
load-bearing on `CreateDeployment`/`rollbackApp`.

## Verification

12 new pinned tests (4 state + 4 memstore mirrors + 2 gateway + 1
handler + 1 scheddgrpc proto wire), plus the 6 PR-B tests still
green. `make proto` regenerates cleanly; `make proto-normalize`
strips toolchain versions (memory [[proto-versions-check-strip-after-regen]]).
The end-to-end operator path is exercised in `cmd/e2e/provision_traffic_split_test.go`
via `pkg/e2etest.WaitForDeploymentTraffic`.
