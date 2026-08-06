-- +goose Up
-- +goose StatementBegin

-- Issue #667 — waitUntil(post-response tail) primitive.
-- ADR-078. Two replay-safe column adds; both default to 0 so existing
-- rows keep their pre-PR shape bit-for-bit (no backfill required).

-- 1. instances.tail_count — the in-flight tail task counter.
--    Bumped by the runner on every waitUntil(promise) registration
--    (raced against the per-plan ConcurrentTailsPerInstance cap), and
--    decremented on each terminal (completed / failed / timeout).
--    The reaper's ReapIdle / ReapAggressive / SelectEvictions and the
--    engine's snapshotAndPark all read this column: a non-zero
--    tail_count blocks the StateRunning → StateSnapshotting transition
--    (the queue-drain precedent from PR #136 / #154 — atomic supersede
--    + durable build queue). The row stays StateRunning throughout;
--    tail_count is metadata, NOT a state machine value.
--
--    NOT NULL + DEFAULT 0 keeps the column cheap to read in the reaper
--    hot path (no NULL coalescing, no constraint check) and lets the
--    engine's TailCount > 0 guard simplify to a plain truthiness
--    check. ADDs are metadata-only on Postgres >= 11 (no default
--    rewrite because the constant 0 is not volatile), so this is a
--    fast operation on a busy instances table.
--
--    No CHECK constraint here: the runner-side cap enforcement
--    (TailCapMax = 16 structural cap, ConcurrentTailsPerInstance
--    per-plan cap) is the single source of truth for the upper
--    bound. A DB CHECK would be a duplicate gate that the runner
--    already enforces before any BumpInstanceTailCount call.
alter table instances
  add column if not exists tail_count integer not null default 0;

-- 2. usage_minutes.tail_seconds — cumulative wall-clock seconds the
--    instance spent draining tail tasks during this minute.
--    Additive merge via AppendUsage (mirrors the cpu_usec / tx_bytes
--    shape from migrations 00055_usage_minutes_cpu.sql and
--    00067_extend_metering_telemetry.sql). Sampler writes via the new
--    tailSeconds parameter on AppendUsage; rollup propagates to
--    usage_daily on the next RollupOnce tick.
--
--    IMPORTANT (ADR-078 §"Tail is informational"): tail_seconds does
--    NOT enter Math.GBHours, Provider.PushUsageRecord, providerOpsFor,
--    or any Stripe/Paddle payload shape. The billable math is
--    unchanged. A permanent guard test
--    pkg/meter/pusher_shadow_test.go::TestPushHour_ExcludesTailSeconds
--    pins this — a follow-up ADR would have to remove it.
alter table usage_minutes
  add column if not exists tail_seconds bigint not null default 0;

-- Mirror on the rollup table usage_daily so the daily panel can show
-- tail latency without joining back to usage_minutes. The rollup
-- OVERWRITES tail_seconds on conflict (col = EXCLUDED.col) — the
-- day-grain is a point-in-time snapshot of the cumulative
-- SUM(tail_seconds) over the day window, so re-aggregation on the
-- next cron tick converges to the same value. Additive merge would
-- multiply by the tick count (~288× at 5-min cadence). Pinned by
-- pkg/meter/rollup_test.go::TestRollupSQL_OverwriteSemantics.
-- See pkg/meter/rollup.go::RollupOnce / RollupLoop for the wiring.
alter table usage_daily
  add column if not exists tail_seconds bigint not null default 0;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
-- Reverse-order teardown. Down-migrations on shipped platforms
-- require a manual runbook per the 00010/00013 posture, so this
-- Down section is the clean reverse; the loud-fail trust is the
-- operator's.
alter table usage_daily    drop column if exists tail_seconds;
alter table usage_minutes  drop column if exists tail_seconds;
alter table instances      drop column if exists tail_count;
-- +goose StatementEnd
