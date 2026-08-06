# ADR-039 — per-instance CPU metering, visibility only (issue #279 / PR-B)

- **Status:** Accepted, 2026-07-27. Owner: @poyrazK. Closes: #279 (PR-B).
- **Date:** 2026-07-27
- **Decision:** Make per-instance CPU consumption visible end-to-end
  (Prometheus, `usage_minutes.cpu_usec`, `GET /v1/usage`,
  `/v1/usage/summary`, `/v1/account/export`, `faas usage` CLI) without
  touching the billing surface. The data is sampled, written, and
  exposed — but **no provider push is added** in this PR. The
  `Provider.PushUsageRecord` extension (Stripe / Paddle) is the
  follow-up PR that lands on top of this seam.
- **Why:** §4.7 documents CPU as a billable dimension, but the wire
  path is half-shipped: `api/proto/.../vmmd.proto` carries
  `InstanceStats.cpu_pct` as `*wrapperspb.DoubleValue`, schedd's
  `instancestats.Poller` rollup already gates the gauge, and
  `pkg/wire/metrics.go` already emits `schedd_instance_cpu_pct{app,node}`
  — but `pkg/vmmdgrpc/server.go::Stats` never populates it, and
  `pkg/fcvm/cgroupstats` is imported by zero production files. The
  poller's design comment at `pkg/sched/instancestats/poller.go:230-238`
  is explicit: "cumulative-counter regression detection lives in
  PR-B's Stats handler." This ADR closes that gap.

  The user's explicit constraint is "see the metrics now, leave
  billing for later". Billing itself is spec §4.7 (plan RAM + 8 MB
  × wall seconds) and the financial model in the spreadsheet — neither is touched. The new
  data is **observational only**; the field is documented as such
  in `api/openapi.yaml` and in the apid handler comments.

## Path

```
vmmd cgroupstats.Reader → pkg/fcvm/cpustats.Cache (rate + accum)
  → cmd/vmmd cpu_poller (250 ms loop)
  → pkg/vmmdgrpc.Stats wire (cpu_pct, cpu_seconds — both nil on Unknown)
  → schedd instancestats.Poller (200 ms tick, per-instance reader)
  → schedd.ListInstanceStats gRPC RPC (new, pkg/scheddgrpc)
  → meterd scheddCPUAdapter (30 s TTL, preserves last-known on transient errors)
  → pkg/meter.Sampler.cpuDeltaForMinute (per-instance baseline)
  → pkg/state.AppendUsage(cpu_usec) — additive merge on conflict
  → usage_monthly view (SUM(cpu_usec))
  → apid /v1/usage, /v1/usage/summary, /v1/account/export
  → faas usage CLI (informational panel, not billed)
```

The schedd-side Prometheus rollup is a separate, parallel path:

```
schedd poller → wire.ReplaceInstanceStats {app, node}
  → CounterVec schedd_instance_cpu_seconds_total{app,node}
       (sum rollup, regression-guarded per (app,node) key)
```

`schedd_instance_cpu_pct{app,node}` (the existing gauge, max rollup)
landed in PR-A (ADR-036); the new `_seconds_total` counter is the
cumulative-work-done sibling.

## Three non-obvious contracts

### 1. Regression drops the baseline (jailer restart = new cgroup)

A cgroup recreation (jailer restart, manual `rmdir`) resets
`cpu.stat`'s `usage_usec` to a smaller value. The cache detects
this by comparing the new reading to the previous one. On a step
down the cache **drops the baseline** and resets
`accumSeconds` to 0; the next `Observe` returns Valid=false (no
reading) until two post-regression samples have been seen.

Why this is intentional and not a bug:

- The customer's CPU clock for the instance starts fresh on a new
  cgroup. We do not patch across the break — the previous cgroup's
  work is a different billable surface from this cgroup's work.
- The schedd poller stamps `CPU=Unknown` on the first
  post-regression row; the Prometheus rollup's
  `cpuSecondsLastSeen` per-key regression guard detects the same
  step-down and emits a 0 delta for that tick (monotonic counters
  cannot subtract).
- The meterd sampler treats `curr < prev` the same way: drop the
  baseline, write 0 to `cpu_usec` for that minute, pick up the new
  counter on the next minute. The cumulative value lost at the
  reset is unrecoverable by design — it is a known
  silent-under-count on cgroup recreation, surfaced as a single
  `slog.Warn` per regression in the schedd poller.

The contract is the same "looks wrong but is load-bearing" shape
as the snapshot `fc_version` reset on Firecracker upgrades
(ADR-005). Operators who need the lost CPU can join the audit log
(`instances.last_reason = "jailer_restart"`) with the per-minute
`cpu_usec` rows outside the break.

### 2. `cpu_usec` is additive on conflict; `mb_seconds` is first-write-wins

`pkg/state.pgstore.AppendUsage` uses two different conflict
semantics inside the same `ON CONFLICT (instance_id, minute)` clause:

| column       | semantics   | reason                                                                                    |
|--------------|-------------|-------------------------------------------------------------------------------------------|
| `mb_seconds` | first-write | This is the **billing-floor metric** — `plan RAM × wall minutes`. Stable once the instance is alive for the minute. A second write is the meterd redelivering the same minute (sampler restart). The first value wins. |
| `requests`   | first-write | Same shape: the dispatched-invocation count is stamped once per row by the drain, idempotent on the row. |
| `cpu_usec`   | additive    | This is the **sampled metric** — `Σ (deltaUsec over the cache's lifetime within the minute)`. The sampler's `cpuDeltaForMinute` is called once per 250 ms tick, so the same `(instance_id, minute)` row is rewritten ~240 times per minute. The merge is `cpu_usec = usage_minutes.cpu_usec + EXCLUDED.cpu_usec`. |

The asymmetry is load-bearing. A second appendage that retries
through the existing `DO NOTHING` would silently under-count CPU
to 1/N of the true value. The `mb_seconds` semantics cannot change
because the financial model depends on first-write-wins for the
billing-floor; the `cpu_usec` semantics cannot be first-write-wins
because the sampling cadence writes multiple times per minute.

### 3. Cumulative on the wire, delta at the rollup

The wire shape from vmmd to schedd carries `cpu_seconds` as a
**monotonic cumulative** counter (the cache's `accumSeconds`,
which is `Σ(deltaUsec) / 1e6` across the cache's lifetime for the
instance). It resets only on regression.

The schedd rollup maintains a `cpuSecondsLastSeen` per `(app, node)`
key. On each `ReplaceInstanceStats` tick it computes
`delta = curr - lastSeen` and emits `delta` to the
`CounterVec.Add(...)` call. `delta <= 0` (regression in the
schedd-side rollup gauge) is treated as 0 — the counter is
monotonic.

The pattern is "wire is the durable record, rollup is the
derivative". This decouples the wire-shape contract (cumulative,
easy to compute from the cgroup cache) from the Prometheus
contract (counter, monotonic, sum-of-deltas). The same shape as
the gateway's `gateway_requests_total` cumulative counter that
the reaper's `recentload` mirror turns into a 5 × 1 s RPS
(ADR-038).

## What is NOT in this PR

- **No** change to `pkg/billing/provider.go`.
- **No** Provider push of `cpu_usec` to Stripe or Paddle.
- **No** change to `OverageMillicentsPerGBHour` or any other
  money constant.
- **No** change to `pkg/api/limits.go` (no new quota; no CPU-based
  admission).
- **No** change to the financial model spreadsheet.
- **No** change to `pkg/sched/admission.go` (CPU-based admission is
  G9 / ADR-037 follow-up, separate PR).
- **No** per-instance dashboard surfacing (M9 work).
- **No** alert on CPU-hour (M8 row 2, separate from row 1).

The seam for the future billing PR is:

- `usage_minutes.cpu_usec` (this PR) — the data is already there.
- `Provider.PushUsageRecord` (next PR) — extends the signature
  with `cpu_usec` and writes the per-month aggregate.
- `pkg/meter.Pusher` (next PR) — invokes the new push once per
  hour per `(account, app, month)`.

The future PR is a single additive change spanning those three
files. This PR makes zero moves in that direction.

## Files

- **New**: `pkg/fcvm/cpustats/` (Cache + Observe + Snapshot +
  Lookup + Forget + Reset), `cmd/vmmd/cpu_poller.go`,
  `migrations/00055_usage_minutes_cpu.sql`,
  `pkg/meter/sampler_cpu_test.go`,
  `pkg/vmmdgrpc/stats_cache_test.go`,
  `pkg/fcvm/cpustats/cache_metal_test.go` (//go:build metal).
- **Modified**: `api/proto/.../{vmmd,schedd}.proto` (new wire
  fields), `pkg/vmmdgrpc/server.go` (Stats wires the cache),
  `pkg/scheddgrpc/{server,client}.go` (new RPC),
  `pkg/sched/instancestats/{reader,poller}.go` (per-instance
  accumulator), `pkg/sched/vmmclient.go` (proto decode),
  `pkg/meter/{sampler,loop,math}.go` (CPUSource seam), `pkg/state/{store,pgstore,memstore}.go`
  (AppendUsage signature), `pkg/wire/metrics.go` (new counter),
  `pkg/api/dto.go` (UsageResponse.CPUUsageUsec / CPUHours(),
  UsageExportResponse.CPUUsageUsec / CPUHours(),
  UsageSummaryResponse.UsedCPUHours),
  `cmd/apid/{handlers_ext,handlers_account}.go` (populate the
  fields), `cmd/faas/commands2.go` (CLI panel),
  `cmd/{schedd,meterd}/main.go` (wire the seams).
- **Unchanged** (explicit non-goals): `pkg/billing/provider.go`,
  `pkg/meter/pusher.go`, `pkg/sched/admission.go`, `pkg/api/limits.go`,
  the financial model spreadsheet.

## Consequences

- A new wire observation (`vmmd.Stats`) carries `cpu_seconds` as
  a `*wrapperspb.DoubleValue` (nullable). Schedd's poller skips
  rows where the value is nil (the existing PR-A contract, applied
  symmetrically to the new field).
- The `usage_minutes` table grows by 8 bytes per row (NULL-free
  bigint). 1 minute × 100 instances × 50 apps = 5,000 rows × 8 B =
  40 KB per minute. Migration is `NOT NULL DEFAULT 0` so the
  `usage_monthly` view's `SUM(cpu_usec)` works on the existing
  rows without a backfill.
- The new meterd `scheddCPUAdapter` keeps a 30 s TTL cache; if
  schedd is unreachable for >30 s, the sampler writes 0
  `cpu_usec` for that minute. A transient blip (one missed
  refresh) preserves the last-known snapshot — the operator sees
  a silent under-count if schedd is down for an entire minute,
  not for a single gRPC round trip.
- The Prometheus counter `schedd_instance_cpu_seconds_total{app,node}`
  is pre-instantiated with `(app="", node="")` so the panel
  exists at day 1 (precedent: `scaleUpDecisions` pre-instantiation
  in `pkg/wire/metrics.go:527-536`).
- The `cpu` field on `pkg/meter.Loop` is wired to `nil` for all
  pre-PR-B tests (the `nopParker` paths in `cmd/meterd/main_test.go`).
  A future refactor that drops the `cpu` parameter would break
  those tests; the seam is the regression catcher.

## Verification

- `make lint` (golangci-lint v2.4 + custom checks) — clean.
- `make proto-check` — clean (the proto changes are committed
  alongside the generated `.pb.go` files).
- `go test ./...` — passes; new tests:
  - `pkg/fcvm/cpustats/cache_test.go` (8 branches), `cache_metal_test.go`
    (3 metal tests, //go:build metal)
  - `pkg/meter/sampler_cpu_test.go` (4 tests pinning the
    `cpuDeltaForMinute` semantics)
  - `pkg/vmmdgrpc/stats_cache_test.go` (3 wire-shape tests)
  - `pkg/state/pgstore_append_usage_test.go::TestPg_AppendUsage_AddsCpuUsecOnConflict`
    (additive merge with non-zero deltas)
- `make test-metal` / `make metal-lima` — green; the new
  `pkg/fcvm/cpustats/cache_metal_test.go` exercises the vmmd-side
  rate cache against a real cgroup v2 mount.
- `make leakcheck` — no leaked netns / TAPs / cgroup leaves
  (the `Forget` on teardown is exercised).
- Operator spot-check on a reference node:
  ```bash
  curl -s http://localhost:9100/metrics | grep schedd_instance_cpu
  # expect: schedd_instance_cpu_pct{app="",node=""} 0   (pre-instantiated)
  # expect: schedd_instance_cpu_seconds_total{app="",node=""} 0   (pre-instantiated)
  # after a wake: schedd_instance_cpu_seconds_total{app="...",node="..."} <value>
  ```
