-- filename: 00082_apps_scaling_policy.sql
-- +goose Up
-- Add per-app scaling_policy jsonb column + cooldown bookkeeping
-- columns to `apps` (issue #462 / ADR-058). Customers get a
-- customer-configurable scaling policy layered on top of the
-- existing per-app knobs (`min_instances`, `max_concurrency`,
-- `autoscale_target_rps`, `autoscale_target_cpu_pct`).
--
-- Three columns:
--   scaling_policy     jsonb NOT NULL DEFAULT '{}'::jsonb
--                       — the canonical wire shape:
--                         {min_instances: int,
--                          max_instances: int,
--                          target: {metric: "concurrent_requests"|"rps"|"p99_latency_ms",
--                                   value: number},
--                          scale_out_cooldown_s: int,
--                          scale_in_cooldown_s: int}
--                         Validation lives in apid (CreateDeploymentOverrides
--                         style) — the DB accepts any jsonb shape and trusts
--                         the application layer.
--   last_scale_out_at  timestamptz (nullable)
--                       — schedd stamps this on the wake-gate admit branch
--                         (used by the cooldown_held outcome). Null on legacy
--                         rows until schedd first admits.
--   last_scale_in_at   timestamptz (nullable)
--                       — schedd stamps this on the reaper park branch
--                         (used by the cooldown_held outcome on scale-in).
--
-- Replay-safe (PR #377 / ADR-041): ADD COLUMN IF NOT EXISTS. The jsonb
-- default is the constant '{}' — no row rewrite, no index bloat, and
-- a second MigrateUp is a no-op.
--
-- The CHECK constraints on last_scale_*_at use the column itself in
-- the predicate, which is replay-safe (the constraint is added once
-- and Postgres does not re-validate existing NULL rows against the
-- NULL < now() bound).
--
-- No backfill of scaling_policy from the legacy columns: apid's
-- appResponse assembles the policy from min_instances /
-- max_concurrency at read time when scaling_policy is the empty
-- default '{}'. The first PATCH that touches any of the new fields
-- writes the jsonb, after which the column is the source of truth.
-- +goose StatementBegin
ALTER TABLE apps
    ADD COLUMN IF NOT EXISTS scaling_policy jsonb NOT NULL DEFAULT '{}'::jsonb;
ALTER TABLE apps
    ADD COLUMN IF NOT EXISTS last_scale_out_at timestamptz;
ALTER TABLE apps
    ADD COLUMN IF NOT EXISTS last_scale_in_at timestamptz;
-- Replay-safe CHECK constraints via DO-block guards. `ADD CONSTRAINT
-- IF NOT EXISTS` is NOT supported by Postgres — Postgres has `IF NOT
-- EXISTS` for ADD COLUMN but not for ADD CONSTRAINT. The DO-block
-- pattern matches the codebase's existing convention (see
-- migrations/00074_projects_and_workloads.sql:86-104, the
-- apps_workload_class_chk precedent). A second MigrateUp against a
-- schema already holding the constraints is a clean no-op.
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_catalog.pg_constraint
        WHERE conname = 'apps_last_scale_out_at_le_now_chk'
          AND conrelid = 'apps'::regclass
    ) THEN
        ALTER TABLE apps
            ADD CONSTRAINT apps_last_scale_out_at_le_now_chk
            CHECK (last_scale_out_at IS NULL OR last_scale_out_at <= now());
    END IF;
    IF NOT EXISTS (
        SELECT 1 FROM pg_catalog.pg_constraint
        WHERE conname = 'apps_last_scale_in_at_le_now_chk'
          AND conrelid = 'apps'::regclass
    ) THEN
        ALTER TABLE apps
            ADD CONSTRAINT apps_last_scale_in_at_le_now_chk
            CHECK (last_scale_in_at IS NULL OR last_scale_in_at <= now());
    END IF;
END$$;
-- +goose StatementEnd

-- +goose Down
-- Reverse: drop the CHECKs, then the three columns. A row that had
-- a non-default scaling_policy loses the customer-authored policy
-- on downgrade; apid's appResponse falls back to the empty-policy
-- projection from min_instances / max_concurrency, which is the
-- correct degraded behaviour (matches the pre-PR contract). DROP
-- CONSTRAINT IF EXISTS is supported by Postgres (the IF EXISTS form
-- is valid on DROP even though it isn't on ADD).
-- +goose StatementBegin
ALTER TABLE apps
    DROP CONSTRAINT IF EXISTS apps_last_scale_out_at_le_now_chk;
ALTER TABLE apps
    DROP CONSTRAINT IF EXISTS apps_last_scale_in_at_le_now_chk;
ALTER TABLE apps
    DROP COLUMN IF EXISTS last_scale_in_at;
ALTER TABLE apps
    DROP COLUMN IF EXISTS last_scale_out_at;
ALTER TABLE apps
    DROP COLUMN IF EXISTS scaling_policy;
-- +goose StatementEnd
