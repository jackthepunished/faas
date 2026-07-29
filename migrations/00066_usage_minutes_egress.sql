-- +goose Up
-- +goose StatementBegin

-- filename: 00066_usage_minutes_egress.sql
--
-- 00066_usage_minutes_egress.sql — ADR-046 (per-instance egress
-- metering, visibility only).
--
-- Two additive-merge columns on usage_minutes:
--   tx_bytes     — cumulative HTTP response body bytes the gateway
--                  forwarded for this instance in this minute. Source:
--                  pkg/gateway/handler.go:statusRecorder.Bytes, folded
--                  into the per-(instance, minute) ring buffer the
--                  meterd sampler drains.
--   net_tx_bytes — cumulative byte delta on root-side
--                  vethHost.rx_bytes for this instance in this minute.
--                  Source: vmmd pkg/fcvm/netstats.Cache reading
--                  /sys/class/net/<vethHost>/statistics/rx_bytes,
--                  exposed via vmmd.Stats → schedd
--                  instancestats.Poller → meterd Sampler.SampleAndRoll.
--
-- Both are additive on (instance_id, minute) because the meterd
-- sampler can call AppendUsage many times within a minute; the
-- billing-floor columns (mb_seconds, requests) keep first-write-wins
-- (PR feat/m7-beta-hardening), and cpu_usec (issue #279, ADR-039)
-- already established the additive pattern this migration follows.
--
-- Out of scope (mirrors ADR-039 §"What is NOT in this PR"):
--   - pkg/billing/{provider.go,stripe/usage.go,paddle/usage.go} — untouched.
--   - pkg/api/limits.go — no new quota/ladder field.
--   - per-plan tc tbf shaping (pkg/netns/config.go:569-575) — unchanged.
--   - The Stripe gb_ram_hour push shape — unchanged.
--   - Historical backfill (the migration default is 0).
--
-- The columns are the seam for the future egress-billing PR which
-- extends Provider.PushUsageRecord. No provider push fires in this
-- migration.
--
-- usage_monthly view (originally in 00002_app_manifest_and_domains.sql,
-- recreated in 00055_usage_minutes_cpu.sql) is recreated here to
-- include the new columns. Migrations are append-only — we do not
-- edit 00002 or 00055; the OR REPLACE makes the view idempotent.

alter table usage_minutes
    add column if not exists tx_bytes     bigint not null default 0,
    add column if not exists net_tx_bytes bigint not null default 0;

comment on column usage_minutes.tx_bytes is
    'Cumulative HTTP response body bytes the gateway forwarded for this instance in this minute. Source: pkg/gateway/handler.go statusRecorder.Bytes → per-(instance, minute) ring buffer → meterd Sampler.SampleAndRoll → AppendUsage. ADR-046. Informational — not billed.';

comment on column usage_minutes.net_tx_bytes is
    'Cumulative byte delta on root-side vethHost.rx_bytes for this instance in this minute. Source: vmmd pkg/fcvm/netstats.Cache reading /sys/class/net/<vethHost>/statistics/rx_bytes → vmmd.Stats → schedd instancestats.Poller → meterd Sampler.SampleAndRoll → AppendUsage. ADR-046. Informational — not billed. Unit = interface bytes (includes Ethernet/IP framing).';

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

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

-- Restore the pre-ADR-046 view shape (no tx_bytes, no net_tx_bytes)
-- and drop the new columns. Down is a clean reversal of Up; we do
-- not touch 00055 — the next migration that recreates the view
-- will overwrite this one again on its own Up.
--
-- NOTE (PR-414 I7): 00055's Down does NOT recreate the view
-- because it predates the multi-column view-recreation pattern.
-- This Down DOES recreate it (parity with Up) because any
-- future migration that depends on the pre-046 columns being
-- present when going Down (e.g. a downstream column-add that
-- expects a known view shape) must see a stable view. A
-- maintainer "fixing" 00065 to match 00055 will silently break
-- that contract. The asymmetry is intentional.

drop view if exists usage_monthly;
create or replace view usage_monthly as
  select
    account_id,
    app_id,
    date_trunc('month', minute) as month,
    sum(mb_seconds) as mb_seconds,
    sum(cpu_usec)   as cpu_usec,
    sum(requests)   as requests
  from usage_minutes
  group by account_id, app_id, date_trunc('month', minute);

alter table usage_minutes
    drop column if exists tx_bytes,
    drop column if exists net_tx_bytes;

-- +goose StatementEnd
