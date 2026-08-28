-- filename: 00494_deployments_canary_step_started_at_not_null.sql
-- +goose Up
-- +goose StatementBegin

-- Issue #976 / ADR-122 / SAFE-RELEASES code-review hardening —
-- backfill NULL canary_step_started_at to COALESCE(created_at, NOW()),
-- lock the column as NOT NULL, and stamp a NOW() default so future
-- INSERT paths that omit the column still get a meaningful (non-
-- zero) timestamp. Migration 00480 left the column nullable on the
-- rationale "NULL = pending / pre-step-1", but the canary_progression
-- runtime (pkg/canary.Once at line 226) and the safedeploy
-- orchestrator (pkg/safedeploy.Orchestrator at line 207) BOTH
-- treat a zero-value timestamp as 'no wall-clock gate'. A NULL
-- column collapsed through the meterd canaryPtrTime adapter
-- (cmd/meterd/canary_progression_ticks.go:191) → time.Time{} →
-- silent soak-bypass on the first tick. The hardening has three
-- layers, each independent so a bug in any one is caught by the
-- next:
--
--   1. COALESCE backfill: existing NULL rows get a meaningful
--      timestamp (created_at when available, NOW() otherwise).
--   2. NOT NULL constraint: PG refuses any future INSERT that
--      leaves the column NULL.
--   3. NOW() default: an INSERT that omits the column lands with
--      NOW() — a non-zero timestamp that the wall-clock gate
--      respects (elapsed = 0 < stage.Duration → safe-skip rather
--      than immediate-advance).
--
-- The apid CreateDeployment handler (cmd/apid/handlers_sidecars.go)
-- still stamps CanaryStepStartedAt = now() explicitly so the
-- production timestamp matches the row's created_at to within
-- microseconds. The DEFAULT only fires for write paths that
-- bypass the handler (pgtest INSERTs, manual SQL, etc.), and
-- those paths are the ones most likely to forget the stamp —
-- so the DEFAULT is the belt-and-braces that keeps the
-- wall-clock gate honest.
--
-- Replay safety: the UPDATE is idempotent on a non-NULL row (the
-- predicate WHERE canary_step_started_at IS NULL excludes it).
-- ALTER COLUMN SET NOT NULL is idempotent (PG accepts a no-op
-- re-application; SET DEFAULT now() is also idempotent).

UPDATE deployments
   SET canary_step_started_at = COALESCE(created_at, NOW())
 WHERE canary_step_started_at IS NULL;

ALTER TABLE deployments
    ALTER COLUMN canary_step_started_at SET DEFAULT NOW();

ALTER TABLE deployments
    ALTER COLUMN canary_step_started_at SET NOT NULL;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

-- DROP NOT NULL + DROP DEFAULT to restore the pre-PR nullable
-- shape. The UPDATE is not reversed (restoring NULL is a no-op
-- semantic — the column was already nullable before, so the
-- backfill values we wrote are valid pre-PR zeros). If a future
-- migration needs to also wipe the backfill, add it here with
-- a WHERE created_at = canary_step_started_at filter so it only
-- touches rows we filled in.
ALTER TABLE deployments
    ALTER COLUMN canary_step_started_at DROP NOT NULL;
ALTER TABLE deployments
    ALTER COLUMN canary_step_started_at DROP DEFAULT;

-- +goose StatementEnd
