-- filename: 00411_liveness_restart_count.sql
--
-- Issue #586 / ADR-129 / cluster C commit 12 of the
-- platform-observability mega-PR.
--
-- Persists the per-deployment liveness restart counter on the
-- deployments row. Today pkg/sched/liveness_window.go keeps the
-- counter in memory only — a schedd restart resets the window
-- and the customer loses the "this deployment has been
-- crashlooping 4 times in 10 minutes" signal that triggered the
-- last park.
--
-- The persistent column is the source of truth across schedd
-- restarts. LivenessWindow still drives the
-- `shouldPark` decision at runtime (so the in-memory path is
-- unchanged); on every restart event pkg/state/pgstore.go::
-- RecordRestart bumps the column. On schedd startup the window
-- seeds from the column so a fresh process inherits the prior
-- count instead of starting at zero.
--
-- Column:
--   liveness_restart_count INT NOT NULL DEFAULT 0
--                       — monotonically incremented by
--                         pkg/state.pgstore.RecordRestart. The
--                         "restart within the last N minutes"
--                         sliding-window check still lives in
--                         LivenessWindow (sub-minute granularity
--                         doesn't justify an events table). The
--                         persistent column complements the
--                         window — it's the count "in the
--                         lifetime of this deployment" for
--                         dashboards, not the per-window
--                         throttle signal.
--
-- Constraint:
--   deployments_liveness_restart_count_nonneg_chk
--                       — non-negative CHECK. The DEFAULT 0 + the
--                         INSERT-time constraint enforces a
--                         known-good floor; the bump path is
--                         never a decrement so the invariant
--                         holds for the lifetime of the row.
--
-- Replay-safety: every ALTER / DROP uses IF NOT EXISTS / IF
-- EXISTS guards so a partial-apply replay (lost goose row,
-- re-run MigrateUp) is idempotent. Same convention as 00264
-- (deployments_secret_findings) and 00213 (deployments_scope).
--
-- Slot reservation: 00411 chosen as next free real slot past
-- 00410 (app_secret_value_hash, PR #1065 ADR-124 prod-fix
-- cluster). The fence at 00411_reserve_slot.sql locks the
-- slot at PR-open time per cross-pr-slot gate race memory
-- entry; the actual migration lands in the same PR because no
-- sibling PR claims an adjacent slot.

-- +goose Up
-- +goose StatementBegin
ALTER TABLE deployments
    ADD COLUMN IF NOT EXISTS liveness_restart_count INT NOT NULL DEFAULT 0;

ALTER TABLE deployments
    DROP CONSTRAINT IF EXISTS deployments_liveness_restart_count_nonneg_chk;
ALTER TABLE deployments
    ADD  CONSTRAINT deployments_liveness_restart_count_nonneg_chk
        CHECK (liveness_restart_count >= 0);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
-- Mirror the Up-shape in reverse. The constraint name is
-- preserved across the round-trip so re-running the Up is
-- idempotent on the constraint (same as the canonical
-- DROP+ADD pattern in 00214 and 00264).
ALTER TABLE deployments
    DROP CONSTRAINT IF EXISTS deployments_liveness_restart_count_nonneg_chk;
ALTER TABLE deployments
    DROP COLUMN IF EXISTS liveness_restart_count;
-- +goose StatementEnd
