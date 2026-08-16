# FaasUpstreamRttDegraded

Source: `deploy/ansible/roles/prometheus/files/faas.rules.yml`, the
`FaasUpstreamRttDegraded` alert block (issue #876, ADR-098 §12).

Metric: `meterd_data_upstream_rtt_ms_bucket{kind, host_redacted_hash, region}`
— a histogram bucketed per (closed-vocab kind, sha256(salt||host)
8-hex prefix, meterd node's compute_nodes.region). The alert reads
p95 RTT via `histogram_quantile(0.95, sum by (le, region) (rate(...[5m])))`
so a single app's degraded RTT surfaces as a per-region inflation of
the percentile. The `host_redacted_hash` label is the §11 barrier —
the plaintext host NEVER appears in any label, audit emit, or log
line; the on-disk `data_upstreams.host` column is the only place
the plaintext lives (kept for inspection, dropped by the probe
loop once the dial resolves, see `pkg/meter/upstream_probe.go`).

Severity: `warn` (capacity, not outage). A degraded RTT to a
customer's database or cache means the chooser may be biasing
wakes toward a region that has no transcontinental edge — the
placement still completes, but with a 100ms+ tail. The `family`
label is `upstream_rtt` so the existing `family`-based inhibition /
silencing rules in `alertmanager.yml.j2` compose with this alert.

## Symptom

The alert fires when the per-region p95 RTT over the rolling 5m
window exceeds 200ms for 10m:

| Alert | Trigger | For |
|---|---|---|
| `FaasUpstreamRttDegraded` | `histogram_quantile(0.95, sum by (le, region) (rate(meterd_data_upstream_rtt_ms_bucket[5m]))) > 200` | 10m |

The `region` dimension preserves the meterd node's compute_nodes.region
so Alertmanager groups by `[alertname, region]` — a single region
degraded fires one page with the region in the summary. Multiple
regions degraded at the same time is rare and indicates a fleet-wide
last-mile issue (separate signal — see FaasNetworkEgressHigh).

## Check

1. `kubectl logs -n meterd deploy/meterd --tail=200 | grep -i 'probe'`
   — look for repeated `outcome=timeout` / `outcome=tls_handshake`
   from the same host_redacted_hash prefix. The 8-hex prefix in the
   log line is the first 8 chars of the host_redacted_hash; correlate
   against `data_upstreams.host` for the matching app/scoped row.
2. `psql -c "SELECT host_redacted_hash, region, ok, count(*) FROM data_upstream_probes WHERE sampled_at > now() - interval '15 minutes' GROUP BY 1,2,3,4 ORDER BY 4 DESC LIMIT 10"`
   — the top failing (host, region) pairs over the last 15 min.
3. Reachability test: from the meterd node, `curl -sIk --max-time 5
   https://<host>` (replace `<host>` with the plaintext from
   `data_upstreams.host` after correlating via host_redacted_hash)
   — should match the per-region TLS handshake latency the
   histogram_quantile is reporting.

## Recover

1. **Customer-side first.** The alert is downstream of a customer's
   own upstream — the customer's managed Postgres / Redis / API
   may have degraded. Confirm with the customer before any
   cluster-side action.
2. **Cluster-side workarounds.** If the customer can't fix the
   upstream, the chooser bias (`FAAS_UPSTREAM_AFFINITY=1`) is
   doing its job — wakes are already being steered toward the
   region's lower-latency region. Verify on the dashboard's
   `gateway_wake_region_distribution` panel.
3. **Recompute the score cache.** If the meterd probe loop was
   restarted mid-incident, the TTL'd `UpstreamAffinity` cache
   in schedd (`pkg/sched/upstream_affinity.go`) may be holding
   stale scores. The cache is bounded to `api.UpstreamAffinityTTL`
   (default 30 s) so a 1-minute wait is the canonical self-heal.
4. **Disable the alert.** If the upstream is permanently degraded
   and the customer is OK with the latency tail, silence the
   alert for the affected `region` label via the standard
   `family=upstream_rtt` Alertmanager silencer — do NOT mute
   `family` globally, the other alarm rules depend on the same
   inhibition graph.
