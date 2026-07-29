-- +goose Up
-- +goose StatementBegin

-- filename: 00068_builder_usage.sql
--
-- Per-build usage grain (ADR-048 §4).
--
-- usage_minutes is keyed by (instance_id, minute) and cannot host
-- per-build rows without either (a) overloading the same PK with a
-- non-instance row or (b) collapsing the idempotency story for
-- webhook redelivery. This migration adds a dedicated grain table:
-- one row per terminal build (succeeded OR failed — the box burned
-- cycles either way), PK build_id, append-only.
--
-- The meterd rollup cron (PR-A commit A.5) sums builder_seconds
-- into usage_daily.builder_seconds per (account_id, app_id, day).
-- The (account_id, app_id, finished_at) index supports the cron
-- window scan without a heap scan.
--
-- builder_kind mirrors builds.kind so the dashboard can break down
-- "your builds cost the box N seconds of 2-vCPU/2 GB time today" by
-- railpack vs dockerfile. 'none' is reserved for rows that pre-date
-- the build-time column add (zero; the rollup excludes them via the
-- CASE WHEN builder_kind <> 'none' guard in the cron SQL).
--
-- ADR-048: informational only. NOT pushed to billing providers.

CREATE TABLE IF NOT EXISTS public.builder_usage (
    build_id     uuid PRIMARY KEY,
    account_id   uuid NOT NULL,
    app_id       uuid NOT NULL,
    finished_at  timestamptz NOT NULL,
    kind         text NOT NULL DEFAULT 'none',
    seconds      bigint NOT NULL DEFAULT 0
);

CREATE INDEX IF NOT EXISTS builder_usage_account_finished_idx
  ON public.builder_usage (account_id, finished_at DESC);

COMMENT ON TABLE public.builder_usage IS 'Per-build wall-clock seconds, one row per terminal build. Source: cmd/builderd reaper + markSucceeded/markFailed adapters. ADR-048. Informational only — not billed.';
COMMENT ON COLUMN public.builder_usage.kind IS 'build kind (railpack|dockerfile|tarball). Mirrors builds.kind. ADR-048.';

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS builder_usage_account_finished_idx;
DROP TABLE IF EXISTS public.builder_usage;
-- +goose StatementEnd