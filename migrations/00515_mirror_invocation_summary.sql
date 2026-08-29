-- filename: 00515_mirror_invocation_summary.sql
-- +goose Up
-- +goose StatementBegin
--
-- Hour-grain rollup of mirror_invocation_results (issue #72 /
-- ADR-124 / ADR-125 PR-A3 commit 4). The raw ledger is the
-- per-invocation audit trail; the dashboard chip
-- (gateway_mirror_dispatched_total + the §12 mirror drift-rate
-- panel) wants hour-grain counts over the rule lifetime without
-- scanning the raw table on every read.
--
-- Key shape: (rule_id, hour_bucket) PRIMARY KEY. Same hour bucket
-- is additive-merged on re-run: re-running the rollup on a
-- partially-collected hour UPSERTs the running counts. Differs
-- from usage_daily's overwrite merge (meter.RollupOnce) because
-- the underlying ledger is append-only; the count is a running
-- sum, not a window total.
--
-- The A2 endpoint GET /v1/apps/{slug}/mirrors/{id}/summary
-- already exists; this table is its back-end. The e2e suite
-- (cmd/e2e/traffic_mirror_e2e_test.go) pins that the summary
-- returns non-zero counts after RollupOnce has run.
--
-- Index: (app_id, hour_bucket DESC) so per-app time-series
-- scans avoid a sequential read on the rule_id-keyed primary.
CREATE TABLE IF NOT EXISTS mirror_invocation_summary (
    rule_id              uuid        NOT NULL,
    app_id               uuid        NOT NULL,
    hour_bucket          timestamptz NOT NULL,
    total_invocations    bigint      NOT NULL DEFAULT 0,
    status_diff_count    bigint      NOT NULL DEFAULT 0,
    schema_diff_count    bigint      NOT NULL DEFAULT 0,
    body_diff_count      bigint      NOT NULL DEFAULT 0,
    crash_count          bigint      NOT NULL DEFAULT 0,
    cap_at_max_count     bigint      NOT NULL DEFAULT 0,
    sum_latency_ms       bigint      NOT NULL DEFAULT 0,
    rolled_up_at         timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (rule_id, hour_bucket)
);

CREATE INDEX IF NOT EXISTS mirror_invocation_summary_app_idx
    ON mirror_invocation_summary (app_id, hour_bucket DESC);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS mirror_invocation_summary_app_idx;
DROP TABLE IF EXISTS mirror_invocation_summary;
-- +goose StatementEnd
