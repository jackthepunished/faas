-- filename: 00101_apps_warm_snapshot.sql
-- +goose Up
-- Issue #470 / ADR-055 — two-tier snapshot (init.snap + warm.snap) —
-- per-app opt-in. Customers on Pro/Scale get the warm tier on by default
-- (the doubled parked footprint is inside the 452 GB budget for opt-in
-- plans; Free/Hobby stay init-only).
--
-- Three columns added to apps:
--
--   warm_snapshot_enabled        boolean NOT NULL DEFAULT false
--   warm_snapshot_min_requests   int     NOT NULL DEFAULT 5
--                                  CHECK (warm_snapshot_min_requests BETWEEN 1 AND 100)
--   warm_snapshot_min_ms         int     NOT NULL DEFAULT 2000
--                                  CHECK (warm_snapshot_min_ms BETWEEN 100 AND 60000)
--
-- The per-plan default (Free/Hobby false; Pro/Scale true) is applied at
-- app create time by apid via pkg/api/limits.go::Plan.WarmSnapshotDefault
-- (PR #377 / ADR-041 mirroring). The column default of false here is the
-- "schema-only" floor; the plan default is the operator-facing policy.
--
-- No partial index on warm_snapshot_enabled=true. Every per-request wake
-- path already loads the App row by id (apps_account_idx, apps slug
-- lookup), so a partial index would not shrink any hot read. The
-- operator dashboard query "which apps have warm-tier on?" is rare
-- enough to full-scan.
--
-- Replay-safe (ADR-041): ADD COLUMN IF NOT EXISTS + CHECK + DEFAULT
-- constant — second MigrateUp is a no-op.
-- +goose StatementBegin
ALTER TABLE apps
    ADD COLUMN IF NOT EXISTS warm_snapshot_enabled boolean NOT NULL DEFAULT false;
ALTER TABLE apps
    ADD COLUMN IF NOT EXISTS warm_snapshot_min_requests int NOT NULL DEFAULT 5;
ALTER TABLE apps
    ADD COLUMN IF NOT EXISTS warm_snapshot_min_ms int NOT NULL DEFAULT 2000;
-- Replay-safe CHECK constraints via DO-block guards. `ADD CONSTRAINT
-- IF NOT EXISTS` is NOT supported by Postgres — Postgres has `IF NOT
-- EXISTS` for ADD COLUMN but not for ADD CONSTRAINT. The DO-block
-- pattern matches the codebase's existing convention (see
-- migrations/00082_apps_scaling_policy.sql:50-77 and
-- migrations/00074_projects_and_workloads.sql:86-104). A second
-- MigrateUp against a schema already holding the constraints is a
-- clean no-op (PR #377 / ADR-041).
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_catalog.pg_constraint
        WHERE conname = 'apps_warm_snapshot_min_requests_check'
          AND conrelid = 'apps'::regclass
    ) THEN
        ALTER TABLE apps
            ADD CONSTRAINT apps_warm_snapshot_min_requests_check
            CHECK (warm_snapshot_min_requests BETWEEN 1 AND 100);
    END IF;
    IF NOT EXISTS (
        SELECT 1 FROM pg_catalog.pg_constraint
        WHERE conname = 'apps_warm_snapshot_min_ms_check'
          AND conrelid = 'apps'::regclass
    ) THEN
        ALTER TABLE apps
            ADD CONSTRAINT apps_warm_snapshot_min_ms_check
            CHECK (warm_snapshot_min_ms BETWEEN 100 AND 60000);
    END IF;
END$$;
-- +goose StatementEnd

-- +goose Down
-- Reverse: drop the CHECKs and the columns. A row that had
-- warm_snapshot_enabled=true loses the bit on downgrade; the GET
-- /v1/apps/{slug} response shape omits the fields because the columns
-- no longer exist, which is the correct degraded behaviour (same
-- pattern as migrations/00080_apps_streaming_enabled.sql::Down).
-- +goose StatementBegin
ALTER TABLE apps
    DROP CONSTRAINT IF EXISTS apps_warm_snapshot_min_ms_check;
ALTER TABLE apps
    DROP COLUMN IF EXISTS warm_snapshot_min_ms;
ALTER TABLE apps
    DROP CONSTRAINT IF EXISTS apps_warm_snapshot_min_requests_check;
ALTER TABLE apps
    DROP COLUMN IF EXISTS warm_snapshot_min_requests;
ALTER TABLE apps
    DROP COLUMN IF EXISTS warm_snapshot_enabled;
-- +goose StatementEnd