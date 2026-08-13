# Grafana dashboard — `faas-fleet.json`

Grafana 11 export. Panels cover all 7 of the spec §12 dashboard rows
that are scorable today; one row remains deferred (rationale below).

## `warm-snapshot.json` (issue #470 / PR C / ADR-074)

Four-panel dashboard for the warm-snapshot tier ops surface: warm-capture
errors per reason (`*_warm_snapshot_errors_total{reason}`), guest-init
duration p50/p95 by app (`vmmd_guest_init_duration_seconds_bucket`),
wake-tier mix stacked (`schedd_wake_snapshot_tier_total{tier}`), and
snapshot-population-by-tier stat panel. UID `faas-warm-snapshot-pr-c`;
land via `dashboards/warm-snapshot.json` import after the ansible role
provisions the fleet dashboard (PR #141, ADR-031).

## `edge-rules.json` (issue #561 / PR B / ADR-091)

Four-panel dashboard for the edge-rules observability cluster: match
rate by kind (`gateway_edge_rule_match_total{outcome="match"}`), apply
rate by kind + result (`gateway_edge_rule_apply_total`, green=success,
red=error), JWT failure rate (`gateway_edge_rule_match_total{kind="jwt",outcome="failed"}`),
and compile-error stat panel (`gateway_edge_rule_compile_error_total{kind}`
— any non-zero paints red, a page-tier signal because a rule shipped
broken is an actionable correctness signal, not a headroom signal).
UID `faas-edge-rules-pr-b`. Mirror at
`deploy/ansible/roles/grafana/files/edge-rules.json` (byte-identical —
`make grafana-mirror-check` enforces the contract). Runbooks in
`docs/runbooks/`: `FaasEdgeRuleApplyHigh.md`, `FaasEdgeRuleCompileError.md`,
`FaasEdgeRuleJWTFailures.md`.

## Provisioning (PR #141, ADR-031)

The canonical install path is `deploy/ansible/roles/grafana/`, which
apt-installs Grafana OSS, SHA-256-pins the binary, provisions the
Prometheus datasource + this JSON from disk, and binds the management
bridge on `10.0.0.1:3000`. Run `make bootstrap` against a reference node to
provision; the dashboard lands at
`/d/faas-fleet-m8/faas-fleet-m8-12`.

For a hand-import path (developer laptop, external Grafana instance):

1. Open Grafana → Dashboards → Import.
2. Upload `faas-fleet.json`.
3. Select your Prometheus datasource (must be named or aliased
   `prometheus` — Grafana's import rewrites the datasource UID).
4. The dashboard lands at `/d/faas-fleet-m8/faas-fleet-m8-12`.

## Scrape source

The dashboard reads from the local Prometheus installed by
`deploy/ansible/roles/prometheus`. The scrape config there
(`prometheus.yml.j2`) targets every Gregale daemon + node_exporter on
the bridge IP. No remote source — the dashboard is single-node today (Tier A
will move to a federated scrape per ADR-031).

## Panels

| Panel | Metric | Spec §12 row |
|---|---|---|
| Wake latency p50 / p95 | `gateway_wake_latency_seconds` | wake latency |
| Wake queue wait p95 | `gateway_wake_queue_wait_seconds` | wake queue wait |
| Cold-boot fallback rate | `vmmd_cold_boot_fallback_total` / Σ(vmmd_ops_total{op=~"CreateFromSnapshot|CreateColdBoot"}) | cold-boot fallback rate |
| Snapshot fleet avg / p95 (MB) | `fcvm_snapshot_fleet_avg_bytes`, `…_p95_bytes` | snapshot fleet avg |
| Resident RAM % | `fcvm_resident_ram_pct` | resident_ram_pct_of_target |
| lv-fc used % | `fcvm_lv_fc_used_pct` | lv-fc utilisation |
| Wake rate | `gateway_requests_total` | — (operator sanity) |
| Edge rule apply rate | `gateway_edge_rule_apply_total{kind,result}` | edge rule apply rate |
| Edge rule compile errors | `gateway_edge_rule_compile_error_total{kind}` | edge rule compile errors |
| Build success rate (non-user_error) | `builderd_ops_total{op="build"}` | build success |
| Build queue wait p95 | `builderd_build_queue_wait_seconds` | build queue wait p95 |
| Build duration p95 (by outcome) | `builderd_build_duration_seconds` | per-outcome wall-clock |
| API availability (5m) | `gateway_requests_total{code=~"2.."}` / `gateway_requests_total` × 100 | public SLO |
| Resident GB per paying customer | `meterd_resident_gb_per_customer{plan}` | resident GB per paying customer |
| Per-route top 10 reqps + error rate (ADR-093) | `faas_gateway_request_rate_5m:by_route`, `faas_gateway_error_rate_5m:by_route` | per-route breakdown (opt-in) |
| Per-route top 10 p95 latency (ADR-093) | `faas_gateway_p95_seconds:by_route` | per-route p95 (opt-in) |

## Deferred rows

- **Per-app SLO row** — the per-app p95 wake + 5xx rate are too
  high-cardinality for the fleet-level dashboard. They live on the
  status page instead (see `deploy/statuspage/index.html`).

## Source of truth

`docs/faas_implementation_spec.md` §12 lists every dashboard row.
Renames must update the spec first, then the metric, then this
dashboard — never the other way around.