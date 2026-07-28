-- +goose Up
-- +goose StatementBegin
-- filename: 00064_invocations_dead_letter.sql
--
-- 00064_invocations_dead_letter.sql — issue #394 (queue introspection).
--
-- Closes the consumer-side gap that today lets queue messages retry
-- indefinitely: 'attempts' was incremented on every transient failure
-- but never gated, so a poisoned payload would churn worker CPU forever.
-- This migration introduces a new terminal state 'dead_letter' that the
-- drain transitions a row to once its per-plan retry budget is spent.
-- Read-side support (GET /v1/apps/{slug}/queues/dead_letter) lands in the
-- same PR against the new partial index.
--
-- State CHECK expansion:
--   before: pending, dispatching, completed, failed, cancelled
--   after:  pending, dispatching, completed, failed, cancelled, dead_letter
--
-- The drain (pkg/sched/drain.go) consults
-- plan.LimitsForPlan(acct.Plan).MaxQueueAttempts and chooses between
-- two update paths on a transient failure:
--   attempts + 1 <  budget → state='pending', attempts = attempts + 1
--   attempts + 1 >= budget → state='dead_letter', completed_at = now()
-- budget == 0 preserves the legacy infinite-retry behavior used by the
-- delayed-task-cap and account-suspended paths (no retry budget applies).
--
-- Index:
--   The new partial index targets the QueueDeadLetter read path: per-app
--   rows with state='dead_letter', newest first. Matches the SQL shape
--   in pkg/state/pgstore.go::QueueDeadLetter and the ORDER BY created_at
--   DESC requirement of the read endpoint.
--
-- No backfill: existing rows with attempts > N keep their current state
-- and will be evaluated against the new budget on the next failure.
-- Intentional — re-classifying rows that were in flight under the old
-- "infinite retry" rule would be a quiet semantic change for in-flight
-- messages.
--
-- Replay-safety: this migration uses search_path-relative identifiers
-- (no `public.` prefix — pgtest isolates each test in its own schema,
-- so a hard-coded `public.` reference fails with SQLSTATE 42P01 when
-- the test schema is the only one created). The CHECK swap is wrapped
-- in a DO-block guard so re-running against a DB that already has
-- 'dead_letter' in its CHECK is a no-op. CREATE INDEX uses IF NOT
-- EXISTS for the same reason. The companion test file
-- (00064_invocations_dead_letter_test.go) pins the contract at PR
-- time. Mirrors the idempotency convention introduced by
-- 00053_deployments_source_url.sql (PR #322) and refined across the
-- subsequent migrations.
--
-- Slot history: the original slot was 60, but PR #399 (alert rules),
-- PR #403 (env vars) all claimed 60 in the same window. Cross-PR slot
-- gate renumbered to 62, then 64 (PR #399 also claimed 62). Issue
-- #366 captures the broader pattern.

DO $$
BEGIN
    -- Drop the existing CHECK (idempotent: IF EXISTS handles the
    -- already-replaced case where a prior replay landed).
    IF EXISTS (
        SELECT 1 FROM pg_catalog.pg_constraint
        WHERE conname = 'invocations_state_check'
          AND conrelid = 'invocations'::regclass
    ) THEN
        ALTER TABLE invocations DROP CONSTRAINT invocations_state_check;
    END IF;

    ALTER TABLE invocations
        ADD CONSTRAINT invocations_state_check
        CHECK (state IN ('pending','dispatching','completed','failed','cancelled','dead_letter'));
END$$;

CREATE INDEX IF NOT EXISTS invocations_app_dead_letter_idx
    ON invocations (app_id, created_at DESC)
    WHERE state = 'dead_letter';

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS invocations_app_dead_letter_idx;

DO $$
BEGIN
    IF EXISTS (
        SELECT 1 FROM pg_catalog.pg_constraint
        WHERE conname = 'invocations_state_check'
          AND conrelid = 'invocations'::regclass
    ) THEN
        ALTER TABLE invocations DROP CONSTRAINT invocations_state_check;
    END IF;

    ALTER TABLE invocations
        ADD CONSTRAINT invocations_state_check
        CHECK (state IN ('pending','dispatching','completed','failed','cancelled'));
END$$;

-- existing dead_letter rows would now violate the restored CHECK; this
-- is intentional rollback semantics — the down direction also forces
-- the operator to manually drain them before rolling back. A re-run
-- of `goose up` is the recovery path.
-- +goose StatementEnd
