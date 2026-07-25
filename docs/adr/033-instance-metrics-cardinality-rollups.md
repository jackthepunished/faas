# ADR-033 · Per-instance metrics: {app,node} cardinality rollups (issue #170 / PR-A)

- **Status:** proposed
- **Date:** 2026-07-25
- **Decision:** The schedd's per-instance metrics surface is exposed
  on Prometheus as a **rolled-up** `{app, node}` label set — never
  `instance_id` — and the in-memory `instancestats.Reader` is the
  only public seam future scale policy code (PR-B / #171 / #169)
  is allowed to read from. Wire cardinality is bounded by
  `(#apps × #compute_nodes)`, which is `O(100s)` on the one-box and
  `O(1000s)` on a small cluster — well under the Prometheus label
  cardinality ceiling ADR-031's egress allowlist set. The reader
  carries the per-instance rows because that's where future
  preferential reaper logic needs them.
- **Why:** Issue #170 calls for 5 Hz per-instance observability
  (CPUPct, RSSMB, InflightRequests, LastRequestAt). The naive
  Prometheus shape — `{app, node, instance}` — collapses to the
  same value across siblings of one app on one node: per the §6.2
  fan-out invariant (issue #168 / PR #176), an app can hold up to
  `max_concurrency(plan)` instances on one node, and Prometheus
  gauge `.Set` is keyed on the label tuple. Two siblings on the
  same `(app, node)` would silently overwrite each other on every
  rollup — the dashboard would show one of them at random, and the
  other one's data would vanish without an error.

  The right surface is: keep the per-instance data in a Reader
  (the future scale policy code reads from it directly), and emit
  only the rolled-up `{app, node}` tuple on Prometheus. The
  rollup rules (max CPU / sum RSS / sum inflight) are codified in
  ADR-016's wire shape and extended here.
- **Consequences:**
  - Three `GaugeVec` + one histogram + one counter land in
    `pkg/wire/metrics.go` under the `schedd_` prefix (ADR-015):
    `schedd_instance_cpu_pct{app,node}` (max rollup),
    `schedd_instance_rss_mb{app,node}` (sum rollup),
    `schedd_instance_inflight_requests{app,node}` (sum rollup),
    `schedd_instance_stats_collect_seconds` (histogram, per-tick
    duration), `schedd_instance_stats_partial_errors_total{node}`
    (counter for per-node dial failures).
  - Wire cardinality is bounded by `(#apps × #compute_nodes)`.
    With the one-box fleet of 100 apps on 1 node that's
    `O(100s)`; on a multi-node cluster with 1000 apps on 5 nodes
    that's `O(5000)` — comfortably under Prometheus's soft cap.
  - The Reader carries the per-instance rows in deterministic
    `(AppID, InstanceID)` order. `SnapshotForApp` /
    `SnapshotForInstance` / `SnapshotAll` are the stable contracts
    #171 (reaper) and #169 (reactive scale-up trigger) will call.
    A future PR that adds fields to `InstanceStat` is fine; one
    that renames or removes a field breaks the future PRs and is
    blocked.
  - NaN values (the "absent this tick" sentinel for CPU/RSS first
    samples and transient cgroup misses) are excluded from the
    rollup. The wire emits NaN for absent values; the rollup drops
    NaN before computing max/sum; the Prometheus client registry
    never sees NaN samples (a regression that emits NaN would
    surface in `promhttp_metric_handler_errors_total{cause="encoding"}`
    — pinned by `metrics_instancestats_test.go`).
  - The poller is the canonical place for the (app, node) tuple
    disambiguation: app_id and node_id are UUIDs / `[a-z0-9-]+`
    in production — neither can contain NUL — so the join byte
    (NUL) used inside `pkg/wire.ReplaceInstanceStats` is
    unambiguous.
  - PR-A leaves the wire-side population of CPUPct /
    InflightRequests / LastRequestAt at zero (PR-B's
    `ActivityTracker` + `stats.go` extraction will populate them).
    The poller respects the contract today: nil → Unknown, row
    lands with CPUPct=NaN and CPU=Unknown, the gauge rollup drops
    the NaN. LastRequestAt falls back to `state.Instance.LastRequestAt`
    so future "instance is idle" derivations have a signal to
    consume.
- **Out of scope (PR-B / future work):**
  - vmmd-side `ActivityTracker` (PR-B step 1).
  - `pkg/vmmdgrpc/stats.go` extraction + populate lease_uid / host_ip
    / resident_bytes / cpu_pct (PR-B step 2).
  - `pkg/vmmdgrpc/forward.go` `defer done()` wrapper for the bridge
    call (PR-B step 3).
  - `cmd/vmmd/main.go` ActivityTracker wiring (PR-B step 4).
  - #171 preferential reaper scale-down — reads from
    `SnapshotForApp`, adds `RecentCPUPct` / `RecentInflight` to
    `InstanceInfo`.
  - #169 reactive scale-up trigger — reads from `SnapshotAll` in a
    new Loop worker.
  - #172 config knobs (`StatsInterval`, `StatsFreshness`,
    `StatsEnabled`, future `CPUDeltaMinUSec` filter).
