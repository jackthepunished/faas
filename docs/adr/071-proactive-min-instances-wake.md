# ADR-071 · Proactive min-instances wake (issue #557)

- **Status:** proposed
- **Date:** 2026-08-03
- **Decision:** Add a `pkg/sched/floor` trigger that pro-actively
  wakes instances up to the customer's effective min-instances floor
  every 1 s; close the dual-source-of-truth revenue bug via a read-side
  `state.App.EffectiveMinInstances()` helper (no migration); tighten
  the per-plan `MaxMinInstances` cap to Free 0 / Hobby 1 / Pro 3 /
  Scale 10; emit `floor.wake` and `instances.parked_min_instances_released`
  audit kinds; surface `min_instances_target` on the
  `GET /v1/instances` wire surface.
- **Why:** Issue [#557](https://github.com/poyrazK/faas/issues/557)
  asks for `min-instances per deployment (Hobby+)`. The literal
  per-deployment axis is rejected for this PR (deferred to #556
  traffic-splitting); the per-app axis already ships end-to-end but
  three gaps blocked customer-facing utility:
  1. **No upward enforcement.** `pkg/sched/scaleup` (RPS/CPU) and
     `pkg/sched/targets` (in-flight) are reactive — they only fire
     on observed signal. The reaper enforces the floor *downward*
     (don't park below it) but nothing wakes instances *up* to it.
     A customer who configures `min_instances=2` and never sends
     traffic sees 0 resident instances, paying the §6.3 wake budget
     on first request after idle.
  2. **Revenue bug from dual source of truth.** The reaper reads
     `apps.min_instances` (legacy column); `pkg/meter/sampler.go`
     reads `state.ScalingPolicyOrDefault(app.ScalingPolicy).MinInstances`
     (jsonb). A bare `SetAppMinInstances` PATCH writes only the
     column — divergent customers get a warm floor they are never
     billed for, contradicting ADR-060 ("billed from t=0").
  3. **Plan cap unbounded.** Today's only ceiling is
     `MaxConcurrency` (1/2/5/20 by plan). A Scale customer can pin
     20 × 1032 MB ≈ 20.6 GB (~43% of the §6.2-2 47,600 MB tenant
     budget) from one API call.

## Decisions (numbered)

1. **New `pkg/sched/floor` trigger.** 1 s cadence, mirrors
   `pkg/sched/scaleup` skeleton. Walks every app the schedd owns;
   per-app: gates on (a) `floor > 0`, (b) `plan.MinInstancesAllowed()`,
   (c) workload-class ≠ worker, (d) `ledger.Concurrency(appID) <
   floor`, (e) `concurrency < effectiveMaxConcurrency(app, plan)`,
   (f) scale-out cooldown elapsed, (g) per-app backoff elapsed,
   (h) `ledger.HeadroomMB()` not breached. On gate pass →
   `engine.AdmitInstance(ctx, app.ID)`. On `AtCapacity` (engine
   refused; no FAILED row) → `OutcomeAtCapacity`. On non-nil error →
   `recordFailure(app.ID)` (exponential backoff capped at
   `api.MaxFloorBackoffSeconds = 60 s`) + emit
   `floor_reconcile_errors_total{kind="admit_error"}` + warn-log.

2. **Read-side helper, no migration.** `state.App.EffectiveMinInstances()`
   returns `max(legacy column, ScalingPolicy.MinInstances jsonb)`.
   Swap-in at three readers: reaper floor arithmetic (loop.go:1033),
   `pkg/meter/sampler.go` floor billing block, and the new trigger's
   `AppStats.Floor`. The engine's `atMinFloorWithNoSignal` (wake-gate
   short-circuit) continues to read only `ScalingPolicy.MinInstances` —
   mixing legacy and jsonb there would block legitimate request-driven
   wakes on the floor, regressing the §4.3 burst semantics. The dual
   sources are intentional per PR-A (#462): the legacy column is the
   projection of the policy; the helper respects the layering.

3. **§6.2-2 ordering.** Floor wakes yield to live wakes via the
   `ledger.HeadroomMB()` pre-check (step 1h above). The engine's
   `NodeLedger.Admit` is unchanged — it's the absolute backstop. Live
   wakes see no new throttling; floor wakes hit `OutcomeRamCeiling`
   and wait for headroom. `RAMPressureEviction`
   (`pkg/sched/reaper.go::SelectEvictions`) deliberately ignores the
   floor — invariant §6.2-2 puts ceiling before floor. Unchanged.

4. **Per-app exponential backoff.** `nextRetryAt = now +
   min(MaxFloorBackoffSeconds, 2^attempts × 1s)` with `attempts`
   capped at 6 (60 s ceiling). Mirrors the targets trigger's per-app
   cooldown (PR-C, #462). Bounds the FAILED-row hazard on a
   RAM-saturated box.

5. **New `MaxMinInstances` plan cap.** Free 0 / Hobby 1 / Pro 3 /
   Scale 10. Tighter than today's implicit `MaxConcurrency` clamp;
   protects the 47,600 MB §6.2-2 ceiling from one customer's API
   call. Plan-tier enforcement lives in the existing `updateApp`
   handler (`cmd/apid/handlers_ext.go`); the new error is
   `ErrMaxMinInstancesExceeded(got, planMax)` modelled exactly on
   `ErrInvalidMinInstances` (`pkg/api/errors.go:1375-1384`), using
   `WithLimit(int64(planMax), int64(got)) + WithDocs(...)`.

6. **Two audit kinds.**
   - `floor.wake`: emitted by the trigger on every successful admit
     (post-`AdmitInstance` returning a live `InstanceID` and
     `AtCapacity=false`). Payload: `{app_id, floor, concurrency_before,
     wake_id}`.
   - `instances.parked_min_instances_released`: emitted by the
     reaper on every aggressive-park tick that drops the running
     count below the customer's effective min_instances (semantic:
     "the customer's floor dropped, so we're releasing instances the
     floor would have kept resident"). Payload: `{app, floor,
     post_park, reason="min_instances_lowered"}`.

7. **New `min_instances_target` wire field on `GET /v1/instances`.**
   `api/openapi.yaml` `InstanceResponse` gains one optional
   `integer >= 0` field; apid populates from
   `state.App.EffectiveMinInstances()` via a per-page batch lookup
   (one `AppByID` per distinct parent app). `omitempty` so customers
   who never opted in see no field.

## Failure modes

| Mode | Behavior | Tripwire |
|---|---|---|
| Box RAM-saturated | `OutcomeRamCeiling` for new floor wakes; live wakes unaffected | `floor_reconcile_errors_total{kind="admit_error"}` rate |
| `AdmitInstance` returns non-`CodePlanLimitConcur` error | `recordFailure` (60 s cap); next tick skips until window | same counter + `floor_instances_admitted_total` flatline |
| Plan gate off (Free) | `OutcomeDisabled` per tick — silent no-op | per-app `floor_decisions_total{outcome="disabled"}` rows |
| Worker-class app | `OutcomeDisabled` — workers are reaper-exempt by design | per-app `disabled` rows |
| Divergent config (column=2, jsonb=5) | helper returns 5 → trigger wakes 5; meter bills 5 | `min_instances_target` on the wire matches `EffectiveMinInstances()` |
| Concurrent admin PATCH mid-tick | `EffectiveMinInstances()` re-read per tick; eventual consistency, never torn | snapshot reads, no locks |

## Security

No new attack surface. The trigger calls the same
`engine.AdmitInstance` path the gateway uses on a request-driven
wake; the engine's wake-gate (`pkg/sched/engine.go:admitGate`) is
unchanged and remains the single admission authority. The helper
is a pure read. The new error uses the existing
`WithLimit`/`WithDocs` shape. Audit emission reuses the existing
`pkg/audit.Auditor.Emit` (best-effort, never blocks, never rolls
back the wake).

## Consequences

- **New package** `pkg/sched/floor` with `doc.go`, `trigger.go`,
  `decide_test.go`, `trigger_test.go`.
- **New helper** `state.App.EffectiveMinInstances()` (file
  `pkg/state/app_min_instances_effective.go`) with full table-driven
  branch coverage (`app_min_instances_effective_test.go`).
- **`pkg/api/limits.go`** gains `MaxMinInstances int` field,
  4 plan rows (0/1/3/10), `Plan.MaxMinInstances()` accessor, plus
  `FloorDecisionIntervalSeconds = 1` and `MaxFloorBackoffSeconds = 60`
  constants. `pkg/api/limits_test.go::TestPlanLimitsMatchSpec` table
  extended with `MaxMinInstances: N` in all 4 plan `want` blocks;
  new `TestPlanMaxMinInstances` pin test.
- **`pkg/api/errors.go`** gains `CodeMaxMinInstancesExceeded = "max_min_instances_exceeded"`
  const + `ErrMaxMinInstancesExceeded(got, planMax)` constructor
  with `WithLimit(int64(planMax), int64(got)) + WithDocs(...)`.
- **`pkg/sched/loop.go`** gains `floor *floor.Trigger` field,
  `WithFloor` option, `floorT` ticker setup, `floorTick` helper,
  select case, `runFloor` method (~50 lines). The reaper floor map
  (`runReaperAggressive`) reads `EffectiveMinInstances()` instead of
  `MinInstances` to align with the trigger.
- **`pkg/sched/engine.go`** — the `atMinFloorWithNoSignal` wake-gate
  helper is unchanged: it continues to read only
  `ScalingPolicy.MinInstances` (jsonb), not `EffectiveMinInstances()`.
  Mixing the legacy column into the wake-gate would block
  request-driven wakes on a customer-configured floor, regressing
  the §4.3 burst path. The floor trigger is the upward path.
- **`pkg/meter/sampler.go`** — the floor billing block reads
  `state.EffectiveMinInstances()` instead of
  `ScalingPolicyOrDefault(...).MinInstances`. Closes the revenue gap.
- **`pkg/wire/metrics.go`** — three new Prometheus surfaces:
  `schedd_floor_reconcile_decisions_total{app, outcome}`,
  `schedd_floor_reconcile_errors_total{app, kind}`, and
  `schedd_floor_instances_admitted_total`. Pre-instantiated for
  closed label sets (8 outcomes, 2 error kinds) so the rows surface
  in `/metrics` from boot. Three nil-safe emitter methods
  (`ObserveFloor`, `IncFloorReconcileError`, `IncFloorInstanceAdmitted`).
- **`api/openapi.yaml` + `pkg/apid/openapi.yaml`** — new
  `min_instances_target` field on `InstanceResponse`. `make spec-sync`
  regenerates the embed.
- **`pkg/api/dto.go`** — `InstanceResponse.MinInstancesTarget int` field
  with `json:"min_instances_target,omitempty"`.
- **`cmd/apid/handlers_ext.go`** — `instanceResponse` signature gains
  the floor parameter; `listInstances` (per-app) reads
  `app.EffectiveMinInstances()` directly.
- **`cmd/apid/handlers_account_scoped.go`** — `listInstancesForAccount`
  adds `batchMinInstancesTargets` helper (one `AppByID` per distinct
  AppID on the page, ≤ limit).
- **`cmd/schedd/main.go`** — `schedulerAuditor` local lifted to share
  with the floor trigger; `schedFloorEngine`, `schedFloorLedger`,
  `schedFloorPlanResolver` adapters added; `loop.WithFloor(floor.New(...))`
  wired after `WithTargets`.

## Rejected alternatives

- **Per-deployment axis.** Deferred to #556 (traffic-splitting).
  `deployments.min_instances` only becomes meaningful alongside
  traffic splitting, and it carries a foot-gun — every new deploy
  would reset the floor to 0 unless inherited.
- **Write-side sync of column ↔ jsonb.** A customer's bare
  `SetAppMinInstances=2` PATCH would silently clobber an explicit
  jsonb `ScalingPolicy.MinInstances=5` (the jsonb is the policy
  source-of-truth; the column is the projection). Forces jsonb
  writes on the 99% of apps that never opted into ScalingPolicy.
  Read-side helper is safer and migration-free.
- **Bypass `admitGate` for floor wakes.** Unnecessary: the gate is
  idempotent because `Concurrency` correctly excludes PARKED
  (pkg/state/machine.go:99-106), so the gate doesn't fire unless
  the floor is genuinely met.
- **Provisioned-concurrency SKU.** Out of scope per the issue.

## Downstream

- **`00142_align_min_instances.sql`** — backfill migration for
  customers with divergent configs (column > 0, jsonb = 0). The
  helper returns `max` so the divergence is currently invisible to
  the trigger (it picks the higher value); the migration is a
  cleanup, not a fix.
- **`FAAS_FLOOR_RESERVED_MB`** env knob — future "20% of ceiling
  reserved for floor wakes" budget so a customer's floor never
  starves itself waiting on headroom. v1 just yields to live wakes.
- **CLI surface** — `faas app <slug> --show-min-instances` to
  inspect both sources (already partially covered by
  `cmd/gregale/commands5.go:471` test path).

## Reused

- `pkg/sched/scaleup/trigger.go` — skeleton for `pkg/sched/floor`.
- `pkg/sched/targets/trigger.go` — newer template for the
  option/constructor shape.
- `pkg/audit/audit.go::Emit` — for both `floor.wake` and
  `instances.parked_min_instances_released` audit events (no new
  audit file).
- `cmd/schedd/main.go::schedTargetsEngine` — adapter pattern for
  `*sched.Engine` → `floor.Engine` interface (mirrors
  `schedFloorEngine`).

## Acceptance criteria → test mapping

| # | Criterion | Code | Test |
|---|---|---|---|
| 1 | `min_instances=2` → 2 RUNNING instances exist before first request | `floor.Tick → engine.AdmitInstance` | `pkg/sched/floor/trigger_test.go::TestTick_AdmitsUpToFloor` |
| 2 | `GET /v1/instances?app={slug}` shows `min_instances_target=2` per row | `handlers_account_scoped + EffectiveMinInstances` | `cmd/apid/handlers_ext_test.go::TestInstanceResponse_MinInstancesTargetOmittedWhenZero`; `make spec-check` gates drift |
| 3 | Lowering floor 2→0 parks the over-min instances within `idle_timeout × 1.5` | reaper (existing) | `pkg/sched/loop_test.go::TestLoopReaperAggressiveHonorsFloor` (pre-existing, regression-tested) |
| 4 | Raising floor 0→2 wakes 2 fresh instances within `cold_boot_budget + 2 × wake_budget` | floor trigger with 1s cadence | `pkg/sched/floor/trigger_test.go::TestTick_AdmitsUpToFloor` (3-tick sequence) |
| 5 | Audit log emits `instances.parked_min_instances_released` | `l.emitFloorReleasedAudit(...)` | `pkg/sched/loop.go::runReaperAggressive` branch; integration covered by existing reaper tests |
| 6 | Financial model: steady-state cost = `min_instances × ram_mb_hours × plan_rate` | `meterd` sampler reads `EffectiveMinInstances` | `pkg/meter/sampler_test.go::TestSampler_FloorOneHobbyBillsFromT0` (pre-fix this fails — customer under-billed; post-fix passes) |
