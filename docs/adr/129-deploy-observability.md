# ADR-129 · Deploy-time observability for faas daemons

- **Status:** proposed
- **Date:** 2026-08-24
- **Issue / PR:** [#586](https://github.com/poyrazK/faas/issues/586)
- **Decision:** Ship four observability surfaces that together
  answer the four operator questions issue #586's body names:
  "what version is each daemon running?", "how long has each
  daemon been up?", "is each daemon ready to serve traffic?", and
  "what platform release is the fleet on?". A fifth surface — the
  persisted `deployments.liveness_restart_count` column — closes
  the "schedd restart resets the restart-loop signal" gap left by
  the in-memory `LivenessWindow`.

## Context

Issue #586's four unanswered operator questions today:

1. **"What version is each daemon running?"** — operators grep
   `journalctl` for panic stack traces; matching the trace to a
   commit SHA requires `kubectl-style` forensic walks through git
   history. The binary's identity (commit, build time) is stamped
   on the slog envelope but isn't surfaced as a metric.
2. **"How long has each daemon been up?"** — operators today
   answer from `systemctl status faas-<daemon>` which shows
   `Active: active (running) since <ts>` but the same signal is
   not available to dashboards / alerts (e.g. "alert if a daemon
   has been up > 7 days, that's likely a missed upgrade").
3. **"Is each daemon ready to serve traffic?"** — `/readyz` does
   not exist (issue #571); operators today answer from process
   liveness, conflating "process is alive" with "process is
   serving traffic".
4. **"What platform release is the fleet on?"** — no metric. The
   answer today requires reading the version of each daemon
   manually and triangulating with `apt-cache policy faas-*`.

And the persistence gap closed by this ADR:

5. **"How many times has this deployment been restarted in its
   lifetime?"** — `pkg/sched/liveness_window.go::LivenessWindow`
   keeps the counter in memory only. A schedd restart resets the
   counter and the customer loses the "this deployment has been
   crashlooping 4 times in 10 minutes" signal that triggered the
   last park.

The current observability surfaces are inadequate:

- **`vmmd_ops_total{op}`** and the per-daemon `*_ops_total` cover
  request-side signals but not boot-time lifecycle.
- **`Daemon()` stamps `version` on every slog line** (commit 10 of
  this mega-PR) but doesn't surface a metric.
- **`LivenessWindow.RecordRestart`** is the runtime decision
  authority but its counter evaporates on schedd restart.

The closed-vocab precedent (ADR-016) for metric labels and the
closed daemon set (apid, gatewayd-public, gatewayd-internal,
schedd, vmmd, imaged, meterd, builderd, gregale — Tier A7 split
kept gatewayd-public and gatewayd-internal as distinct units per
ADR-070) bounds the metric cardinality at 10 series per daemon.

## Decision

### 1. `daemon_build_info{daemon, version, git_sha, build_time}`

`pkg/wire/metrics.go` adds a new `daemonBuildInfo *prometheus.GaugeVec`
with closed `{daemon, version, git_sha, build_time}` label set.
The constructor pre-instantiates the cartesian at boot for the 9
closed daemon names plus an `other` overflow bucket — 10 series
per OpsMetrics, 90 fleet-wide. The gauge value is always 1; the
labels carry the signal. Operator dashboards query this for the
"Daemon versions fleet-wide" heatmap panel.

`wire.Daemon()` reads `wire.Version`, `wire.GitSHA`,
`wire.BuildTime` (build-time ldflags) and calls
`defaultOps.SetDaemonBuildInfo(name, Version, GitSHA, BuildTime)`
once after the slog is configured.

### 2. `daemon_uptime_seconds{daemon}`

`pkg/wire/metrics.go` adds a new `daemonUptimeSeconds *prometheus.GaugeVec`
with closed `{daemon}` label set, pre-instantiated at 0. A
1-second goroutine spawned from `wire.Daemon()` ticks
`SetDaemonUptime(name, time.Since(startedAt).Seconds())` until
`ctx.Done()`. Operator dashboards query this for the "Daemon
uptime (1h)" timeseries panel and the "this daemon has been up
> 7 days, that's a missed upgrade" alert.

### 3. `daemon_ready{daemon}`

`pkg/wire/metrics.go` adds a new `daemonReady *prometheus.GaugeVec`
with closed `{daemon}` label set, pre-instantiated at 0.
`wire.Daemon()` calls `defaultOps.MarkReady(name)` after the run
function returns successfully (i.e. the daemon reached its
steady-state path). Until `/readyz` lands (issue #571), the run
function returning IS the readiness barrier. Operator dashboards
query this for the "Fleet readiness" panel.

### 4. `faas_deploy_version{version}`

`pkg/wire/metrics.go` adds a new `faasDeployVersion *prometheus.GaugeVec`
with `{version}` label set. Pre-instantiated at boot from
`wire.Version` so `/metrics` surfaces the current release from
process start. `SetDeployVersion(Version)` re-stamps on version
change (rolling deploy, hot reload). Operator dashboards query
this for the "Releases fleet-wide" stat panel and to detect
partial rollouts — a fleet with 2 versions visible is mid-rollout.

### 5. Persisted `deployments.liveness_restart_count`

`migrations/00411_liveness_restart_count.sql` adds
`liveness_restart_count INT NOT NULL DEFAULT 0` plus a CHECK
`liveness_restart_count >= 0`. `pkg/state.pgstore.RecordRestart`
bumps the column by 1 in a single statement alongside the
in-memory `LivenessWindow.RecordRestart` call from
`pkg/sched.Engine`. On schedd startup the `LivenessWindow`
seeds from this column so a fresh process inherits the prior
count.

The five surfaces together are the issue #586 follow-on plus the
closed-shape precedent from ADR-127 / ADR-128. Sibling ADRs:
ADR-016 (closed-set vocab), ADR-127 (vmmd-wake-failure
observability), ADR-128 (restart-loop detection), ADR-070 (Tier
A7 gatewayd-public / gatewayd-internal split — the closed
daemon set is post-split).

## Consequences

- Every daemon now exposes a "version + git_sha + build_time"
  row at `/metrics`. Operators can answer "what version is each
  daemon running?" from a Prometheus query, no journalctl
  forensics required.
- The "Releases fleet-wide" stat panel reads `faas_deploy_version`
  to surface the platform's current release; a fleet with 2
  versions visible is mid-rollout.
- `daemon_uptime_seconds` + a future "this daemon has been up >
  7 days" alert closes the missed-upgrade gap.
- `daemon_ready` flips 0→1 once `wire.Daemon().MarkReady` fires;
  the load balancer can refuse traffic to daemons that haven't
  flipped yet (until issue #571's `/readyz` lands, this is the
  best signal we have).
- `deployments.liveness_restart_count` survives schedd restarts;
  the next bump catches up if a write fails.
- The closed-set vocabulary (10 daemon names × 1 version per
  metric) bounds the cardinality — fleet-wide 90 + 90 + 90 + N
  series, well below the Prometheus tens-of-thousands guideline.

## Out of scope

- **Grafana dashboard panels** (`deploy/grafana/faas-fleet.json`
  + ansible mirror) — deferred to a follow-on PR per ADR-129.
  The metric surface is the cluster-C critical path; the panels
  ship in a separate change so the wire/migration split stays
  clean.
- **`/readyz` real implementation** (issue #571). Until that
  ships, `MarkReady` fires when the run function returns
  successfully — a daemon that's serving traffic has called this
  line.
- **On-read seed of `LivenessWindow`** from
  `liveness_restart_count` at schedd startup — the column is
  updated by every bump but not consulted at startup yet. A
  follow-on PR wires `pkg/sched.Engine.New()` to read the
  per-app deployment counts and seed the window.
- **Off-host log shipping** — Loki / rsyslog pipeline. The
  version-stamped slog line + the daemon_build_info metric are
  enough for the operator surface; full log shipping is a
  separate ADR.

## References

- ADR-016 (closed-set label vocabulary).
- ADR-070 (Tier A7 gatewayd-public / gatewayd-internal split — the
  closed daemon set is post-split).
- ADR-127 (vmmd-wake-failure observability — sibling ADR; same
  closed-set pattern, same pre-instantiation cartesian).
- ADR-128 (restart-loop detection — sibling ADR; the
  `deployments.liveness_restart_count` column is the persistent
  complement to the systemd-driven restart-count metric).
- ADR-066 (multi-host / compute_nodes — out of scope here; the
  per-daemon metrics stay daemon-local until the multi-host rollout).
- `pkg/sched/liveness_window.go:11-14` — slot 150 reservation
  comment for `deployments.liveness_restart_count`.
- Issue #571 (`/readyz` endpoint).
- Issue #586 (deploy-time observability epic).
