-- +goose Up
-- +goose StatementBegin

-- filename: 00067_extend_metering_telemetry.sql
--
-- 00067_extend_metering_telemetry.sql — ADR-048 (extend metering
-- telemetry: ingress bytes, WakeMethod, builder-time, usage_daily
-- rollup; visibility only).
--
-- Four additive-merge columns on usage_minutes:
--   net_rx_bytes     — cumulative byte delta on root-side
--                      vethHost.tx_bytes (root→guest = ingress) for
--                      this instance in this minute. Source: vmmd
--                      pkg/fcvm/netstats.Cache TX path, exposed via
--                      vmmd.Stats → schedd instancestats.Poller →
--                      meterd Sampler.SampleAndRoll. Closes the
--                      audit finding "ingress is invisible" — a
--                      customer cannot use the egress path as an
--                      infinite free upload pipeline once metered.
--   cold_boot_count  — per-minute count of WAKE_RESTORE→WAKE_COLD_BOOT
--                      transitions observed for this instance, detected
--                      by the meterd sampler from the LastWakeMethod
--                      field on scheddgrpc.InstanceStatsRow. Cold-boot
--                      cache hit-rate is now visible from the meter
--                      surface without depending on Prometheus scrapes.
--   builder_seconds  — billable builder VM seconds, written once per
--                      build at build completion via a new
--                      AppendBuilderUsage Store method keyed by
--                      build_id. Counts the 2-vCPU/2048-MB builder
--                      microVM time (spec §4.5). NOT counted in
--                      CountsForRAM() — the GB-RAM-hour billing shape
--                      for runtime instances is unchanged. Feeds the
--                      future "builder minutes overage" pricing tier
--                      without committing to it.
--   builder_kind     — parallel to builds.kind (railpack/dockerfile/
--                      tarball); 'none' for non-build rows. Lets the
--                      dashboard show which build kind dominates
--                      a customer's box-cycle burn.
--
-- All four are additive on (instance_id, minute) because the meterd
-- sampler can call AppendUsage many times within a minute. The
-- billing-floor columns (mb_seconds, requests) keep first-write-wins
-- (PR feat/m7-beta-hardening); cpu_usec (issue #279, ADR-039), and
-- tx_bytes + net_tx_bytes (ADR-046) already established the additive
-- pattern this migration follows.
--
-- Out of scope (mirrors ADR-039 §"What is NOT in this PR" + ADR-046):
--   - pkg/billing/{provider.go,stripe/usage.go,paddle/usage.go} — untouched.
--   - pkg/api/limits.go — no new quota/ladder field.
--   - The Stripe gb_ram_hour push shape — unchanged.
--   - Historical backfill (the migration default is 0).
--
-- The columns are the seam for the future telemetry-billing PR
-- which extends Provider.PushUsageRecord. No provider push fires
-- in this migration.
--
-- usage_monthly view (originally in 00002_app_manifest_and_domains.sql,
-- recreated in 00055_usage_minutes_cpu.sql, then again in
-- 00066_usage_minutes_egress.sql) is recreated here to include the
-- new columns. Migrations are append-only — we do not edit the prior
-- migrations; the OR REPLACE makes the view idempotent.
--
-- usage_daily rollup table is added as a parallel materialised
-- surface. The dashboard / /v1/usage/daily read path uses it instead
-- of scanning usage_monthly. A new meterd cron tick
-- (FAAS_ROLLUP_INTERVAL, default 5 min) populates it via INSERT ...
-- SELECT ... GROUP BY from usage_minutes with an ON CONFLICT additive
-- merge so a redelivered cron never double-counts.

alter table usage_minutes
    add column if not exists net_rx_bytes     bigint  not null default 0,
    add column if not exists cold_boot_count  integer not null default 0,
    add column if not exists builder_seconds  bigint  not null default 0,
    add column if not exists builder_kind     text    not null default 'none';

comment on column usage_minutes.net_rx_bytes is
    'Cumulative byte delta on root-side vethHost.tx_bytes (root→guest = ingress) for this instance in this minute. Source: vmmd pkg/fcvm/netstats.Cache TX path reading /sys/class/net/<vethHost>/statistics/tx_bytes → vmmd.Stats → schedd instancestats.Poller → meterd Sampler.SampleAndRoll → AppendUsage. ADR-048. Informational — not billed. Unit = interface bytes (includes Ethernet/IP framing).';

comment on column usage_minutes.cold_boot_count is
    'Per-minute count of WAKE_RESTORE→WAKE_COLD_BOOT transitions observed for this instance. Source: scheddgrpc.InstanceStatsRow.LastWakeMethod, sampled by meterd Sampler.SampleAndRoll. ADR-048. Informational — not billed. Idempotent on a redelivered tick within the same minute (only the transition counts).';

comment on column usage_minutes.builder_seconds is
    'Billable builder VM seconds (2-vCPU / 2048-MB per spec §4.5), written once per build at build completion via state.Store.AppendBuilderUsage keyed by build_id. ADR-048. Informational — not billed. NOT counted in CountsForRAM() — runtime GB-RAM-hour billing is unchanged.';

comment on column usage_minutes.builder_kind is
    'Build kind parallel to builds.kind (railpack / dockerfile / tarball); ''none'' for non-build rows. ADR-048. Informational — not billed.';

-- Recreate usage_monthly view to include the new columns. Same
-- pattern as 00055 (cpu_usec) and 00066 (tx_bytes / net_tx_bytes).
drop view if exists usage_monthly;
create or replace view usage_monthly as
  select
    account_id,
    app_id,
    date_trunc('month', minute) as month,
    sum(mb_seconds)     as mb_seconds,
    sum(cpu_usec)       as cpu_usec,
    sum(requests)       as requests,
    sum(tx_bytes)       as tx_bytes,
    sum(net_tx_bytes)   as net_tx_bytes,
    sum(net_rx_bytes)   as net_rx_bytes,
    sum(cold_boot_count) as cold_boot_count,
    sum(case when builder_kind <> 'none' then builder_seconds else 0 end) as builder_seconds
  from usage_minutes
  group by account_id, app_id, date_trunc('month', minute);

-- usage_daily rollup table. Populated by the meterd cron tick
-- (pkg/meter/rollup.go + FAAS_ROLLUP_INTERVAL, default 5 min).
-- PK (account_id, app_id, day) makes the cron additive-merge
-- idempotent under redelivery (the INSERT ... ON CONFLICT clause
-- in pkg/meter/rollup.go re-aggregates a sliding window and
-- adds to existing rows). The secondary index supports the
-- /v1/usage/daily?day=YYYY-MM-DD read path (account-scoped
-- recent-first).
create table if not exists usage_daily (
    account_id      uuid         not null,
    app_id          uuid         not null,
    day             date         not null,
    mb_seconds      bigint       not null default 0,
    requests        bigint       not null default 0,
    cpu_usec        bigint       not null default 0,
    tx_bytes        bigint       not null default 0,
    net_tx_bytes    bigint       not null default 0,
    net_rx_bytes    bigint       not null default 0,
    cold_boot_count bigint       not null default 0,
    builder_seconds bigint       not null default 0,
    rolled_up_at    timestamptz  not null default now(),
    primary key (account_id, app_id, day)
);

create index if not exists usage_daily_account_day_idx
    on usage_daily (account_id, day desc);

comment on table usage_daily is
    'Per-(account, app, day) materialised rollup of usage_minutes. Populated by the meterd cron tick FAAS_ROLLUP_INTERVAL (default 5 min) via INSERT ... SELECT ... GROUP BY with ON CONFLICT additive merge. Read by GET /v1/usage/daily. ADR-048. Informational — not billed.';

comment on column usage_daily.cold_boot_count is
    'Per-day sum of usage_minutes.cold_boot_count for this (account, app, day). ADR-048. Informational — not billed.';

comment on column usage_daily.rolled_up_at is
    'Timestamp the meterd cron last wrote this row. Stamped on every ON CONFLICT update so a stuck cron is visible in /v1/usage/daily metadata.';

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

-- Reverse of Up. Drop usage_daily, restore the pre-ADR-048 view
-- shape (no net_rx_bytes, no cold_boot_count, no builder_seconds
-- in the SUM), then drop the new columns. Down is a clean reversal
-- of Up; we do not touch 00055 / 00066 — the next migration that
-- recreates the view will overwrite this one again on its own Up.
--
-- NOTE: same caveat as 00066 Down (PR-414 I7): prior migrations'
-- Down blocks do NOT recreate the view because they predate the
-- multi-column view-recreation pattern. THIS Down DOES recreate
-- it (parity with Up) because any future migration that depends
-- on the pre-048 columns being present when going Down (e.g. a
-- downstream column-add that expects a known view shape) must see
-- a stable view. The asymmetry is intentional.

drop table if exists usage_daily;

drop view if exists usage_monthly;
create or replace view usage_monthly as
  select
    account_id,
    app_id,
    date_trunc('month', minute) as month,
    sum(mb_seconds)   as mb_seconds,
    sum(cpu_usec)     as cpu_usec,
    sum(requests)     as requests,
    sum(tx_bytes)     as tx_bytes,
    sum(net_tx_bytes) as net_tx_bytes
  from usage_minutes
  group by account_id, app_id, date_trunc('month', minute);

alter table usage_minutes
    drop column if exists net_rx_bytes,
    drop column if exists cold_boot_count,
    drop column if exists builder_seconds,
    drop column if exists builder_kind;

-- +goose StatementEnd