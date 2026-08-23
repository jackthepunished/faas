# ADR-127 · Wake-failure-mode observability for operators

- **Status:** proposed
- **Date:** 2026-08-23
- **Issue / PR:** [#1059](https://github.com/poyrazK/faas/issues/1059) (also bundles #955 closure)
- **Decision:** Ship two additive Prometheus metrics on the vmmd / schedd
  side — `*_wake_failure_total{box, reason}` and `*_wake_latency_seconds{box, phase}` —
  pre-instantiated at their closed label sets, plus the recording rule
  `vmmd_cold_boot_ratio` and the `FaasColdBootRatioHigh` alert. Operators
  get the answers to "which box is slow", "what failed", and "what
  fraction of wakes cold-booted" directly off §12 dashboards + alerts,
  without grepping slog.

## Context

The wake timeline ships three operator surfaces today:

- **ADR-123** (`gateway_wake_latency_seconds`, `gateway_wake_latency_seconds_by_node{node_id}`,
  `gateway_wake_phase_duration_seconds{phase}` with closed phase set
  `{queue_wait, coordinator_wait, schedd_admit, vmmd_wake, guest_ready,
  cold_fallback_reason}`) — the customer-facing "why is my instance
  slow" surface.
- **ADR-097** (`schedd_wake_rpc_duration_seconds{app, phase}` with closed
  phase set `{admit_to_rpc, rpc_call, rpc_to_running}`) — the per-phase
  sched-side histogram.
- **ADR-074** (`*_wake_snapshot_tier_total{tier=warm|init|cold_boot_fallback}`
  and `*_warm_snapshot_errors_total{reason=vmm_call|store_write}`) — the
  warm-vs-cold split and the warm-capture error breakdown.

What the platform does **not** have is the operator-facing complement:
when vmmd fails to wake an instance, every failure site currently logs a
`slog.Warn` with the wrapped error string
(`pkg/fcvm/manager.go:2602` restore-fallback, `:2377` plan-cgroup,
`:2405`/`:2411` workload-cgroup, `:2226` setupNetwork, `:2646` cold-boot
terminal; `pkg/sched/engine.go:2138` mark-snapshot-stale) but there is
no closed-set counter that maps an incident to a root cause. The only
metric today is the unlabeled `vmmd_cold_boot_fallback_total` which
counts the restore→cold-boot transition but says nothing about *why*
the restore failed.

Three concrete operator questions are unanswerable from the §12 surface:

1. **Which box is slow?** — `gateway_wake_latency_seconds` is a fleet
   aggregate. The per-node sibling has `node_id` but operators need the
   same decomposition on the vmmd side (where the slow path actually
   runs).
2. **What fraction of wakes cold-booted?** — the warm-vs-cold counter
   exists but there's no SLO and no alert. A fleet that creeps from
   10 % cold-boot to 40 % over a week goes unnoticed.
3. **What failed and how often?** — the only signal is `slog.Warn`
   lines, which means grepping logs after the fact. A counter would let
   alerts fire on the *type* of failure (e.g. `disk_full` should page
   the moment it crosses zero).

Issue #1059 proposes the operator-facing complement. This ADR
formalises the metric names + label sets + SLO so dashboards, alerts,
and the metric itself stay in lockstep.

## Decision

### 1. Two new metrics, owned by `pkg/wire.OpsMetrics`

| Name | Type | Labels | Closed label sets |
|---|---|---|---|
| `*_wake_failure_total` | `CounterVec` | `box`, `reason` | `reason ∈ {snapshot_stale, disk_full, jailer_fail, netns_fail, cgroup_fail, vsock_fail, snapshot_restore_err, mem_backend_err}`; `box` ≤ `maxBoxLabelValues` (admission set, overflow → `__other__`) |
| `*_wake_latency_seconds` | `HistogramVec` | `box`, `phase` | `phase ∈ {queue_wait, coordinator_wait, schedd_admit, vmmd_wake, guest_ready}`; `box` ≤ `maxBoxLabelValues` (same admission set) |

Both ship via the existing `wire.NewOpsMetrics(prefix)` single-registry
constructor (`pkg/wire/metrics.go:1284-1285`), pre-instantiated at the
closed set so `/metrics` surfaces zero rows from boot (per the
ADR-074 / ADR-097 precedent).

`*` is the per-daemon prefix — `vmmd_*` on the host-side VM lifecycle,
`schedd_*` on the engine side. schedd emits
`schedd_wake_failure_total{reason=record_runtime_failed}` from the
existing audit-reason string at `pkg/sched/engine.go:2198`; vmmd emits
`vmmd_wake_failure_total{reason=...}` from the wake-failure hook sites
listed in §3 below.

### 2. `box` cardinality bound — `maxBoxLabelValues = 64`

The closed `box` label set is bounded by a fixed-capacity non-evicting
admission set (`boxLabelSet`, `maxBoxLabelValues = 64`). Overflow
collapses to `__other__`. This mirrors the existing precedent at
`pkg/wire/metrics.go:413-424` for `accountLabelSet` and `ipLabelSet`.

64 covers the single-control-plane reality today (N=1) and gives
headroom for the Tier A multi-host rollout (ADR-062, ADR-064, ADR-066
chain). When `ComputeNodesGauge` lands as a fleet cardinality
indicator, the cap stays in `pkg/wire/metrics.go` — system-level
metric-label cardinality caps are owned by the metrics package per the
sibling precedent of `maxAccountLabelValues` and `maxIPLabelValues`
(also in `pkg/wire/metrics.go`). `pkg/api/limits.go` is reserved for
*per-plan quotas* (deployed/concurrent/RAM), not fleet-cardinality
caps.

For now `box` resolves to the literal string `"local"` on vmmd and
`schedd` (single-control-plane placeholder); the multi-host rollout
replaces this with a `compute_nodes.id` lookup. §3.4 documents the
placeholder.

### 3. Hook points for `vmmd_wake_failure_total`

The closed `reason` vocabulary is enforced by a single classifier,
`classifyWakeError(err error, snap *fcvm.Snapshot, fcVersion string) string`
in `pkg/fcvm/wake_classify.go`. The classifier is called **once** at
each wake-failure site, not scattered. Sub-classification uses
`errors.Is` against sentinel errors (`ErrDiskFull`, `ErrJailerFail`,
`ErrNetnsFail`, `ErrCgroupFail`, `ErrVSockFail`) and inspects the
wrapped message for `ENOSPC` (the only consumer today is the
`stageReadOnlyAs` I/O at `pkg/fcvm/vmm.go:649-652`).

Hook sites:

| Site | Reason mapping | Notes |
|---|---|---|
| `pkg/fcvm/manager.go:2613` (restore-fallback) | `classifyWakeError(rErr, req.Snapshot, m.fcVersion)` | Pre-existing `m.metrics.ObserveFallback()` continues to count the unlabeled fallback; the new counter adds the typed reason. |
| `pkg/fcvm/manager.go:2646` (cold-boot terminal) | same | Terminal failure on the cold-boot path. |
| `pkg/fcvm/manager.go:2226-2229` (`setupNetwork`) | hardcode `netns_fail` | The error wrap already names the failure surface. |
| `pkg/fcvm/manager.go:2377-2379` (plan cgroup) | hardcode `cgroup_fail` | Warn-and-continue path; still counts. |
| `pkg/fcvm/manager.go:2405-2411` (workload cgroup) | hardcode `cgroup_fail` | Same. |
| `pkg/vmmdgrpc/server.go:341-344` (`CreateFromSnapshot`) | hardcode `snapshot_restore_err` | gRPC-level failure on the wire path. |
| `pkg/vmmdgrpc/server.go:368-370` (`CreateColdBoot`) | hardcode `mem_backend_err` | gRPC-level failure on the cold-boot path. |

`mem_backend_err` is unreachable today (only `backend_type: "File"`
exists at `pkg/fcvm/vmm.go:684-688`). Including it now keeps the
vocabulary closed and avoids a label-value migration later when
hugetlbfs lands.

### 3.5. What `vmmd_wake_failure_total` counts (per-step, not per-wake)

The counter name is the most-precise label that still fits the
existing §12 dashboard legend (`wake_failure`), but its semantic is
**per-step**, not per-wake: the counter increments once per wake-step
that fails, regardless of whether the wake ultimately returns success.

Concretely:

- The **restore-fallback hook** fires when the snapshot-restore step
  fails. BootColdBoot is then attempted; if it succeeds, the wake
  returns success to the customer, but `wake_failure_total` still
  incremented (the operator sees `cold_boot_fallback` plus
  `snapshot_restore_err` for that wake). The metric is the per-step
  reason split, not the customer-visible outcome.
- The **cold-boot terminal hook** fires only when BootColdBoot itself
  fails — i.e. the wake returns an error to the customer.
- The **setupNetwork hook** fires before `bringUp` is reached; if the
  wake returns an error to the customer, this counter is the reason.
- The **cgroup hooks** are warn-and-continue; they fire on the
  warn path but the wake still returns success. They surface a
  sustained silent regression that operators must catch from a panel.

Operators cross-referencing the cold-boot-ratio alert against this
counter should expect the counter to be *equal to or higher than* the
ratio × wake volume: every cold-boot fallback implies at least one
`snapshot_restore_err` increment, even when the wake ultimately
succeeds via the cold-boot path. The runbook at
`docs/runbooks/FaasColdBootRatioHigh.md` §Check documents this
relationship.

A future rename to `vmmd_wake_step_failure_total` is the cleanest
fix for the ambiguity, but it's a §12 dashboard-legend migration
deferred to a follow-up PR — see §6.

### 4. `box` value plumbing — `box = "local"` placeholder

vmmd constructs `OpsMetrics` with `prefix = "vmmd"` and resolves `box`
to the literal `"local"`. schedd does the same. When the multi-host
control plane lands (ADR-062 / 066), vmmd gains a `compute_nodes.id`
lookup at boot and resolves `box` from the `compute_nodes` table; the
placeholder is replaced in a follow-up commit per the
"every quota / limit lives in `pkg/api/limits.go`" rule.

### 5. Cold-boot ratio SLO — `vmmd_cold_boot_ratio` + `FaasColdBootRatioHigh`

Recording rule:

```
vmmd_cold_boot_ratio = rate(vmmd_wake_snapshot_tier_total{tier="cold_boot_fallback"}[5m])
                     / rate(vmmd_wake_snapshot_tier_total[5m])
```

Alert:

```
- alert: FaasColdBootRatioHigh
  expr: vmmd_cold_boot_ratio > 0.30
  for: 10m
  labels: { severity: page, component: vmmd, family: cold_boot_ratio }
  annotations:
    summary: "vmmd cold-boot fallback > 30 % for 10m"
    description: "..."
    runbook_url: "https://github.com/poyrazK/faas/blob/main/docs/runbooks/FaasColdBootRatioHigh.md"
```

Threshold rationale: 30 % is below the §6.2 budget saturation point
(47,600 MB / 56 GB) on the wake-economics curve. A fleet at 30 %
cold-boot is doing ~3× the wake-side I/O it would at 10 %, which
stresses `lv-fc` (`pkg/fcvm/lvfc.go`) and amplifies snapshot-write
contention on the host. Catching the trend at 30 % gives the operator
~24 h before the `snapshot_fleet_avg_mb > 160` alert fires (per
CLAUDE.md's fleet snapshot target). 10-minute `for:` matches the
existing `FaasSnapshotFleetAvgHighWarn` (`faas.rules.yml:202-212`) so
both alerts page the same incident class.

The ratio is recorded once per fleet, not per-box, because the cold-boot
trigger today is global (the snapshot tier is selected by schedd, not
vmmd). Per-box aggregation is a follow-up tracked separately.

## Consequences

### Positive

- Operators can read "which box is slow", "what fraction cold-booted",
  and "what failed" directly off §12 dashboards and PagerDuty alerts.
- The closed `reason` vocabulary means a new failure mode gets a new
  reason value, never a new label dimension — Prometheus cardinality
  stays bounded without a runtime cap.
- The `__other__` collapse on `box` matches the §11
  plaintext-host-redaction precedent; operators get the same recovery
  story (`docs/runbooks/FaasApidAuditWriteFailures.md:85-103`).
- The cold-boot-ratio SLO closes the gap that ADR-074 left: warm-vs-cold
  was countable but not alertable.

### Negative / trade-offs

- Two metrics with overlapping data (`vmmd_wake_phase_duration_seconds{phase}`
  fleet and `vmmd_wake_latency_seconds{box, phase}` per-box). The fleet
  histogram is kept for §12 dashboard back-compat; the new histogram is
  the operator-facing surface. Dashboards gain the per-box view; the
  fleet view stays in place.
- `box = "local"` placeholder has to be replaced before the multi-host
  rollout. This is called out in §3.4 above and tracked as a Tier A
  prerequisite.
- Memory note (`ADR-016` closed-set rule): every caller must pass from
  the closed enum, not bare strings. The classifier enforces this at
  one site; call sites hardcode the reason string directly. A reviewer
  flagging a bare string is correct.

### Out of scope (per issue #1059 explicit deferral)

- Per-app wake failure breakdown (fleet-only for v1).
- Auto-remediation on cold-boot ratio (operator-driven).
- Cross-box cold-boot ratio aggregation (per-box first).

## References

- ADR-016 — vmmd stats + metrics naming convention (closed label sets).
- ADR-064 — wake timeline canonical vocabulary (`wake.boot_started` /
  `wake.boot_completed` events).
- ADR-074 — warm-snapshot audit + GC; the `*_wake_snapshot_tier_total{tier}`
  counter this ADR derives `vmmd_cold_boot_ratio` from.
- ADR-097 — schedd wake-phase telemetry; the per-phase histogram
  precedent this ADR extends with a `box` label.
- ADR-123 — wake-boot trigger + queue-depth + concurrency-at-admit;
  the customer-facing surface that ADR-127 complements for operators.
- ADR-098 — connection-aware execution; referenced from the
  issue-955 closure path (probe-outcome alerts live in `data_placement.yaml`).
- `pkg/wire/metrics.go:1312-1317` — `*_warm_snapshot_errors_total{reason}`
  closed-set shape that the new `*_wake_failure_total{box, reason}` mirrors.
- `pkg/wire/metrics.go:1421-1428` — `*_wake_rpc_duration_seconds{app, phase}`
  per-phase histogram shape that the new `*_wake_latency_seconds{box, phase}`
  mirrors.
- `pkg/wire/metrics.go:413-424` — `accountLabelSet` / `ipLabelSet`
  `__other__` collapse precedent that the new `boxLabelSet` mirrors.
- `pkg/fcvm/manager.go:2602` — the closest pre-existing wake-failure
  log site (`slog.Warn "restore failed, falling back to cold boot"`).
- `docs/runbooks/FaasSnapshotFleetHigh.md` — the runbook template the
  new `docs/runbooks/FaasColdBootRatioHigh.md` mirrors.
- `deploy/ansible/roles/prometheus/files/faas.rules.yml:190-212` — the
  `FaasSnapshotFleetAvg*` family this ADR's `FaasColdBootRatioHigh` joins.
- `pkg/promqlrules/data_placement.yaml` — the file the issue-955 closure
  path adds alerts to.