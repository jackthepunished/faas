-- filename: 00435_request_telemetry_monthly_partitions.sql
-- +goose Up
-- +goose StatementBegin

-- ADR-127 §PR-B — explicit monthly partitions for request_telemetry.
--
-- PR-A's 00427 ships only the default partition (catches anything
-- outside explicit partitions). PR-B creates explicit partitions
-- for current month, next month, and month-after-next so the
-- steady-state has 3 months of pre-allocated partitions. The cron
-- in cmd/apid/debug_regression_cron.go::ensureMonthAfterNextPartition
-- then rotates them forward on every 5-minute pass — a 60-day
-- gatewayd-internal outage cannot push rows into the default
-- partition because the cron continuously extends the rolling
-- window.
--
-- Why this matters:
--   * Without explicit partitions, RetentionOnceRequestTelemetry
--     (pkg/meter/retention.go:154-201) would have to ctid-IN-SELECT
--     across millions of rows in the default partition to drop them
--     (the partition-attached DROP TABLE is one DDL op, free at
--     scan time).
--   * PR-A's PR-B readme explicitly deferred partition management
--     here: "default partition would balloon unbounded and the
--     sweep would have to ctid-IN-SELECT across millions of rows
--     instead of dropping a partition" (plan §"Rejected
--     alternatives", bullet (c)).
--
-- Partition naming: request_telemetry_YYYYMM (4-digit year + 2-digit
-- month). Mirrors the TimescaleDB / pg_partman convention without
-- the dependency. The cron uses the same format so a sweep can
-- match by prefix when deciding what to drop.
--
-- Replay-safe posture: each CREATE TABLE uses IF NOT EXISTS. A
-- replay that runs in the same month applies no-op CREATE TABLE
-- statements (the partitions already exist). A replay that runs
-- across a month boundary creates a new partition without clobbering
-- the existing one. The DO block is one transaction so the three
-- partitions land atomically or not at all.

DO $$
DECLARE
    now_ts              timestamptz := now();
    cur_start           timestamptz := date_trunc('month', now_ts);
    nxt_start           timestamptz := date_trunc('month', now_ts) + interval '1 month';
    after_nxt_start     timestamptz := date_trunc('month', now_ts) + interval '2 month';
    cur_end             timestamptz := nxt_start;
    nxt_end             timestamptz := after_nxt_start;
    after_nxt_end       timestamptz := date_trunc('month', now_ts) + interval '3 month';
    cur_name            text := 'request_telemetry_' || to_char(cur_start, 'YYYYMM');
    nxt_name            text := 'request_telemetry_' || to_char(nxt_start, 'YYYYMM');
    after_nxt_name      text := 'request_telemetry_' || to_char(after_nxt_start, 'YYYYMM');
BEGIN
    EXECUTE format(
        'CREATE TABLE IF NOT EXISTS %I PARTITION OF request_telemetry FOR VALUES FROM (%L) TO (%L)',
        cur_name, cur_start, cur_end);
    EXECUTE format(
        'CREATE TABLE IF NOT EXISTS %I PARTITION OF request_telemetry FOR VALUES FROM (%L) TO (%L)',
        nxt_name, nxt_start, nxt_end);
    EXECUTE format(
        'CREATE TABLE IF NOT EXISTS %I PARTITION OF request_telemetry FOR VALUES FROM (%L) TO (%L)',
        after_nxt_name, after_nxt_start, after_nxt_end);
END $$;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
-- Forward-only: rolling back would orphan rows in the dropped
-- partitions (the default partition still catches new INSERTs,
-- but historical data would be lost). Down is a sentinel so a
-- replay lands on the DO block, not on a destructive DROP.
SELECT 1;
-- +goose StatementEnd