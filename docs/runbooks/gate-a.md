# Gate-A — Active-passive HA topology

Spec §14 M8 row 5 ("Gate-A runbook (2nd box active-passive)").
Founding doc R3 per `docs/STATUS.md`. This is the v1 reference for
the second-box HA topology — the moment we go from one box (a
single 56 GB tenant budget, one FC-detect, one `compute_nodes`
row) to two boxes where one is "the public face" and the other is
"ready to take over".

The runbook is split into: topology, promotion (standby → active),
failover (active dead → standby active), rollback, validation.

## Topology

Two physical boxes at one Hetzner FSN + HEL pair (per spec
§14 row "Regional expansion"). Wire identity:

| Role | Hostname | DB row in `compute_nodes` | Public face |
|---|---|---|---|
| Primary (active) | `faas-fsn-1` | `node_name='fsn-1'`, `state='active'` | yes — `apps.<domain>` resolves here |
| Stand-by (passive) | `faas-fsn-2` | `node_name='fsn-2'`, `state='standby'` | no — receives only shadow wakeups |

Both boxes share the same Postgres (Hetzner-managed, single
primary + read replica in FSN; FSN-2 reads the same WAL stream
for crash-consistent shadow data). The shared Postgres is the
coordination point that makes active-passive work — the standby
is always within ~1 s of the primary's `instances` state because
both subscribe to the same `pg_notify` channel that schedd
publishes on (see `pkg/sched`).

DNS is the public cutover: both boxes hold a wildcard TLS cert
for `*.apps.<domain>`, but only the active box's A record is
published. Promotion / failover is the act of moving the A
record.

Each box runs the full daemon fleet (apid, schedd, vmmd, imaged,
meterd, gatewayd, builderd) — no "control plane on one box,
compute on another" split until Gate-B. vmmd is local-root on
each box (spec §11 — `vmmd` is the only root component); per
ADR-028, the stand-by does NOT serve traffic until promoted.

See `pkg/scheduler` for the per-app admission path that ties
together `compute_nodes.state` + the `instances` rows. See
`pkg/wire` for the ops metrics the Prometheus alerts reference
(`up{job=~"apid|gatewayd|...|githubd"}`, `gateway_wake_latency_seconds`).

## Promotion steps — stand-by → active

"Promotion" is the planned cutover: FSN-2 becomes the new active
while FSN-1 is taken out for maintenance (drained, upgraded,
re-imaged). The reverse direction (FSN-1 → FSN-2) is identical
with the roles swapped.

1. **Verify the stand-by is healthy.** On FSN-2:
   ```bash
   ssh faas-fsn-2 'systemctl is-system-running && \
     for d in apid schedd vmmd imaged meterd gatewayd builderd githubd; do \
       systemctl is-active faas-$d || exit 1; \
     done'
   ```
   All seven daemons must be `active`; if any is `failed`, abort
   and triage (see `docs/runbooks/FaasDaemonDown.md`).
2. **Verify the stand-by is in sync.** On FSN-2:
   ```bash
   curl -fsS 'http://127.0.0.1:9090/api/v1/query?query=pg_stat_replication_replay_lag'
   ```
   `replay_lag < 1 s` is the gate. See
   `docs/runbooks/FaasSnapshotFleetHigh.md` for the
   `pg_basebackup` lifecycle that backs the sync.
3. **Block writes on the active.** On FSN-1:
   ```bash
   ssh faas-fsn-1 'systemctl stop faas-gatewayd faas-apid faas-schedd'
   ```
   The order matters: gatewayd first (so no new public
   requests), then apid (the only writer to apps/deployments),
   then schedd (the only writer to `instances`). Leave vmmd,
   imaged, meterd, builderd, githubd running so they can drain
   in-flight work.
4. **Wait for drain.** On FSN-1:
   ```bash
   timeout 60 sh -c 'until [ "$(curl -fsS http://127.0.0.1:9090/api/v1/query?query=count(up{job=\"gatewayd\"}%3D%3D1))" = 0 ]; do sleep 1; done'
   ```
   60 s matches the spec §6.4 idle timeout; adjust if your
   tail changed it.
5. **Flip the DB role.** On FSN-2 (run as the role that owns
   `compute_nodes`):
   ```bash
   psql -c "UPDATE compute_nodes SET state='active' WHERE node_name='fsn-2';"
   psql -c "UPDATE compute_nodes SET state='drained' WHERE node_name='fsn-1';"
   ```
   Both updates land in the same transaction; the `pg_notify`
   on `compute_node_notify` wakes schedd on FSN-2 which
   transitions from `standby` to `active`.
6. **Stop the old primary.** On FSN-1:
   ```bash
   ssh faas-fsn-1 'systemctl stop faas-vmmd faas-imaged faas-meterd faas-builderd faas-githubd'
   ```
7. **Move the DNS A record.** Update the wildcard
   `apps.<domain>` A record from FSN-1's address to FSN-2's.
   TTL matters: ≤ 60 s for a planned promotion, ≤ 300 s is
   acceptable for a fresh install.
8. **Verify the public face.** From outside:
   ```bash
   curl -fsS -H 'Host: any-app.apps.<domain>' https://<fsn-2>/healthz
   ```
   200 OK = promotion successful. The status page
   (`https://<fsn-2>/status`) will read `degraded` for up to
   30 s while the `statusCache` refills; that's cosmetic.

## Failover steps — active dead → stand-by active

"Failover" is the unplanned cutover: FSN-1 is unreachable
(network split, kernel panic, lost ZFS pool), and we promote
FSN-2 in `< 5 min` to keep the SLO budget.

1. **Confirm FSN-1 is actually down.** Check via Hetzner Cloud
   Console (the box-level metric + serial console). Don't
   trust Prometheus alone — a `gatewayd` scrape failure on
   FSN-1 looks identical to FSN-1 being down.
2. **Run steps 2-8 of "Promotion" above**, replacing "stand-by
   is healthy" with "stand-by is healthy **enough**". A stand-by
   with a 5 s replay lag is acceptable for failover; a stand-by
   with `replay_lag > 60 s` should be paged on separately and is
   not a viable failover target.
3. **Page the on-call.** Even on a successful failover, the
   primary being unexpectedly down is a page-severity event.
   The alert `FaasDaemonDown` does NOT distinguish per-host, so
   the dashboard page is the source of truth until
   per-host alerts land (ADR-039).

## Rollback

If promotion surfaces a regression (e.g. FSN-2 rejects every
Nth wake because its `gateway_wake_latency_seconds` is 5x
FSN-1's number — see the 30-day baseline in `pkg/wire` ops
metrics), rolling back is the same dance reversed.

1. **Stop writes on FSN-2** (steps 3-4 of Promotion).
2. **Restore FSN-1.** If the original FSN-1 was lost (kernel
   panic + lost disk), run `make bootstrap` against the
   replacement Hetzner box with the same `node_name='fsn-1'`.
   The new node fills its `compute_nodes` row as `drained`,
   which is the correct state for the rollback target.
3. **Flip the DB role back.** Mirror step 5 of Promotion with
   the roles swapped.
4. **Move the DNS A record back.** TTL ≤ 300 s means the
   rollback completes inside one TTL window.

## Validation matrix

| Check | Where | When |
|---|---|---|
| `gateway_wake_latency_seconds` (p95) ≤ 1 s | `pkg/wire` `DefaultOpsMetrics` | continuous; alert if breached for 5 min (`FaasWakeLatencyHigh`) |
| `faas_build_success_pct` ≥ 99 % | `pkg/wire` `DefaultOpsMetrics` | continuous; alert if breached for 5 min (`FaasBuildSuccessLow`) |
| `pg_stat_replication_replay_lag < 1 s` | PromQL ad-hoc | on every promotion; `< 5 s` acceptable for failover |
| HTTP 200 from `https://<active-box>/status` | smoke | after every promotion / failover |
| HTTP 200 from `https://<active-box>/status/slo.json` with `degraded=false` | smoke | after every promotion / failover; the cache takes 30 s to fill |
| `make backup-restore-drill` exit 0 | `docs/drills/<UTC>-restore-drill.md` | quarterly + before every promotion |
| `make lint-drill` exit 0 | CI | continuous |

## Acceptance

This runbook is required by spec §14 M8 row 5. The test that
pins its presence + section shape is
`TestRunbooks_GateA_ExistsAndHasRequiredSections` in
`cmd/e2e/runbooks_e2e_test.go`.
