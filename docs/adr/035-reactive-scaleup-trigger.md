# ADR-035 — reactive scale-up trigger (issue #169 / #172)

Status: Accepted, 2026-07-25. Owner: @poyrazK. Closes: #169, #172.
Related: #170 (PR #205, in flight, CPU signal source), #171 (preferential
reaper, separate PR).

## Context

The platform's autoscaling today is "warm pool + idle reaper": an instance
is admitted only when a request arrives with no routable target, and the
reaper parks it when its per-app idle timeout (30 / 60 / 300 / 600 s)
elapses. The plan's `max_concurrency` is a *ceiling*, not a target — to
get burst capacity, customers must pre-allocate `min_instances = N` and
pay for N resident instances at all times, even at idle.

#169 is the missing piece: a per-app, signal-driven scale-up trigger.
When measured load exceeds a configured target, schedd should admit
additional instances proactively — but only up to `max_concurrency`,
never beyond.

## Decision

Ship the **whole vertical slice** for #169 in one combined PR: schema
(issue #172), apid validation, schedd trigger, metrics, ADR, CLI, and
OpenAPI. The PR review boundary is the feature, not a single commit
landed piecemeal — splitting it across multiple PRs would leave the
"enabled" model in flight for weeks.

### Signal sources

Two parallel signal paths feed the trigger:

- **RPS** — scraped from gatewayd's local `/metrics` endpoint
  (`gateway_requests_total{app="…",code="…"}`). Parsed by
  `pkg/sched/scaleup.HTTPPromScraper` into a per-app cumulative count
  and folded into a 5-second ring buffer (1s buckets, 5 buckets).
  Per-instance RPS = `sum(window) / max(1, inflight_count)`.
- **CPU** — read from `pkg/sched/instancestats.Reader.SnapshotForApp`
  (PR #205, in flight on a feature branch). Pro/Scale only. The
  reader is nil-safe inside the trigger — a missing reader silently
  falls back to RPS-only mode.

### "Enabled" model — infer from target columns

There is no separate `enabled` boolean. An app has autoscale turned on
iff at least one of `autoscale_target_rps` / `autoscale_target_cpu_pct`
is non-NULL. Single source of truth: the targets themselves. This is a
deliberate choice over a separate boolean — the boolean would let the
two columns drift (an enabled app with both columns = 0 is a no-op
but a "valid" config to the user; the infer-from-targets model makes
the table unopinionated about enable state and pushes the decision
into a single `if app.AutoscaleTargetRPS == 0 && app.AutoscaleTargetCPUPct == 0 { skip }` check).

### Plan tiering

- **Free**: neither target. Single-concurrency plan; can't grow beyond
  one instance per app.
- **Hobby**: RPS only. The cost shape of "scale on CPU without a
  `min_instances` floor" is unbounded on the cheaper tiers.
- **Pro**: RPS + CPU. The full surface.
- **Scale**: RPS + CPU. Same as Pro; Scale already has the largest
  caps so the same fields apply unchanged.

The plan gate runs in apid's `validateUpdateApp` BEFORE the bounds
check so a Free account PATCHing a 50-rps value surfaces the gate
(`403 plan_autoscale_not_allowed`), not the bounds
(`422 invalid_autoscale_target_rps`). 403 supersedes 422.

### Tick rate

1 second. This balances two competing costs:
- "Admit Nth instance before the gateway queue builds" — 1s is fast
  enough that the per-instance RPS exceeds the target for at most
  one tick before the trigger fires.
- "Don't hammer Postgres with a full app list on every tick" —
  `ListAllApps` is one query, and the trigger already uses
  `Ledger.Concurrency` (in-memory) for the divisor.

`api.ScaleUpDecisionIntervalSeconds = 1` is the single source of truth.

### Ledger interaction

Read-only. The trigger never bypasses `NodeLedger.Admit`; it calls
`Engine.AdmitInstance`, which goes through the same ledger path the
gateway uses on a request-driven wake. The cap is enforced inside
`AdmitInstance` (per-app `perApp[appID] < maxConc` check at
admission.go:129), so a 6th wake on a 5-instance app returns
`WakeResult{AtCapacity: true}` even when the trigger decided to
admit. The trigger observes this as `OutcomeRejectAtCap`, NOT
`OutcomeAdmit`, and never writes a FAILED row.

### Metrics

- `schedd_scale_up_decisions_total{app, outcome}` — counter, closed
  label set `{admit, reject_at_cap, no_signal, no_capacity}`,
  pre-instantiated so the rows surface in `/metrics` from boot.
- `schedd_scale_up_admit_rps` — unlabelled histogram, per-instance
  RPS at the moment of admit. Sized to the realistic per-instance
  range (1..1000).

`app` cardinality is bounded by the number of apps with autoscale
configured (the platform caps at hundreds of apps per account, only
a fraction of which enable autoscale).

### Nil-safety matrix

- `appStore == nil` → no-op (boot, before store wire-up).
- `instats.Reader == nil` → CPU path skipped, RPS-only mode
  (PR #205 not yet merged is fine).
- `PromScraper == nil` → RPS path skipped; degraded mode logs once
  and emits no_signal.
- `engine == nil` → no-op (boot).
- `Loop.WithScaleUp(nil)` → the ticker arm of `Run`'s select never
  fires. schedd's test surface stays green without a trigger.

The trigger is constructed defensively at every layer so schedd can
wire it before every downstream dependency is fully online.

### Why a separate package, not `pkg/sched/loop.go` directly

The trigger has its own inputs (instancestats + a request-count ring
buffer), its own worker struct, and its own metrics — three
responsibilities that don't fit `loop.go`'s ticker-arm pattern
cleanly. The package mirrors the `pkg/sched/heartbeat` /
`pkg/sched/instancestats` shape (constructor → `Tick(ctx)` → `Run(ctx)`).

The `scaleup.AdmitResult` type intentionally avoids importing
`pkg/sched` (which would create an import cycle: `sched` → `scaleup` →
`sched` via `loop.go`). The cmd/schedd wiring includes a thin
`s schedScaleUpEngine` adapter that lifts the relevant fields from
`sched.WakeResult` into `scaleup.AdmitResult`. The trigger only
inspects `AtCapacity` + `InstanceID` — the rest of `WakeResult` is
internal to the gateway's wake path.

## Consequences

- **Positive**: a customer on Hobby+ can configure a per-instance
  RPS target and stop paying for `min_instances` to handle burst
  traffic. The trigger admits extra instances on the fly, the
  reaper parks them when the burst subsides, and the customer
  pays only for the time those instances were actually resident.
- **Positive**: dashboard panel for "scale-up aggressiveness" via
  `schedd_scale_up_admit_rps` p95/p99 (spec §12).
- **Positive**: PR #171 (preferential reaper) can now read the
  trigger's per-app RPS history to bias eviction order — the
  trigger's ring buffer is the input side, the reaper is the
  output side.
- **Negative**: the trigger's correctness depends on
  `instancestats.Reader` landing (PR #205). Until that merges, the
  CPU target is inert and Hobby/Pro+Scale are functionally identical
  on CPU. We accept this — the trigger is correct in RPS-only mode
  and PR #205 is on a feature branch we can land first.
- **Negative**: the trigger reads every app per tick. With ~100 apps
  on a single box, that's 100 list + 100 ledger lookups per second.
  Within the 85% RAM ceiling budget but worth watching in
  benchmark (out of scope for this PR; spec §14 acceptance gate).

## Open follow-ups (not blocking)

- **Hobby CPU target** — defer; the current tiering (Hobby = RPS-only)
  is the conservative default. A future `hobby_cpu_target_allowed bool`
  opt-in could ship behind a flag.
- **Per-instance vs fleet-wide RPS target** — current draft is
  per-instance. Fleet-wide is a one-line change to `decide()` and a
  UX call; defer.
- **Reconcile with PR #170 / #171** — once those land, this ADR
  should add a "Related work" section referencing the merge order.
