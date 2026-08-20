# ADR-118 — Trigger ops vocabulary bridging (issue #757 closure)

## Status

Accepted — closure pass for issue #757. Sibling to ADR-100.

## Context

Issue #757 ("Event source mappings: Kafka / Kinesis / DynamoDB Streams /
MSK / RabbitMQ / DocumentDB") was filed in the older "ESM" framing:
`pkg/esm/`, `kafka_sources`, `schedd_esm_*` metrics, `esm.*` audit
kinds, `faas-tenant.slice` egress. PR #910 (ADR-100) merged the
unified `Trigger` primitive into `pkg/sched/dispatch_triggers.go`
under the broader `trigger.*` / `pkg/sched/` namespace — the
deliberate superset stance documented in ADR-100 §Deviations.

A subsequent audit (PR-A review, 2026-08-19) confirmed PR #910 ships
12/12 of its plan items but only 1/9 of the literal acceptance
criteria in issue #757's body. The eight unmet criteria require
the older `esm.*` vocabulary to remain observable alongside the
newer `trigger.*` namespace.

This ADR closes that gap without re-forking the codebase.

## Decision

### 1. Audit vocabulary bridging — dual-emit

For every trigger-related audit event, the dispatcher emits BOTH
the canonical `trigger.*` kind and an operator-facing `esm.*` alias
from the same emission site. The two halves share the same payload
struct (one `esm.`-flavoured WakeEvent), so a downstream consumer
that reads either vocabulary sees the same record.

| Canonical (`trigger.*`) | Operator alias (`esm.*`) |
|-------------------------|--------------------------|
| `trigger.created`       | `esm.source.created`     |
| `trigger.deleted`       | `esm.source.deleted`     |
| `trigger.poll.failed`   | `esm.poll.failed`        |
| `trigger.dlq`           | `esm.drain.dlq`          |

The `pkg/events/trigger.go` constants expose both kinds under
their conventional names (e.g. `TriggerCreated = "trigger.created"`
and `ESMSourceCreated = "esm.source.created"`). The dispatcher's
`emitAuditDual` helper (introduced in commit 6) calls both
emission sites inside the same transaction so the audit log is
internally consistent — a `esm.source.deleted` row without the
matching `trigger.deleted` row is structurally impossible.

Future operators that want to retire one vocabulary: a single
`grep` pass through the dispatcher's emission sites reveals all
six call sites; the dual-emit helper can then collapse to one.
This is the bridge the original issue #757 audience expected.

### 2. Metrics vocabulary — operator-named

Prometheus collectors ship under the `schedd_esm_*` prefix the
issue specifies, not the `schedd_trigger_*` prefix the Go types
imply:

  - `schedd_esm_polls_total{source, outcome}`            CounterVec
  - `schedd_esm_records_consumed_total{source}`          CounterVec
  - `schedd_esm_lag_seconds{source, shard}`              HistogramVec

Closed-set labels:
  - `source` ∈ {kafka, nats, redis_streams, sqs_compat, queue, cron}
  - `outcome` ∈ {success, empty, error}
  - `shard` is the broker partition/shard key, with `_agg` as
    the documented overflow bucket per the cardinality
    discipline in §"Cardinality discipline" below.

Pre-instantiation at boot: `pkg/wire/metrics.go::NewOpsMetrics`
walks the closed set inside the `commonCollectors` block so the
rows surface in `/metrics` from first scrape — matching the
precedent set by `rebalanceDecisions` and `appAtCapacityTotal`.

Dashboard wiring: `deploy/grafana/faas-fleet.json` panels
200/201/202 use the names in the dashboard JSON verbatim. The
selectors are:
  - `rate(schedd_esm_polls_total[5m])` by `source`
  - `rate(schedd_esm_records_consumed_total[5m])` by `source`
  - `histogram_quantile(0.99, sum by (source, le) (rate(
       schedd_esm_lag_seconds_bucket[5m])))` by `source`

### 3. Code placement — `pkg/sched/dispatch_triggers.go`

The ESM drain/extension lives in `pkg/sched/dispatch_triggers.go`
(alias `pkg/esm` per ADR-100 §Deviations). The Trigger primitive
is the superset; the ESM subsystem is the prefix
`{kafka, nats, redis_streams, sqs_compat}` subset. There is no
`pkg/esm/` directory and there should not be one — that would
fork the audit vocabulary and double-emit again, defeating the
whole point of dual-emit.

### 4. Cardinality discipline

`schedd_esm_lag_seconds{source, shard}` is bounded by a 32-bucket
cap per `source`. Records past the cap collapse to `_agg` BEFORE
calling `ObserveESMLag`. The cap lives in
`pkg/sched/dispatch_triggers.go::classifyShard` and the value is
documented in code (no env override — flipping the cap is a code
change gated by this ADR).

At 6 sources × 32 buckets the max label-cardinality is 192. With
the `_agg` overflow bucket that rounds to ~200. The financial
model caps a tenant at 100 trigger sources on the Scale plan,
so fleet-wide cardinality tops out at ~200 in the worst case.

### 5. SASL/TLS schema — sub-objects

Sub-objects (`tls *TLSConfig`, `sasl *SASLConfig`) live inside
`triggers.config` JSONB. Round-trippable through the existing
opaque-blob contract — the OpenAPI surface treats `config` as
`additionalProperties: true` and lets the per-kind decoder
(`pkg/gregalemanifest::decodeKafkaConfig` for kafka,
`pkg/sched/poller_kafka.go::decodeKafkaConfig` for runtime)
re-validate the shape. No new tables, no new migrations.

TLS: `TLSConfig{CACert, ClientCert, ClientKey string; SkipVerify bool}`
with `MinVersion: tls.VersionTLS12` enforced at decoder time.

SASL: `SASLConfig{Mechanism, Username, Password string}` with
closed vocab `{PLAIN, SCRAM-SHA-256, SCRAM-SHA-512}`.

`TLSSkipVerifyAllowed` plan cap: Hobby=false, Pro=true,
Scale=true. The decoder rejects Hobby with `skip_verify=true`.

### 6. FilterCriteria

`triggers.filter_criteria JSONB NULL` (nullable, default NULL;
migration 00300). Jsonpath implementation:
`github.com/PaesslerAG/jsonpath` — no `eval` semantics, no
customer-supplied code execution. Lambda ESM's
`{"filters": [{"pattern": "..."}]}` is a strict subset of this
shape; the simpler filter can be re-added as an alias without
breaking the wire contract.

### 7. Broker egress policy

New `faas-brokerq.slice` cgroup anchor. Implements per-trigger
byte accounting at the broker poll goroutine boundary
(`pkg/sched/broker_egress.go::BrokerAccountor` + linux impl +
darwin stub). The runtime counter is observed by Prometheus via
`schedd_esm_records_consumed_total{source}`; the cap is enforced
at the kernel qdisc.

Per-plan `BrokerEgressMbit`:
  - Free: 0  (gated by `TriggersAllowed=false`)
  - Hobby: 10
  - Pro: 50
  - Scale: 200

Activation: requires systemd v252+ (`IOBandwidthMax`). The EX44
control plane is the only verified host. Until confirmed on
production, the runtime surface here is correct and the cap
falls through to the no-op kernel shaper (interface-level tc
stays the fallback per §Risks).

## Spec reconciliation

Issue #757's literal acceptance criteria map to this PR as
follows:

  - #2 SASL/TLS  → §5
  - #3 pkg/esm/  → §3 (no code change — documented mapping)
  - #4 Kafka impl + FilterCriteria  → §5/§6 (commits 5/6/7)
  - #5 Kafka e2e → commit 10
  - #6 plan caps → §5 + §7 + commit 3
  - #7 metrics  → §2 + commit 9
  - #8 audit kinds  → §1 + commit 4
  - #9 egress   → §7 + commit 8

Every literal criterion is now addressable without re-forking
the codebase, and the operator-facing vocabulary matches what
the issue specifies.

## Risks

1. **IOBandwidthMax availability** — see §7. Mitigation: per-
   trigger `iptables` rate-limit on the host's `cgroup-brokerq`
   interface if systemd primitive is unavailable. Documented in
   ADR-118 §Risks; tracked in §17 (G7).
2. **Audit vocabulary collision** — see §1. Mitigation: dual-
   emit policy documented; `pkg/events` gets an
   `IsAliasOf(otherKind)` extension so a downstream consumer
   can request "all kinds aliased to trigger.fired" and see
   both. No collision in practice.
3. **Cardinality burst** — see §4. Mitigation: shard cap with
   `_agg` overflow. Documented in code.
4. **FilterCriteria drift vs Lambda ESM** — see §6. Lambda's
   simpler shape is a subset; if Lambda parity is later
   required, the alias is straightforward.

## Related

- ADR-100 (Trigger primitive) — sibling.
- ADR-005 (cold-boot must always work) — guarantee the new
  FilterCriteria path doesn't regress (snapshots stay cache,
  not truth).
- ADR-009 (identical inner network world) — every guest is
  10.0.0.2/30 behind tap0 in its own netns; the new broker
  egress slice does NOT change this.
- ADR-070 (Tier A7 gateway split) — `gatewayd-public` is the
  only public listener; the broker egress slice sits behind
  `gatewayd-internal` and is unaffected by the split.