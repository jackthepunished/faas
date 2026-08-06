# ADR-048 — Extend metering telemetry: ingress bytes, WakeMethod, builder-time, usage_daily rollup

- **Status:** Accepted
- **Date:** 2026-07-29
- **Decision:** Land four additive-merge columns on `usage_minutes` (`net_rx_bytes`,
  `cold_boot_count`, `builder_seconds`, `builder_kind`) plus a `usage_daily`
  materialised rollup table, end-to-end into the meter surface (`/v1/usage`,
  `/v1/usage/summary`, `/v1/usage/daily`, `/v1/account/export`, `faas usage`),
  without touching the billing surface. The data is sampled, persisted, and
  exposed — but **no provider push is added** in this set of PRs. The
  `Provider.PushUsageRecord` extension (Stripe / Paddle) for any of the new
  columns is the follow-up that lands on top of this seam, exactly as ADR-039
  (cpu_usec) and ADR-046 (tx_bytes / net_tx_bytes) deferred their billing
  integration to later PRs.
- **Why:** an audit on 2026-07-29 surfaced four gaps between what the meter
  records and what an operator needs to run a billing company:

  1. **Ingress is invisible.** `vethHost.tx_bytes` (root→guest) is never read.
     Egress is metered (ADR-046); ingress isn't. A customer can shovel
     arbitrary bytes *into* a guest for free — using the platform as an
     asymmetric free-upload channel. Symmetric to egress; the egress pattern
     transfers 1:1.
  2. **Cold-boot vs warm-wake is invisible from the meter surface.**
     `pkg/scheddgrpc/server.go:357` exposes a `WakeMethod` proto enum
     (`WAKE_COLD_BOOT / WAKE_RESTORE`), but `pkg/sched/instancestats/poller.go:155`
     does not propagate it to the Reader. The M3 §14 wake-latency acceptance
     gate (park→wake p50 ≤ 350 ms) depends on snapshot hit-rate, which today is
     only inferable from Prometheus — not from a customer-visible usage row.
  3. **Builder VM time is not metered.** `pkg/builderd/builderd.go:308` emits
     `build_duration_seconds{outcome}` for observability only. Free-tier
     builders cost real box RAM (2 vCPU / 2048 MB per spec §4.5); a customer
     who runs thousands of OOM-bomb builds burns box cycles invisibly.
  4. **`/v1/usage` is a per-minute scan of `usage_minutes`.** The
     `usage_monthly` view is a `GROUP BY` over every minute row since account
     creation. There is no per-day materialised surface, and the table grows
     unbounded (audit finding). A dashboard query is a heap scan + hash
     aggregate.

  ADR-039 (cpu_usec, migration 00055) and ADR-046 (egress bytes, migration
  00066) established the measurement-only precedent this ADR follows: persist
  the data, expose it through every usage surface, freeze the unit, defer
  billing. ADR-048 extends that pattern to four additional surfaces, all
  informational, none billed.

## Context

The meterd single-writer invariant on `usage_minutes` is preserved:
`pkg/meter/sampler.go::SampleAndRoll` is the only path that calls
`AppendUsage`. New producers (ingress bytes from `vethHost.tx_bytes`,
WakeMethod transitions from schedd wire, builder seconds from builderd
terminal events) accumulate into existing seams — `EgressSource`-style
interfaces with nil-source `0,0` defaults (`pkg/meter/sampler.go:335-340`).

The append-only migration contract (`migrations/00055_usage_minutes_cpu.sql:40-43`,
`migrations/00066_usage_minutes_egress.sql`) is preserved. New columns are
`bigint NOT NULL DEFAULT 0` (or `text NOT NULL DEFAULT 'none'`,
`integer NOT NULL DEFAULT 0`) added with `ADD COLUMN IF NOT EXISTS`. The
`usage_monthly` view is recreated with `CREATE OR REPLACE VIEW` so prior
migrations' Down blocks are unaffected (same precedent as 00066 Down).
**No historical backfill** — the migration default is 0 (audit confirms
this is the established pattern; a backfill would be a separate
intentional decision).

The `usage_daily` rollup is the first materialised table the meterd
populates via a cron tick. The PK is `(account_id, app_id, day)` and the
cron uses `INSERT … ON CONFLICT … DO UPDATE SET … = … + EXCLUDED.`
(additive merge), so a redelivered cron never double-counts. The table
is the read-side surface for `GET /v1/usage/daily?day=YYYY-MM-DD` and
for the dashboard's "today so far" panel.

## Decision

### 1. Column inventory

| Column | Type | Source | Wire path | Sampler seam |
|---|---|---|---|---|
| `net_rx_bytes` | `bigint NOT NULL DEFAULT 0` | `vethHost.tx_bytes` (root→guest = ingress) via `/sys/class/net/<vethHost>/statistics/tx_bytes` | `vmmd.Stats.IngressTxBytes` → `schedd.instancestats.Poller` → `meterd.scheddIngressAdapter` | Extend `EgressSource` to `IngressBytes(instanceID) (rxBytes uint64, ok bool)`. Nil-source returns `0,0,false`. |
| `cold_boot_count` | `integer NOT NULL DEFAULT 0` | `WakeMethod` transition `WAKE_RESTORE→WAKE_COLD_BOOT` between two consecutive ticks for the same `(instance, minute)` | `scheddgrpc.InstanceStatsRow.LastWakeMethod` → `schedd.instancestats.Poller` → `meterd.SampleAndRoll` | New `wakeMethodSource` map in sampler guarded by `wakeMethodMu`. Transition-only; idempotent on a redelivered tick within the same minute. |
| `builder_seconds` | `bigint NOT NULL DEFAULT 0` | `time.Since(build.started_at)` at build terminal event (success / failure / abort) | `cmd/builderd/reaper.go` → `state.Store.AppendBuilderUsage(build_id)` | New `Store.AppendBuilderUsage(ctx, accountID, appID, buildID, finishedAt, kind, seconds)` keyed by `build_id`. NOT counted in `CountsForRAM()`. |
| `builder_kind` | `text NOT NULL DEFAULT 'none'` | Parallel to `builds.kind` enum (`railpack` / `dockerfile` / `tarball`); `'none'` for non-build rows | Same as `builder_seconds` | Same. |

All four are additive on `(instance_id, minute)` (or on `build_id` for
builder rows). The `mb_seconds` / `requests` billing-floor columns keep
first-write-wins semantics (unchanged). The `cpu_usec` / `tx_bytes` /
`net_tx_bytes` precedent established by ADR-039 and ADR-046 is the
template.

### 2. Unit / source of truth

- **`net_rx_bytes`** is **interface bytes on root-side `vethHost.tx_bytes`**
  (includes Ethernet/IP framing). Unit pinned by comment on the column
  and by the new `pkg/fcvm/netstats/cache_metal_test.go::TestNetstatsIngressRegressionEndToEnd`
  which boots a real guest, drives scripted ingress (a `curl` upload through
  the gateway), and asserts `vethHost.tx_bytes` delta equals
  `usage_minutes.net_rx_bytes` sum within 0 bytes. The same test pattern
  that ADR-046 §7 promises for egress (audit confirms that metal test
  is missing in trunk today — this ADR ships it for both directions at
  once).
- **`cold_boot_count`** counts only the `WAKE_RESTORE → WAKE_COLD_BOOT`
  transition between two ticks. A redelivered tick within the same
  minute does not increment the count (the second tick sees the same
  `LastWakeMethod`). The `WAKE_COLD_BOOT → WAKE_RESTORE` direction does
  not decrement; the count is a per-minute "did this minute see a cold
  boot?" flag, not a balance.
- **`builder_seconds`** is wall-clock `time.Since(build.started_at)` at
  the moment `builds.status` transitions to `succeeded` / `failed` /
  `aborted` (spec §4.5 build timeouts). Includes any internal retries —
  the box burned the cycles either way. NOT computed against
  `builds.finished_at` minus `builds.started_at` to avoid timezone
  surprises; the sampler/builderd pair owns the clock.

### 3. `usage_daily` materialised rollup

| Column | Type | Source |
|---|---|---|
| `(account_id, app_id, day)` | PK | Caller (cron tick) |
| `mb_seconds, requests, cpu_usec, tx_bytes, net_tx_bytes, net_rx_bytes, cold_boot_count, builder_seconds` | `bigint NOT NULL DEFAULT 0` | `SUM(...) FROM usage_minutes WHERE minute >= window_start AND minute < window_end` |
| `rolled_up_at` | `timestamptz NOT NULL DEFAULT now()` | Stamped on every `ON CONFLICT` update |

The cron tick (`FAAS_ROLLUP_INTERVAL`, default 5 min) runs in meterd
alongside the existing six ticks. It reads
`MAX(rolled_up_at)` from `usage_daily` (or `now() - 5 min` on first
run), aggregates the sliding window, and inserts with `ON CONFLICT
(account_id, app_id, day) DO UPDATE SET … = … + EXCLUDED.…` so
redelivery is idempotent.

The view `usage_monthly` is recreated in migration 00067 to add
`SUM(net_rx_bytes)`, `SUM(cold_boot_count)`, and
`SUM(case when builder_kind <> 'none' then builder_seconds else 0 end)`
to its existing columns. View recreation follows the same `DROP VIEW IF
EXISTS + CREATE OR REPLACE VIEW` shape as 00055 (cpu_usec) and 00066
(tx_bytes / net_tx_bytes).

### 4. Out of scope (mirrors ADR-039 + ADR-046 "What is NOT in this ADR")

- `pkg/billing/{provider.go,stripe/usage.go,paddle/usage.go}` — untouched.
  `Provider.PushUsageRecord` is not extended; the new columns are not
  pushed to Stripe or Paddle.
- `pkg/api/limits.go` — no new quota/ladder field, no new overage
  constant. The seam is the column, not the limit.
- `pkg/netns/config.go:569-575` (per-plan `tc tbf` shaping) — unchanged.
  Ingress metering does NOT enable per-plan ingress shaping; that is a
  separate ADR (would need a mirror `tc tbf` on `vethHost.tx_bytes`).
- The Stripe `gb_ram_hour` push shape — unchanged.
- Historical backfill — the migration default is 0; per the audit,
  backfilling would require a one-shot `UPDATE usage_minutes SET …`
  sourced from the durable `instances.started_at` / `terminated_at`
  log. That's a separate decision (the audit's "single biggest
  structural gap" — covered by ADR-049 PR-B instead).
- Wake-method-billed (per-cold-boot pricing). The column exists; the
  pricing decision is deferred.
- Builder-bomb quota enforcement. The column exists; the consumer
  quota work is separate.
- Partitioning `usage_minutes` by month. Audit-confirmed "PARTITION BY
  is absent everywhere". ADR-049 PR-B addresses retention via a
  weekly DELETE cron first; partitioning is a follow-up PR after one
  cycle of measured retention behavior.

### 5. Schema growth

Per the audit: 4 × `bigint` + 1 × `integer` + 1 × `text` per
`usage_minutes` row (~40 bytes). At 1 minute × 100 instances × 50 apps
× 1.5× the existing 16-byte egress overhead = ~120 KB/min additive
on top of the `cpu_usec` + egress cost. The `usage_daily` table grows
at one row per `(account, app, day)` — bounded by `n_accounts ×
n_apps × n_days`; at 1k accounts × 5 apps × 365 days = 1.8M rows/year,
~250 MB/year. Both fit comfortably on the reference node NVMe.

## Consequences

- **Wire observation grows.** `vmmdgrpc.InstanceStatsRow` gains
  `IngressTxBytes uint64` + `IngressTxValid bool` + `LastWakeMethod int32`.
  `scheddgrpc.InstanceStatsRow` mirrors. `schedd.instancestats.Poller`
  propagates both. Schedd skips rows where the values are nil (same
  shape as ADR-046 §7).
- **Schema growth.** `usage_minutes` gains 4 additive columns + a
  recreated `usage_monthly` view. `usage_daily` is a new materialised
  table populated by a meterd cron. All `ADD COLUMN IF NOT EXISTS`
  so the migration is idempotent under replay.
- **The new columns are informational.** A future billing PR must
  define the unit (interface bytes vs IP bytes vs payload bytes for
  ingress; per-cold-boot vs per-wake for WakeMethod; per-second vs
  per-build for builder time) before pushing. This ADR freezes:
  - `net_rx_bytes` = **interface bytes on root-side `vethHost.tx_bytes`**
    (mirror of ADR-046's egress unit decision).
  - `cold_boot_count` = **per-minute transition count** (not a balance).
  - `builder_seconds` = **wall-clock from `builds.started_at` to
    build terminal event** (not the spec §4.5 10-minute budget).
- **The sampler keeps a 30 s TTL on the schedd source.** If schedd is
  unreachable for >30 s, the sampler writes 0 for `net_rx_bytes` and
  0 for `cold_boot_count` for that minute (same operational shape as
  `cpu_usec` per ADR-039). Builder writes are not on the schedd
  path — they go straight from builderd to `state.Store.AppendBuilderUsage`,
  so they are unaffected.
- **The OpenAPI / apid handler / CLI panels mirror the columns.** The
  `usage_monthly` view's SUMs are the source for `UsageResponse` /
  `UsageSummaryResponse`. The new `DailyUsageResponse` DTO and
  `GET /v1/usage/daily?day=` endpoint are added with `description:
  "Informational; not billed."` on every new field (ADR-046 precedent).
- **The `pkg/api/limits.go` table is untouched.** No new limit field,
  no per-plan ingress quota, no overage constant. The seam is the
  column, not the limit.
- **`usage_daily` populated by a meterd cron tick.** The tick is the
  seventh loop goroutine alongside `sample / quota / stripe / dunning /
  residency / alerts` (`pkg/meter/config.go:3-13`). `FAAS_ROLLUP_INTERVAL`
  env knob, default 5 min. Idempotent under redelivery (ON CONFLICT
  additive merge).
- **SDK regen is mandatory.** `make sdk-gen` regenerates
  `sdk/node/src/generated/` + `sdk/python/faas_sdk/`. `make sdk-check`
  pins the new `DailyUsage` SDK method. `cmd/sdk-coverage` adds the
  `GET /v1/usage/daily` row to `methodRouteMap` (auto-derivation may
  pick the wrong verb for the new route — manual entry is the
  precedent).

## Verification

Per migration 00067 test (`migrations/00067_extend_metering_telemetry_test.go`):

1. All four new columns exist on `usage_minutes` with the expected
   type / nullability / default (`bigint NOT NULL DEFAULT 0` for
   numeric, `integer NOT NULL DEFAULT 0` for `cold_boot_count`,
   `text NOT NULL DEFAULT 'none'` for `builder_kind`).
2. Insert omitting the new columns succeeds; defaults apply.
3. Insert with the new columns populated round-trips correctly.
4. `usage_monthly` view sums all four new columns.
5. Explicit NULL into the new bigint columns raises SQLSTATE 23502
   (proves NOT NULL was applied at table-create, not via backfill).
6. `usage_daily` table exists with the expected columns.
7. `usage_daily` PK conflict path: second INSERT ... ON CONFLICT
   additive-merge results in the expected summed totals.
8. `usage_daily_account_day_idx` exists in `pg_indexes`.

Per producer tests (PR-A tasks A.3 / A.5):

- `pkg/state/pgstore_append_usage_test.go::TestPg_AppendUsage_AddsNetRxBytesAndColdBootCountOnConflict` —
  pins additive merge for `net_rx_bytes` + `cold_boot_count` (mirrors
  ADR-046's `…_AddsTxBytesAndNetTxBytesOnConflict:168`).
- `pkg/state/pgstore_builder_usage_test.go::TestPg_AppendBuilderUsage_FirstWriteWinsPerBuildID` —
  three cases (success, redelivered webhook, NOT NULL on `builder_kind`).
- `pkg/meter/sampler_egress_test.go::TestSampler_NilIngressSourceWritesZero` and
  `…_IngressDeltaIsAdditive` — mirror the egress counterparts.
- `pkg/meter/sampler_wake_method_test.go::TestSampler_RestoreToColdAddsOne` —
  four cases (transition adds 1, restore→restore no-op, cold→cold no-op,
  redelivered tick no-op).
- `pkg/meter/sampler_builder_test.go::TestSampler_BuilderSuccessAndFailureBothAppend` —
  three cases (success adds `builder_seconds`, failed build also adds,
  redelivered webhook is no-op).
- `pkg/meter/rollup_test.go::TestMeter_Rollup_IsAdditiveUnderRedelivery` —
  the cron is idempotent across two ticks covering the same window.
- `pkg/fcvm/netstats/cache_test.go::TestCache_TxRegressionEndToEnd` —
  eight branches mirroring the egress eight; same regression-safe
  shape.
- `pkg/fcvm/netstats/cache_metal_test.go::TestNetstatsIngressRegressionEndToEnd`
  (//go:build metal) — boots a real guest, drives scripted ingress,
  asserts kernel `vethHost.tx_bytes` delta equals persisted
  `net_rx_bytes` sum within 0 bytes. Closes the audit's "missing
  metal test" gap for both directions at once.
- `pkg/vmmdgrpc/stats_egress_test.go` and `pkg/scheddgrpc/stats_egress_test.go`
  — extended with ingress counterpart + WakeMethod propagation.
- `pkg/sched/instancestats/poller_test.go::TestPoller_PropagatesLastWakeMethod`
  — pins the wire field propagation.
- `cmd/e2e/egress_metering_test.go::TestEgressMetering_AppendBuilderUsageReadback` —
  end-to-end happy path; builderd → AppendBuilderUsage → `usage_daily`
  read-back via `GET /v1/usage/daily`.

CI gates PR-A must pass: `make test` (race), `make test-state-coverage`
(≥70 %), `make sqlc-check`, `make migrations-check` (slot collision +
replay-safety), `make spec-check` (vacuum), `make sdk-check`,
`make sdk-gen` (aggregator dirty-diff), `golangci-lint v2.4.0`, `gofmt`,
`go vet`, `govulncheck`, `make e2e`. **`pkg/api/limits.go` untouched.**
**`pkg/billing/provider.go` untouched.** **No financial-model edit.**