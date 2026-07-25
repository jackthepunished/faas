-- +goose Up
-- +goose StatementBegin
-- Issue #169 + #172: per-app autoscale target columns. Reactive scale-up
-- trigger (pkg/sched/scaleup) reads these on every tick; when measured
-- load crosses the target, schedd admits another instance up to
-- plan.MaxConcurrency. Both columns are nullable — autoscale is
-- "enabled" iff at least one is non-NULL (per user direction, no
-- separate boolean). Plan-tier gating is enforced by apid, not the DB
-- (Free tier is forbidden from setting either column).
ALTER TABLE apps
    ADD COLUMN autoscale_target_rps    integer,
    ADD COLUMN autoscale_target_cpu_pct integer;

ALTER TABLE apps
    ADD CONSTRAINT apps_autoscale_target_rps_nonneg
        CHECK (autoscale_target_rps IS NULL OR autoscale_target_rps >= 0),
    ADD CONSTRAINT apps_autoscale_target_cpu_pct_range
        CHECK (autoscale_target_cpu_pct IS NULL
            OR (autoscale_target_cpu_pct >= 0 AND autoscale_target_cpu_pct <= 100));

COMMENT ON COLUMN apps.autoscale_target_rps IS
    'Per-instance RPS target. When live_request_count / live_instance_count exceeds this, schedd admits another instance (up to plan max_concurrency). Hobby/Pro/Scale only (plan gate). 0 / NULL = disabled (the trigger skips the app).';
COMMENT ON COLUMN apps.autoscale_target_cpu_pct IS
    'Per-instance CPU% target (1..100). Pro/Scale only (plan gate). 0 / NULL = disabled (the trigger skips the app). CPU target is unbounded above 100 inside the DB; the apid handler enforces [1, 100] via 422.';
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE apps
    DROP CONSTRAINT IF EXISTS apps_autoscale_target_cpu_pct_range,
    DROP CONSTRAINT IF EXISTS apps_autoscale_target_rps_nonneg,
    DROP COLUMN IF EXISTS autoscale_target_cpu_pct,
    DROP COLUMN IF EXISTS autoscale_target_rps;
-- +goose StatementEnd
