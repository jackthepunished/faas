# ADR-044 — Per-plan CPU fairness at the cgroup level (issue #301)

Status: Accepted, 2026-07-28. Owner: @poyrazK. Related: issue #301
("OPS: No CPU fairness at the cgroup level beyond inherited slice
weights"). Supersedes the literal-but-misnamed metric shape
`cgroup_cpu_throttled_us_total{slice}` from the issue body. The
replacement is `vmmd_cpu_throttle_ratio{slice}` (gauge) plus
`vmmd_cpu_throttle_seconds_total{account_id, app_id}` (counter, top-100).

## Context

Today every VM lands directly at
`/sys/fs/cgroup/faas-tenant.slice/<instance>` with `cpu.weight=256`
hard-coded as a neutral default. There is no per-plan `cpu.max` quota,
no per-plan `cpu.weight` ratio, and no per-plan sub-slice. A Scale
customer's tight infinite loop burns the whole multi-core share and
starves Hobby/Free tenants sharing the box. The financial model
assumes a steady-state plan mix (§1); a single hot-loop app invalidates
that assumption and the Free + Hobby tiers are the de facto acquisition
funnel — outrage during public launch kills growth.

The 5 acceptance items from issue #301 close the gap: per-plan
`cpu.max` quota, per-plan `cpu.weight` ratio, a `FaasCpuStarvation`
alert at > 80% throttle ratio over 5m, a per-app
`vmmd_cpu_throttle_seconds_total{account_id, app_id}` counter (top-100
hottest apps), and an e2e test.

## Decision

**1. Three-level cgroup hierarchy via systemd sub-slices.**
`faas-tenant.slice/<plan-slice>/<instance>` where `<plan-slice>` is
`tenant-free|tenant-hobby|tenant-pro|tenant-scale`. The sub-slices are
owned by systemd (one unit each, dropped by
`deploy/ansible/roles/systemd_slices/tasks/main.yml`), not vmmd. The
ansible role sets `CPUWeight=` and `Parent=faas-tenant.slice` only —
**no `MemoryMax`, no `CPUQuota`** on the sub-slices; the parent's hard
fence stays the only memory ceiling, and per-instance `cpu.max` is
written by vmmd (jailer v1.7 has no `--cgroup cpu.max=N` arg).

**2. `cpu.weight` ratio per plan is 2:4:8:16.** Encoded in
`pkg/api/limits.go::Limits.CPUWeight` (Free=2, Hobby=4, Pro=8,
Scale=16) and surfaced by `(Plan).CPUWeight()`. Wired through the
jailer's `--cgroup cpu.weight=N` argv (`pkg/fcvm/config.go`) so the
weight lands at scope-create time. The systemd sub-slices carry the
same weight, kernel-accounted from boot, so the ratio is correct
even before the first Wake.

**3. Per-plan `cpu.max` quota is 100/200/500/1000 ms / 100 ms period.**
Encoded in `pkg/api/limits.go::Limits.CPUQuotaUS` and
`Limits.CPUPeriodUS`, surfaced by `(Plan).CPUQuotaUS()` and
`(Plan).CPUPeriodUS()`. Written by `writeCPUMaxTo` in
`pkg/fcvm/cgroup.go` (a direct `/sys/fs/cgroup/.../cpu.max` file
write — the only path that works given jailer v1.7's argv surface).

**4. `vmmd_cpu_throttle_ratio{slice}` is a sibling gauge, not a
metric the kernel emits.** `node_cpu_cgroup_throttled_seconds_total`
is cumulative without a usage denominator, so the alert expression
the issue literally names (`throttled_us_total / …`) is unworkable.
The vmmd sampler computes
`throttle_delta / (throttle_delta + usage_delta)` over the per-tick
5m window in `pkg/fcvm/cpustats/cache.go` and exposes it as
`vmmd_cpu_throttle_ratio{slice}` — the source of the
`FaasCpuStarvation` alert. The `slice` label is the per-plan
sub-slice (`tenant-free|tenant-hobby|tenant-pro|tenant-scale`).

**5. `vmmd_cpu_throttle_seconds_total{account_id, app_id}` is the
cumulative counter the issue asked for.** Counter shape
(`Σ(throttled_usec delta) / 1e6`), bounded to the top-100 hottest
apps via `pkg/wire/topn_app.go::topAppSet` (sibling primitive to
`topAccountSet` from PR #300 / ADR-041). Wire value:
`InstanceStats.cpu_throttled_seconds` (issue #301 / PR-D field),
populated by `pkg/vmmdgrpc/server.go::Stats` from
`cpustats.Cache.Lookup`.

**6. `Plan` and `AccountID` thread onto the wake wire as new proto
fields.** `CreateFromSnapshotRequest` and `CreateColdBootRequest`
each gain `string plan = 4` and `string account_id = 5`. `Plan` drives
the per-plan cgroup sub-slice path (`ParentCgroupFor(plan)`); an
empty plan falls back to the legacy 2-level path (`ParentCgroupRoot/<instance>`)
for pre-#301 callers. `AccountID` rides alongside so vmmd can label
the throttle counter — empty = "anonymous" admission (matches the
`requestTotal` overflow policy in `pkg/wire/metrics.go`).

**7. ADR slot 042.** Main has 040 (OCI symlink) + 041 (per-account
rate limit) at the time of this PR. Next free is 042.

## Consequences

- A Scale customer's tight infinite loop is capped at 1000 ms / 100 ms
  (= 10 vCPU at the per-instance level). The kernel additionally
  enforces the per-plan `cpu.weight` ratio so quiescent workloads
  don't preempt each other within a slice.
- `pkg/fcvm/cgroupstats/reader.go::Sample` now also reads
  `throttled_usec` from the same `cpu.stat` file as `usage_usec`.
  On kernels < 5.14 (where `throttled_usec` was added) the reader
  returns 0 + ok=true; `vmmd_cpu_throttle_ratio{slice}` interprets a
  zero ratio as "no throttling" rather than alerting.
- `pkg/fcvm/cpustats/cache.go` adds `ThrottledSeconds` to the
  cumulative `Reading` (counter-shape) and `ThrottledRatio` to the
  per-tick ratio. Regression detection on both `usage_usec` and
  `throttled_usec` (either counter regressing trips a reset — they're
  written by the same kernel struct under the same cgroup, so a
  recreation that nuked one baseline nuked both).
- `pkg/wire/topn_app.go` is the new sibling primitive — capacity 100
  hottest apps, key `(account_id, app_id)`, 24h reset window. Mirrors
  `pkg/wire/topn.go::topAccountSet` shape. Used by schedd to admit
  only the top-100 hottest apps into the
  `vmmd_cpu_throttle_seconds_total` counter; the rest collapse to
  `account_id="other", app_id="other"`.
- New runbook `docs/runbooks/FaasCpuStarvation.md` (6-section shape,
  same as `tenant-abuse.md`).
- New alert `FaasCpuStarvation` in
  `deploy/ansible/roles/prometheus/files/faas.rules.yml`. Severity
  warn — CPU starvation ≠ customer action failing (the cgroup's
  `cpu.max` just caps their slice); the response is operator-side
  capacity work, not a customer-facing outage.
- New dashboard panel `top-throttled-apps` (Grafana export at
  `deploy/grafana/top-throttled-apps.json`, byte-identical copy at
  `deploy/ansible/roles/grafana/files/top-throttled-apps.json`).
- New e2e `cmd/e2e/cpu_fairness_test.go` (`//go:build metal`):
  1 hot-loop Hobby app + 5 quiet Hobby apps; assert each quiet app's
  p95 latency stays within 2x baseline.

## Rejected alternatives

- **`cgroup_cpu_throttled_us_total{slice}`** (literal issue metric):
  rejected. The kernel-emitted counter has no usage denominator, so
  any meaningful alert expression needs `throttle / (throttle +
  usage)` math that isn't expressible on a single counter. We surface
  the same shape as `vmmd_cpu_throttle_ratio{slice}` (computed
  windowed) and `vmmd_cpu_throttle_seconds_total{account_id, app_id}`
  (cumulative counter).
- **Per-vCPU pinning (taskset / cpuset.cpus)**: out of scope for
  issue #301. The single-box deployment shares a socket; pinning
  doesn't add fairness at this scale and complicates the snapshot
  restore path (snapshots are already pinned to the FC version, not
  the CPU).
- **Cross-host quotas**: covered by issue #56 (multi-box), not this
  PR. Issue #301 is per-box.
- **Charging for throttled time**: separate billing work. The metric
  is informational here; no billing path consumes it yet.
- **`vmmd-mkdir` for the per-plan sub-slice dirs**: rejected. Race
  against vmmd's own UID on first Wake + a per-cgroup write authority
  that's harder to reason about than systemd. systemd drops the
  sub-slices at boot (idempotent) and the ansible role is the
  single source of truth.