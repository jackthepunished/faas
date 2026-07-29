# ADR-049 — Metering ops surfaces: drift detector, indexes, storage rollup, retention

- **Status:** Accepted
- **Date:** 2026-07-29
- **Decision:** Land four ops-side closures on the metering pipeline — a
  push↔usage drift detector (`pkg/billing/reconciler`), a `(account_id,
  minute DESC)` partial index on `usage_minutes`, a `snapshot_storage_daily`
  rollup table for per-(account, app, day) snapshot+layer byte totals, and
  a meterd retention cron that DELETEs `usage_minutes` rows older than 13
  months — without changing the billable shape of the system. The drift
  detector ships fail-soft and never blocks the loop on a single account;
  the retention cron ships as a weekly DELETE (B.4.c), with declarative
  partitioning deferred to a follow-up PR.

- **Why:** the 2026-07-29 metering audit flagged four ops gaps that the
  producer-side PRs (ADR-046, ADR-048) cannot address on their own:

  1. **No push↔usage drift detector.** If Stripe silently drops a
     `usage_record` push — a real Stripe SDK failure mode on 5xx, network
     blips, or rate-limit — no Prometheus gauge fires. The customer is
     silently under-billed (revenue leakage) or over-billed (refund
     liability), and the operator only learns when the customer complains.
     `pkg/meter/pusher.go:138` already dedupes; it does not reconcile.
  2. **No `(account_id, minute)` index on `usage_minutes`.** The
     `UsageByHour`, `UsageByMonth`, and `CurrentMonthOverageCents` queries
     are all heap scans today (`pkg/state/pgstore.go:4596`, `:5310`,
     `:5281`). At the audit's measured minute-row cardinality (≈ 5 M rows
     per month on the EX44 fleet), `/v1/usage?month=2026-07` is a ~400 ms
     query; with index it is < 5 ms.
  3. **No snapshot / app-layer storage metering.** `snapshots.mem_bytes`
     and `snapshots.disk_bytes` exist (`pkg/state/pgstore.go:3929`) but
     are never summed per account. The 130 MB/sandbox fleet-average
     target has no per-app visibility today, and a "Pro gets 1 GB
     included / overage €X/GB-month" line item cannot be priced without
     the rollup.
  4. **No retention policy.** `usage_minutes` grows forever. The
     13-month retention requirement (financial model §1: "billing
     disputes window") is implicit; nothing enforces it. A meterd
     outage silently loses minute-rows (`pkg/meter/sampler.go:193` writes
     only the current minute); the synthetic-row recovery path is also
     missing.

  ADR-039 (cpu_usec, migration 00055), ADR-046 (egress bytes, migration
  00066), and ADR-048 (ingress bytes + usage_daily rollup) all deferred
  ops-side closures. ADR-049 closes the second of those two halves:
  schema + seams first (PR-A), producers + ops dashboards second
  (PR-B). This is the same PR-1 / PR-2 cadence as ADR-046.

## Context

The meterd single-writer invariant on `usage_minutes` is preserved: the
retention cron runs in meterd (`cmd/meterd/main.go`), the storage rollup
runs in meterd, and the drift detector reads `usage_minutes` but does not
write to it. None of these surfaces mutate the per-minute grain.

The append-only migration contract is preserved. New artifacts are:
- `usage_minutes_account_minute_idx` (migration 00069) — `CREATE INDEX
  CONCURRENTLY IF NOT EXISTS`, runs outside a transaction so the
  in-place index build does not lock writes.
- `snapshot_storage_daily` table (migration 00070) — `(account_id,
  app_id, day)` PK with the same `(account_id, day DESC)` index pattern
  as `usage_daily`.

`Provider.PushUsageRecord` is **not** touched. The drift detector
introduces a new read-only method `Provider.ReconcileUsage(ctx, account,
hour, observedMBSeconds) (pushedMBSeconds int64, err error)` on the
`billing.Provider` interface (pkg/billing/provider.go). Stripe +
Paddle implementations call their respective summary endpoints;
failures log a warning + bump `meterd_billing_drift_reconcile_failures_total`
and skip the account.

## Decision (one paragraph each)

**Drift detector.** New package `pkg/billing/reconciler`. Loop tick
default 6 h (`FAAS_RECONCILE_INTERVAL`). For each paid account, sum
`usage_minutes.mb_seconds` for the last 24 h window and compare against
the provider's last-24h push summary. Expose
`meterd_billing_drift_mb_seconds{account_id, provider}` (signed) and
`meterd_billing_drift_ratio{account_id, provider}` (`abs(drift) /
max(local, pushed)`). A new Prometheus alert
(`deploy/ansible/roles/prometheus/files/faas.rules.yml`,
`alert: BillingDrift`) pages on `ratio > 0.005 for 1h`. **No financial
model change.** **No provider push path mutation.**

**`(account_id, minute)` index.** Migration 00069 adds
`usage_minutes_account_minute_idx` via `CREATE INDEX CONCURRENTLY`. The
migration runner splits the file outside a transaction (same pattern as
the existing `cmd/migrate/main.go` concurrent-index carve-out). Test
pins the planner picks an Index Scan over the new index for `WHERE
account_id = $1 AND minute >= $2 AND minute < $3`.

**Storage rollup.** New table `snapshot_storage_daily (account_id,
app_id, day, snapshot_bytes, layer_bytes, computed_at)`. New
`pkg/meter/storage.go` cron tick `FAAS_STORAGE_ROLLUP_INTERVAL` (default
1 h) sums `snapshots.mem_bytes + disk_bytes` + overlay staging per
`(account, app, day)`. New DTO `StorageUsageResponse` exposes the
rollup through `GET /v1/usage/storage?day=YYYY-MM-DD`. SDK method
`StorageUsage(ctx, day)`. New Prometheus gauge
`meterd_storage_bytes{account_id, app_id}` (the rollup source-of-truth).

**Retention + outage catch-up scaffolding.** New `pkg/meter/retention.go`
cron tick `FAAS_RETENTION_INTERVAL` (default 1 day, aligned with §11
Sunday 04:00 UTC reboot window) runs `DELETE FROM usage_minutes WHERE
minute < now() - interval '13 months'`. Migration 00069 also adds a
partial index on `usage_minutes(minute)` to keep the DELETE cheap.
Synthetic-row recovery is **scaffold-only** this PR: `pkg/meter/sampler.go`
detects a ≥ 2-tick gap and logs a warning. The `synthetic` column +
backfill insert are deferred to a follow-up PR (B.5 alternative).

## Consequences

**Positive:**
- Drift detector closes the revenue-leakage gap. The alert fires on
  real Stripe / Paddle outages that would otherwise go unobserved.
  Operationally: a single P95 query to identify the offending account.
- `(account_id, minute)` index turns `/v1/usage` from 400 ms → < 5 ms.
  At audit-measured cardinality (5 M rows/month), the planner picks an
  Index Scan automatically.
- Storage rollup unblocks a future "Pro plan gets 1 GB included" line
  item — out of scope for this PR but the data is now available.
- Retention cron enforces the 13-month financial-model requirement
  without operator intervention. Bounded `usage_minutes` cardinality.

**Negative / accepted tradeoffs:**
- `CREATE INDEX CONCURRENTLY` cannot run inside a transaction block.
  The migration runner splits the file at the index boundary (mirrors
  `cmd/migrate/main.go`'s existing carve-out). A migration mid-flight
  crash leaves the build in `INVALID` state; the runner detects and
  re-issues `REINDEX` on the next boot. No silent corruption.
- The drift detector's Stripe `UsageRecordSummaries.list` call adds a
  new SDK query surface. The Stripe SDK has known rate-limit issues at
  >100 RPS; the fail-soft path means a rate-limit is a missed
  reconciliation tick, not a meterd crash. Prom rule compensates.
- Retention cron is a `DELETE` (B.4.c, not B.4.a declarative
  partitioning). Vacuum cost on `usage_minutes` after a 13-month
  retention tick is non-trivial on the EX44 (~5 minutes wall-clock
  measured in pre-prod). Deferred to a follow-up PR with the
  partitioning migration once weekly DELETE behaviour is measured.
- Synthetic-row scaffold logs gaps but does NOT emit a backfill row.
  Outage-driven underbilling is still possible; a follow-up PR adds
  the column + insert path once gap-detection tests demonstrate the
  value.

**Reversibility:**
- Drift detector: delete `pkg/billing/reconciler/` + the alert rule +
  the meterd wiring. No data lost.
- Index: `DROP INDEX CONCURRENTLY usage_minutes_account_minute_idx`.
- Storage rollup: drop the table + cron. `snapshots.mem_bytes` /
  `disk_bytes` rows are unaffected.
- Retention: stop the cron. Rows persist. (Slightly larger blast radius
  if a misfire deletes too aggressively — bounded by the
  `WHERE minute <` predicate + the partial-index scan being cheap.)

**Audit gates closed by this ADR:**
- Gap #1 (drift detector): ✅ §B.1
- Gap #2 (`(account_id, minute)` index): ✅ §B.2
- Gap #7 (storage rollup): ✅ §B.3
- Gap #8 (retention + catch-up scaffolding): ✅ §B.4 + §B.5 (partial)

**Out of scope (deliberately deferred):**
- Declarative monthly partitioning (B.4.a) — separate PR after
  retention cron has been measured for one cycle.
- Synthetic-row column + backfill insert (B.5 alternative) — separate
  PR once gap-detection tests prove the value.
- Provider push for any of the new informational columns (ADR-046 /
  ADR-048 / ADR-049 all defer the billing integration; the financial
  model is unchanged).
