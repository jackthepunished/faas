# Per-node live utilization on `/v1/admin/obs/nodes`

- Status: Draft (rev 2 — expands per-node CPU%, disk, wake-latency, anomaly)
- Date: 2026-08-10
- Scope: `migrations/`, `pkg/state/`, `cmd/schedd/`, `cmd/vmmd/`, `cmd/apid/`,
  `pkg/gateway/`, `pkg/wire/`, `pkg/api/obs.go`, `cmd/apid/spec_compliance_test.go`,
  `cmd/sdk-coverage/main.go`, `pkg/appmetrics/`.

## 1. Context

After PR #817 (operator-obs PR #3, ADR-091 §3) lands, the operator surface
ships `/v1/admin/obs/nodes` with **static capacity only**: `vpcpus`,
`mem_mb`, `max_concurrency`, `admission_ceiling_mb`, `last_heartbeat_at`,
`created_at`. There is no live utilization on the wire — the operator must
`curl` Prometheus `/metrics` to know "node-1 is at 67% RAM" or "node-1
had a 12s wake spike at 03:00".

The §6.2 invariant #2 — `Σ(ram_mb + 8) ≤ 47,600 MB` over live instances — is
the load-bearing admission number. Today it lives in the schedd engine, not
the operator wire.

PR #4 brings **all 5 per-node observability axes** to the operator surface
in a single PR:

1. **Live instance count + RAM admission** — derived from `instances` per
   `(node_name, state)` (§6.2 invariant #2 calculation, per node).
2. **Per-node CPU %** — sampled at heartbeat-mint time from the cgroup;
   rolled up over the last 60s.
3. **Per-node disk pressure** — `du -sb /srv/fc/snapshots/$node_name` + the
   spool scratchpad, polled into the heartbeat row.
4. **Per-node wake-latency p50/p95/p99** — new Prometheus histogram
   labelled by `node_name`; surfaces via the obs endpoint as computed
   quantiles (the existing fleet histogram stays untouched — the §12 SLA
   contract is preserved).
5. **Per-node anomaly detection** — extends the existing hour-of-day
   z-score detector with a per-node baseline, surfaces rows where the
   anomaly is concentrated on a single node (vs the current all-nodes
   baseline).

## 2. Blockers I found during recon

### 2.1 `instances` has no `node_name` column

`migrations/00001_init.sql:81` defines `instances` with no `node_name`
column. The one-box posture meant node binding was implicit. ADR-056
added `schedd.NodeName` config but never propagated it onto the
`instances` row.

Three options, with the recommended one picked:

| Option | Touches | Recommendation |
|---|---|---|
| (A) Add `instances.node_name` + have schedd write it | Schedd (breaks "schedd-owns-instances" rule) | ❌ cross-cutting |
| **(B) New `instance_node_bindings` table written by vmmd** | **Vmmd (already owns firecracker), migration, apid reader** | **✅ chosen** |
| (C) Derive node_name from `wake.requested` event data | Read-only, eventually-consistent | ❌ breaks if vmmd fails mid-event |

Option (B) keeps schedd untouched, puts the writer where the knowledge
already lives.

### 2.2 The unlabeled `gateway_wake_latency_seconds` histogram is a §12 contract

`pkg/appmetrics/appmetrics.go:175` reads the **unlabeled**
`gateway_wake_latency_seconds` for the fleet p95. Adding a `node_name`
label would bucket the existing query and break the §12 SLA scraper.

**Resolution**: add a **new** histogram
`gateway_wake_latency_seconds_by_node` (labelled by `node_name`) and
**keep the existing unlabeled one untouched**. The §12 fleet p95 contract
is preserved. The new per-node histogram is opt-in and documented as
operator-side only.

### 2.3 The CPU % sampling happens in cgroup, not in the heartbeat row

`pkg/wire/metrics.go` has `vmmd_stats_cpu_collect_seconds` (histogram of
collection duration) but no per-node CPU gauge. The cgroup read happens
in vmmd at stats-poll time; the number never lands in the heartbeat
row today.

**Resolution**: vmmd reads `cpu.stat` from the per-instance cgroup, sums
across the local instances, and folds the result into the next
`compute_node_heartbeats` row. The heartbeat row already exists, we just
add 2 columns (`cpu_pct_60s`, `disk_used_bytes`).

### 2.4 Per-node anomaly baseline is a new aggregate, not a re-architecture

The existing `/v1/admin/obs/anomalies` (PR #2) groups by
`(account_id, app_id, hour)`. Adding `node_name` to the GROUP BY is a
sqlc change plus a wire field. The hour-of-day baseline math is unchanged.

## 3. Decision

ADR-091 §3.6 was deliberately Postgres-only ("the obs surface reads
`usage_minutes`, not Prometheus"). This PR **does NOT contradict that
decision** for axes #1, #2, #3, #5 (all read Postgres tables or Postgres
rollups). For axis #4 (wake-latency p50/p99), the PR introduces a new
Prometheus histogram labelled by `node_name` and surfaces a PromQL
quantile computation in the apid handler. ADR-091 needs a §3.6 amendment
noting "per-node live stats aggregate from Postgres; per-node wake
latency surfaces via a PromQL eval over a new labelled histogram
(`gateway_wake_latency_seconds_by_node`) — the existing unlabeled
histogram remains the §12 fleet p95 source."

### 3.1 New table: `instance_node_bindings`

```sql
-- migrations/00194_instance_node_bindings.sql
-- +goose Up
create table instance_node_bindings (
  instance_id uuid primary key references instances(id) on delete cascade,
  node_name text not null,
  bound_at timestamptz not null default now(),
  released_at timestamptz
);
create index instance_node_bindings_node_name_idx
  on instance_node_bindings (node_name) where released_at is null;

-- +goose Down
drop index if exists instance_node_bindings_node_name_idx;
drop table if exists instance_node_bindings;
```

### 3.2 Migration: heartbeat-row columns

```sql
-- migrations/00195_compute_node_heartbeats_stats.sql
-- +goose Up
alter table compute_node_heartbeats
  add column if not exists cpu_pct_60s numeric(5,2),
  add column if not exists disk_used_bytes bigint;

-- +goose Down
alter table compute_node_heartbeats
  drop column if exists disk_used_bytes,
  drop column if exists cpu_pct_60s;
```

The `cpu_pct_60s` is a windowed aggregate (CPU used in the last 60s as a
percentage of `vpcpus * 100`). 2 decimal places because 0.01% granularity
is enough and `numeric` avoids float-precision concerns. The vmmd-side
calc maintains a sliding window across the last 60s of cgroup reads.

### 3.3 New Store interface methods

```go
// Instance-node binding (vmmd writes).
BindInstanceToNode(ctx context.Context, instanceID, nodeName string) error
ReleaseInstanceBinding(ctx context.Context, instanceID string) error

// Heartbeat row with the new CPU/disk fields populated.
InsertComputeNodeHeartbeatWithStats(
  ctx context.Context, nodeID string,
  receivedAt, lastHeartbeatAt time.Time,
  source string,
  cpuPct60s float64, diskUsedBytes int64,
) error

// Per-node live stats aggregate (replaces the inline subquery in the handler).
PerNodeLiveStats(ctx context.Context) ([]PerNodeStats, error)

// Per-node wake-latency rollup. The Prometheus eval lives in the
// handler (single call to apid's /metrics; bounded by HistogramVec).
// The store does NOT cache the quantile — that would stale out.
```

### 3.4 New sqlc queries

```sql
-- name: PerNodeLiveStats :many
select
  b.node_name,
  count(*) filter (where i.state in ('waking','cold_booting','running')) as instances_live,
  coalesce(sum(i.ram_mb + 8) filter (where i.state in ('waking','cold_booting','running')), 0)::bigint as ram_used_mb,
  count(*) filter (where i.state = 'running') as instances_running,
  count(*) filter (where i.state = 'waking') as instances_waking,
  count(*) filter (where i.state = 'cold_booting') as instances_cold_booting
from instance_node_bindings b
join instances i on i.id = b.instance_id
where b.released_at is null
group by b.node_name;

-- name: LatestHeartbeatStats :many
select distinct on (node_id)
  node_id, cpu_pct_60s, disk_used_bytes, received_at
from compute_node_heartbeats
order by node_id, received_at desc;

-- name: PerNodeAnomalies :many  -- extends ObsAnomalyRow with node_name
select
  account_id, app_id, node_name, minute,
  current, baseline_mean, baseline_stddev, baseline_samples, z_score, reason
from (
  -- same hour-of-day z-score CTE as today, but groups by
  -- (account_id, app_id, node_name, hour_of_day) instead of
  -- (account_id, app_id, hour_of_day).
) ...
```

### 3.5 Prometheus: new labelled histogram

```go
// pkg/gateway/metrics.go — alongside the existing wakeLatency
wakeLatencyByNode: prometheus.NewHistogramVec(prometheus.HistogramOpts{
    Name: "gateway_wake_latency_seconds_by_node",
    Help: "Per-node wake latency. The unlabeled gateway_wake_latency_seconds remains the §12 fleet p95 source.",
    Buckets: []float64{0.05, 0.1, 0.2, 0.3, 0.35, 0.5, 0.8, 1.0, 1.5, 3.0, 5.0, 10.0},
}, []string{"node_name"}),
```

The label cardinality is bounded by the compute node count (today: 1).
The §12 fleet p95 (`pkg/appmetrics/appmetrics.go:175`) keeps reading
the unlabeled histogram — no change.

### 3.6 Wire: `ObsNodeRow` extensions + new endpoint

`ObsNodeRow` gains 6 fields (the live stats). A new endpoint
`GET /v1/admin/obs/nodes/{name}/wake-latency` surfaces per-node quantiles
via PromQL eval over `gateway_wake_latency_seconds_by_node`. The existing
`/v1/admin/obs/anomalies` gains a `?group_by=node` filter (default
`app`, opt-in `node`) — same response shape, different GROUP BY.

### 3.7 Handler changes

- `obsListNodes` reads `PerNodeLiveStats` + `LatestHeartbeatStats`,
  folds onto the row projection. ~25 lines added.
- New `obsNodeWakeLatency` (~30 lines) — calls PromQL eval via the
  metrics scraper helper. Quantiles default to {p50, p95, p99}.
- `obsAnomalies` gets a `?group_by=node|app` parameter; the existing
  path is unchanged for `?group_by=app` (default).

## 4. Migration slot

Next free slot is 00194 (the 00190 admin-obs index landed in PR #1, 00191
+ 00192 were reserved/dropped per the recent fence resolution, 00193 was
not needed). **This PR needs two migrations**: 00194 (instance_node_bindings)
and 00195 (heartbeat stats columns). Verify no parallel PR is in flight
via `gh pr list --state open --search "migration"` and the cross-PR slot
fence pattern ([migration-gates-collision-and-replay]).

## 5. Tests (~200 lines)

- `TestInstanceNodeBinding_Lifecycle` — bind/release idempotency.
- `TestPerNodeLiveStats_EmptyDB` — zero instances → empty result.
- `TestPerNodeLiveStats_ParkedInstanceExcluded` — instance state='parked'
  must not contribute (§6.2 invariant #4).
- `TestPerNodeLiveStats_ReleasedBindingExcluded`.
- `TestPerNodeLiveStats_MultipleNodes`.
- `TestComputeNodeHeartbeats_StatsColumns` — InsertComputeNodeHeartbeatWithStats
  + read-back, assert the new columns are populated.
- `TestObsListNodes_LiveStatsSurfaced` — wire-level: bind + heartbeat,
  GET `/v1/admin/obs/nodes`, assert 6 new fields populated.
- `TestObsListNodes_AdmissionMargin` — assert `RAMAdmissionMarginMB =
  AdmissionCeilingMB - RAMUsedMB` (§6.2 invariant #2).
- `TestObsListNodes_HeartbeatStatsSurfaced` — assert cpu_pct_60s +
  disk_used_bytes fold onto the row.
- `TestObsNodeWakeLatency_FleetAndNode` — confirm the fleet histogram
  is unchanged (regression guard for §12 SLA), and the new labelled
  histogram surfaces the expected quantile.
- `TestObsAnomalies_GroupByNode` — seed per-node usage, verify the
  new group_by surfaces per-node rows.

## 6. Risks

- **Cross-cutting change** — touches vmmd (writer) + apid (reader) +
  pgstore + memstore + the wire DTO + Prometheus. Mitigated by isolating
  each writer behind the existing `Store` interface.
- **Component ownership** — vmmd is already the only firecracker-toucher;
  the binding write goes on the existing lifecycle path. Schedd is NOT
  touched.
- **Prometheus label cardinality** — `node_name` label is bounded by
  the compute node count. Today: 1. Tomorrow: bounded by the cluster
  size. Never user-supplied. ADR-091 §3.6 already documents the label
  cardinality constraint.
- **Backfill** — existing instances (pre-migration) have no binding
  row. The LEFT JOIN handles this. CPU%/disk on existing heartbeats is
  NULL until vmmd writes a new row. A future backfill PR can synthesize
  from `wake.requested` event data.
- **Pricing/billing** — invariant §6.7 says billing is on plan RAM + 8
  MB per running second. The new `ram_used_mb` is operator-only; meterd
  never reads it.
- **PR size** — this is a larger PR than the original §3 plan (~600
  lines of code + SQL, ~200 lines of tests, 2 migrations). Still
  reviewable in ~15 min if the commit is split into logical hunks
  (binding table, heartbeat columns, sqlc, wire, handlers, tests).

## 7. Out of scope (future PRs)

- Time-series visualization (Grafana scrape of the new histogram). The
  new histogram is exposed via /metrics; a Grafana dashboard is its
  own PR.
- Per-node build queue depth (the builder VM lifecycle — different
  component, separate PR).
- Per-node egress bytes (root-side vethHost counters — different table).
- Per-node snapshot restore latency (different histogram, different
  bucketing).

## 8. ADR-091 amendment

ADR-091 §3.6 needs an amendment noting:

> Per-node live stats (#1, #2, #3, #5) aggregate from Postgres
> (`compute_node_heartbeats`, `usage_minutes`). Per-node wake
> latency (#4) surfaces via a new Prometheus histogram
> `gateway_wake_latency_seconds_by_node` (labelled by `node_name`);
> the existing unlabeled `gateway_wake_latency_seconds` remains the
> §12 fleet p95 source. apid PromQL-evals the labelled histogram in
> `obsNodeWakeLatency` and surfaces quantiles as a response body.

That's an amendment to ADR-091 §3.6 (and a one-line addition to §3.6's
existing "Posture" section). No new ADR file.

## 9. Revision 2 — instance_node_bindings removed

**Status: §2.1 was wrong; this PR ships the corrected design.**

While implementing §3.1 I re-read `migrations/00024_compute_nodes.sql`
and discovered that **`instances.node_id` is already a NOT NULL FK to
`compute_nodes(id)`**, backfilled on pre-existing rows from the
synthetic `default-local` node. The premise of §2.1 — "`instances` has
no `node_name` column, so we need a new `instance_node_bindings`
table" — is false. `instances.node_id` has been carrying this data
since migration 00024 landed (long before PR #4).

Concretely:

- **§3.1 dropped.** No new `instance_node_bindings` table, no new
  migration `00194_instance_node_bindings.sql` (the slot is released).
- **§3.3 `BindInstanceToNode` + `ReleaseInstanceBinding` removed.**
  The Store interface gains only `AppendComputeNodeHeartbeatWithStats`,
  `LatestHeartbeatStats`, and `PerNodeLiveStats`.
- **`PerNodeLiveStats` joins on `instances.node_id = compute_nodes.id`**
  (the existing FK from migration 00024). The +8 MB overhead still
  mirrors §6.2 invariant #2.
- **vmmd lifecycle hooks (§3 of the v2 plan) are not needed.** vmmd
  has never written to `instance_node_bindings` because the table
  doesn't exist — and it doesn't need to. The hosting node is
  already tracked on the `instances` row, written by schedd's
  placement chooser at wake time.

Net effect: PR #4 ships three migrations minus one, three new Store
methods (not five), and zero new vmmd code paths. The wire shape and
the operator dashboard are unchanged.

The §8 amendment above already reflects the corrected list of source
tables — `compute_node_heartbeats` and `usage_minutes`, not
`instance_node_bindings`.

## 10. Verification

```
go test ./pkg/state/ ./pkg/gateway/ ./pkg/wire/ ./cmd/schedd/ ./cmd/vmmd/ ./cmd/apid/ -count=1 -timeout=180s
make sqlc-check
make spec-check
make lint
```

Manual smoke (against a running stack):

1. `psql -c "insert into instance_node_bindings ..."` to seed a binding.
2. `curl .../v1/admin/obs/nodes` — observe `instances_live`, `ram_used_mb`,
   `ram_admission_margin_mb`, `cpu_pct_60s`, `disk_used_bytes`.
3. Trigger a wake, observe `gateway_wake_latency_seconds_by_node` counter
   increment on the daemon's `/metrics`.
4. `curl .../v1/admin/obs/nodes/node-1/wake-latency` — observe {p50, p95, p99}.
5. `curl .../v1/admin/obs/anomalies?group_by=node` — observe per-node
   anomaly rows.
