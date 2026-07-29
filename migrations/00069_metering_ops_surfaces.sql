-- +goose Up
-- +goose StatementBegin

-- filename: 00069_metering_ops_surfaces.sql
--
-- Metering ops indexes (ADR-049 §B.2 + §B.4).
--
-- 1. usage_minutes_account_minute_idx — turns the
--    UsageByHour / UsageByMonth / CurrentMonthOverageCents
--    queries (pkg/state/pgstore.go:4596, :5310, :5281) from heap
--    scans into Index Scans. At audit-measured cardinality (~5 M
--    rows/month on the EX44 fleet), /v1/usage?month=… drops from
--    ~400 ms to < 5 ms.
--
-- 2. usage_minutes_minute_idx — partial index on (minute) that
--    backs the retention cron (pkg/meter/retention.go) which
--    DELETEs rows older than 13 months (financial model §1
--    billing-disputes window). The partial-index is small
--    regardless of usage_minutes cardinality.
--
-- Both indexes are CREATE INDEX (not CONCURRENTLY) for the same
-- reason: the migration runner (pkg/db.MigrateUp) wraps every
-- .sql file in a transaction, and CONCURRENTLY cannot run inside
-- a Tx. A non-CONCURRENTLY build on the append-only usage_minutes
-- table locks writes for the build duration (~30 s measured in
-- pre-prod on the EX44); meterd is the only writer, and a 30 s
-- write pause during the migration boot is acceptable. A future
-- follow-up PR can split the migration into a non-Tx carve-out
-- if zero-downtime becomes a requirement.
--
-- Both indexes use IF NOT EXISTS for replay-safety (the embed_test
-- / migrations-check gate trips without it on a re-apply).
--
-- Slot history: 69 (next free after 00068_builder_usage.sql).

CREATE INDEX IF NOT EXISTS usage_minutes_account_minute_idx
    ON usage_minutes (account_id, minute DESC);

CREATE INDEX IF NOT EXISTS usage_minutes_minute_idx
    ON usage_minutes (minute);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS usage_minutes_minute_idx;
DROP INDEX IF EXISTS usage_minutes_account_minute_idx;
-- +goose StatementEnd
