# Gate-A — Per-node schedd + schedd-side async placement claim (Phase 2)

Spec §14, Phase 2 / Gate A. Founding doc for the multi-host
topology: N peer-equal schedds, each owning a slice of apps via
`apps.node_id`, and `gatewayd` routing each customer request to the
owner schedd. The mid-PR architectural pivot moved the placement
chooser out of `apid` (where the depguard rule
`apid-control-plane-only` forbids it) into schedd via
`pkg/sched.PlacementClaimSubscriber`. See
[`../adr/062-tier-a-per-node-schedd-and-placement.md`](../adr/062-tier-a-per-node-schedd-and-placement.md).

## Definitions

- **Owner schedd.** The schedd instance whose `Engine.OwnerNodeID`
  matches `apps.node_id` for a given app. Exactly one schedd owns
  any given app at any moment; the conditional
  `UPDATE … WHERE node_id IS NULL` in `Store.SetAppNodeID` is the
  serialisation primitive.
- **App owner.** The `compute_nodes.id` recorded on `apps.node_id`
  (migration 00083). Set once at claim time, **immutable
  post-claim** for v1.0 — rebalance is a v1.1 follow-up.
- **Eligibility.** A compute node is eligible to host apps iff
  `compute_nodes.active = true`. Inactive rows are skipped by
  schedd's chooser and absent from `gatewayd`'s dial cache.

## Topology

N peer-equal schedd instances, one per non-`default-local` active
compute node. No leases, no leader election, no advisory locks, no
consensus. Each schedd:

1. Resolves its `OwnerNodeID` from `cfg.NodeName` →
   `compute_nodes.id` at startup (via `ActiveComputeNodes`).
2. Maintains its own VMMRouter + per-node schedd client cache
   (gateway-side).
3. Filters every pg_notify feed by `app.node_id == OwnerNodeID`
   before issuing vmmd calls. Closes the duplicate-`Prime` hazard.
4. Runs the `PlacementClaimSubscriber` on `NotifyAppChanged`;
   races the conditional UPDATE to stamp owners on `kind="created"`.

`default-local` remains the synthetic single-box seed node
(migration 00024 + 00083 backfill). When a box runs in single-node
mode (no non-`default-local` is active), `cfg.NodeName` is empty
and schedd falls through to the legacy fleet-wide posture with
`OwnerNodeID = ""`.

The shared Postgres is the coordination point: the conditional
UPDATE serialises every schedd into one winner per unplaced app.

## Active-passive is control-plane only — do not invent `compute_nodes.state`

Phase 2 / Gate A does **not** introduce `compute_nodes.state`. There
is no per-schedd `state` column, no `standby`/`active`/`drained`
semantics on `compute_nodes`. The active-passive concept
applies only to the gateway DNS cutover (which is orthogonal to
schedd peer equality) and is not changed by this gate.

If you find yourself wanting to mark a schedd "standby", flip
`compute_nodes.active = false` instead — the chooser skips it and
the gateway stops dialling it.

## Compute eligibility — `compute_nodes.active`

Drain a compute node without removing it:

```bash
psql -c "UPDATE compute_nodes SET active = false WHERE name = 'fsn-2';"
```

Reverse:

```bash
psql -c "UPDATE compute_nodes SET active = true WHERE name = 'fsn-2';"
```

The `compute_node_changed` notify fires on UPDATE; the schedd's
router refresh watcher rebuilds its dial cache and the gateway's
per-node schedd client cache rebuilds its target URL map.

## Adding a second compute node

The minimum operator surface for cutting over a second compute
node:

```bash
# 1. Insert the row in compute_nodes.
psql <<'SQL'
INSERT INTO compute_nodes (
  name, active, target_url, schedd_target_url,
  vcpus, mem_mb, max_concurrency, admission_ceiling_mb
) VALUES (
  'fsn-2', true,
  'tcp://10.0.0.2:7000', 'tcp://10.0.0.2:7100',
  80, 28000, 10, 23800
);
SQL

# 2. Bootstrap vmmd on the new box (per deploy/ansible).
ssh faas-fsn-2 'make bootstrap'
ssh faas-fsn-2 'systemctl enable --now faas-vmmd'

# 3. Bootstrap schedd on the new box with the per-node identity.
ssh faas-fsn-2 'FAAS_NODE_NAME=fsn-2 systemctl enable --now faas-schedd'

# 4. Verify the schedd resolved its owner and stamped at least one
#    claim (or none, if no unplaced apps existed at boot).
ssh faas-fsn-2 'journalctl -u faas-schedd --since -1m | grep "claim\|owner"'
```

Schedd startup refuses to come up if:

- `FAAS_NODE_NAME` is empty while any non-`default-local` is
  active. **Fail-fast on ambiguity.**
- `FAAS_NODE_NAME` matches no active row. **Fail-fast on typo.**
- `FAAS_NODE_NAME` resolves to `default-local` while any
  non-`default-local` is active. **Fail-fast on synthetic
  poisoning.**

## Cold-start sweep

`pg_notify` is fire-and-forget. A schedd that was down while an
apid createApp landed missed the `kind="created"` notify. To close
the missed-event window, `cmd/schedd` runs a one-shot
`store.ListUnplacedApps()` + `engine.ClaimUnplaced` per row at
boot. Sub-second in steady state; one tx per unplaced app.

Operator diagnosis:

```bash
# Are there any unplaced apps right now?
psql -c "SELECT id, slug, account_id FROM apps WHERE node_id IS NULL AND status <> 'deleted';"
```

If non-empty and a schedd is healthy, the cold-start sweep or the
live subscriber will reconcile within seconds. If non-empty and no
schedd is running, start one — the sweep will run on its first
tick.

## Gateway wake race (sub-second, ≤ 5 s pathological)

A fresh app's first Wake may arrive in the gateway before the
schedd has stamped `node_id`. The gateway's per-node schedd client
cache rejects empty NodeID today and the existing fallthrough path
in `cmd/gatewayd/scheddrouter.go` handles it. The customer's
TCP-level retry closes any gap.

No new gateway retry/poll is added in this gate. The schedd
claim latency is sub-second in steady state. Document but do not
hand-hold.

## Validation matrix

| Check | Where | When |
|---|---|---|
| `select name, active, schedd_target_url from compute_nodes` | psql | continuous; all non-default rows must have `schedd_target_url` populated |
| `select node_id, count(*) from apps group by node_id` | psql | continuous; should split across non-default rows within seconds of a createApp |
| `select node_id, count(*) from instances group by node_id` | psql | continuous; should mirror the apps distribution once wakes land |
| `select id, slug from apps where node_id is null and status <> 'deleted'` | psql | continuous; empty in steady state, ≤ 1 during a cold-start sweep |
| `journalctl -u faas-schedd \| grep "stamped owner"` | box | continuous; one log per successful claim |
| `gateway_wake_latency_seconds` p95 ≤ 1 s | `pkg/wire` ops metrics | continuous; alert if breached for 5 min (`FaasWakeLatencyHigh`) |
| `faas_build_success_pct` ≥ 99 % | `pkg/wire` ops metrics | continuous; alert if breached for 5 min (`FaasBuildSuccessLow`) |
| `make migrations-check` exit 0 | CI | continuous; gates 00083 + 00084 |
| `make test` exit 0 | CI | continuous |

## Rollback

If a Phase 2 schedd surfaces a regression (e.g. two schedds stamp
the same app to different owners — `apps.node_id` flipping on each
notify — see the rejection case in
`pkg/sched/placement_claim_test.go::TestClaimUnplaced_RaceLosesSilently`),
rolling back is local:

1. **Stop the new schedd.** On the new box:
   ```bash
   ssh faas-fsn-2 'systemctl stop faas-schedd'
   ```
2. **Mark it inactive.** So the gateway stops dialling and the
   chooser skips it:
   ```bash
   psql -c "UPDATE compute_nodes SET active = false WHERE name = 'fsn-2';"
   ```
3. **Rebind apps to a single owner.** The legacy `default-local`
   backfill from migration 00083 stays in place; do **not** flip
   rows manually — the chooser would just re-stamp them on the
   next cold start. Leave the rollback at "one schedd serves
   everyone via default-local" and triage in a follow-up PR.

`vmmd`, `imaged`, `meterd`, `builderd`, `apid`, `gatewayd` are
unaffected by the rollback.

## Removed in this gate

The following constructs from the pre-Phase-2 runbook are
**removed** and MUST NOT be referenced:

- `compute_nodes.node_name` (replaced by `name`)
- `compute_nodes.state` ('active' / 'standby' / 'drained')
- `compute_node_notify` channel
- `pkg/scheduler` package
- `pkg/scheduler` per-app admission path

If you find a reference in another doc or runbook, file a
follow-up to delete it.

## Acceptance

The runbook is required by spec §14 Phase 2 / Gate A. The test
that pins its presence + section shape is
`TestRunbooks_GateA_ExistsAndHasRequiredSections` in
`cmd/e2e/runbooks_e2e_test.go`.