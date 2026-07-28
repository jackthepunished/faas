-- +goose Up
-- +goose StatementBegin
--
-- 00060_invocations_dead_letter.sql — issue #394 (queue introspection).
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
-- The drain (pkg/sched/drain.go) consults plan.LimitsForPlan(acct.Plan).MaxQueueAttempts
-- and chooses between two update paths on a transient failure:
--   attempts + 1 < budget → state='pending', attempts = attempts + 1
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

ALTER TABLE public.invocations
    DROP CONSTRAINT invocations_state_check;

ALTER TABLE public.invocations
    ADD CONSTRAINT invocations_state_check
    CHECK (state IN ('pending','dispatching','completed','failed','cancelled','dead_letter'));

CREATE INDEX invocations_app_dead_letter_idx
    ON public.invocations (app_id, created_at DESC)
    WHERE state = 'dead_letter';

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS invocations_app_dead_letter_idx;

ALTER TABLE public.invocations
    DROP CONSTRAINT invocations_state_check;

ALTER TABLE public.invocations
    ADD CONSTRAINT invocations_state_check
    CHECK (state IN ('pending','dispatching','completed','failed','cancelled'));

-- existing dead_letter rows would now violate the restored CHECK; this
-- is intentional rollback semantics — the down direction also forces
-- the operator to manually drain them before rolling back. A re-run
-- of `goose up` is the recovery path.
-- +goose StatementEnd
