-- +goose Up
-- +goose StatementBegin

-- filename: 00070_snapshot_storage_daily.sql
--
-- snapshot_storage_daily — per-(account, app, day) byte totals from
-- snapshots.mem_bytes + snapshots.disk_bytes + overlay staging
-- (ADR-049 §B.3). Backs the dashboard's storage rollup surface and
-- a future "Pro plan gets 1 GB included / overage €X/GB-month"
-- line item.
--
-- This PR is informational only; the existing billing math (plan
-- RAM + 8 MB per running second) is unchanged. The table exists
-- so the future billing PR has the data to price overage against.
--
-- Shape mirrors usage_daily (migration 00067): PK (account_id,
-- app_id, day), additive-merge ON CONFLICT, indexed by
-- (account_id, day DESC) for the cron window scan.
--
-- Schema-scoping: identifiers are search_path-relative (no
-- `public.` prefix) per the convention documented at
-- migrations/00064_invocations_dead_letter.sql:39-49. Production
-- search_path=public puts the table there; pgtest search_path=
-- faas_test_<hex> puts the table in the isolated test schema —
-- preventing the 40P01 deadlock on pg_class when N parallel test
-- packages each try CREATE TABLE public.snapshot_storage_daily
-- against the same cluster (issue surfaced on CI run 30645758787
-- TestPg_ClaimCliAuthCode_BindsAccountID alongside migration
-- 00068). Companion test updated to query through
-- current_schema(). Original convention pinned by PR #394.
--
-- Slot history: 70 (next free after 00069_metering_ops_surfaces.sql).

CREATE TABLE IF NOT EXISTS snapshot_storage_daily (
    account_id      uuid    NOT NULL,
    app_id          uuid    NOT NULL,
    day             date    NOT NULL,
    snapshot_bytes  bigint  NOT NULL DEFAULT 0,
    layer_bytes     bigint  NOT NULL DEFAULT 0,
    computed_at     timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (account_id, app_id, day)
);

CREATE INDEX IF NOT EXISTS snapshot_storage_daily_account_day_idx
    ON snapshot_storage_daily (account_id, day DESC);

COMMENT ON TABLE snapshot_storage_daily IS
    'Per-(account, app, day) byte totals from snapshots.mem_bytes + disk_bytes + overlay staging. Source: pkg/meter/storage.go cron tick. ADR-049 §B.3. Informational only — not billed today; the future "Pro plan 1 GB included" PR consumes this surface.';

COMMENT ON COLUMN snapshot_storage_daily.snapshot_bytes IS
    'Σ snapshots.mem_bytes + snapshots.disk_bytes (latest non-stale row per app per day). ADR-049 §B.3. Informational.';

COMMENT ON COLUMN snapshot_storage_daily.layer_bytes IS
    'Σ overlay staging bytes per app per day. ADR-049 §B.3. Informational.';

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS snapshot_storage_daily_account_day_idx;
DROP TABLE IF EXISTS snapshot_storage_daily;
-- +goose StatementEnd
