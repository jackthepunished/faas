-- +goose Up
-- +goose StatementBegin
-- filename: 00169_invocations_outcome.sql
--
-- 00169_invocations_outcome.sql — issue #791 (cron run history).
--
-- Adds a durable, normalized `outcome` to the invocations table so the
-- per-cron run-history surface (GET /v1/crons/{id}/runs) can render
-- "timeout" as distinct from a generic failure.
--
-- Why a column and not a derivation:
--   `state` conflates every terminal failure into 'failed'. A gateway
--   504 (the app exceeded its deadline) and a malformed-payload reject
--   are indistinguishable downstream — both land as state='failed' with
--   free-text last_error. Substring-matching last_error to recover the
--   distinction is brittle: the text is operator-facing, unversioned,
--   and already varies by call site ("wake: ...", "invoke: ...").
--   Recording the classification at write time, where the caller
--   already knows it, is authoritative.
--
-- Value domain:
--   'success'      — terminal, state='completed'
--   'failed'       — terminal, permanent error (4xx, shape, capacity)
--   'timeout'      — terminal, the dispatch exceeded its deadline
--                    (gateway 504 / lease expiry)
--   'dead_letter'  — terminal, per-plan retry budget exhausted
--   NULL           — non-terminal (pending / dispatching). The API
--                    renders NULL as "running"; the column stays
--                    nullable so the write path is purely additive and
--                    a transient re-queue does not have to clear it.
--
-- Backfill maps existing terminal rows off `state`. Historical rows
-- cannot be re-classified as 'timeout' — that information was never
-- recorded — so they land on 'failed'. This is intentional and lossy
-- in the safe direction: no historical row is claimed to be a timeout
-- when we cannot prove it was.
--
-- Index:
--   invocations_cron_idx backs the new read path — per-cron rows,
--   newest first — matching the ORDER BY (created_at DESC, id DESC)
--   in pkg/state/pgstore.go::ListCronRunsForCron. Partial on
--   cron_id IS NOT NULL because only source='cron' rows carry it;
--   the vast majority of the table (async_invoke/queue) is excluded,
--   keeping the index small.
--
-- Replay-safety: search_path-relative identifiers (no `public.`
-- prefix — pgtest isolates each test in its own schema). The CHECK
-- swap is wrapped in a DO-block guard and CREATE INDEX uses
-- IF NOT EXISTS, so re-running against an already-migrated DB is a
-- no-op. Mirrors the convention established by
-- 00064_invocations_dead_letter.sql.

ALTER TABLE invocations ADD COLUMN IF NOT EXISTS outcome text;

DO $$
BEGIN
    IF EXISTS (
        SELECT 1 FROM pg_catalog.pg_constraint
        WHERE conname = 'invocations_outcome_check'
          AND conrelid = 'invocations'::regclass
    ) THEN
        ALTER TABLE invocations DROP CONSTRAINT invocations_outcome_check;
    END IF;

    ALTER TABLE invocations
        ADD CONSTRAINT invocations_outcome_check
        CHECK (outcome IS NULL OR outcome IN ('success','failed','timeout','dead_letter'));
END$$;

-- Backfill terminal rows from state. Non-terminal rows (pending,
-- dispatching) are left NULL by the WHERE clause. Idempotent: the
-- `outcome IS NULL` guard means a replay does not rewrite rows the
-- live write path has since classified.
UPDATE invocations
   SET outcome = CASE state
                     WHEN 'completed'   THEN 'success'
                     WHEN 'dead_letter' THEN 'dead_letter'
                     ELSE 'failed'
                 END
 WHERE outcome IS NULL
   AND state IN ('completed','failed','cancelled','dead_letter');

CREATE INDEX IF NOT EXISTS invocations_cron_idx
    ON invocations (cron_id, created_at DESC)
    WHERE cron_id IS NOT NULL;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS invocations_cron_idx;

DO $$
BEGIN
    IF EXISTS (
        SELECT 1 FROM pg_catalog.pg_constraint
        WHERE conname = 'invocations_outcome_check'
          AND conrelid = 'invocations'::regclass
    ) THEN
        ALTER TABLE invocations DROP CONSTRAINT invocations_outcome_check;
    END IF;
END$$;

ALTER TABLE invocations DROP COLUMN IF EXISTS outcome;
-- +goose StatementEnd
