-- +goose Up
-- +goose StatementBegin
-- filename: 00159_invocations_replay_source.sql
--
-- 00159_invocations_replay_source.sql — issue #315 / tier-2 DX.
--
-- Adds the new invocation source value 'replay' to the
-- invocations_source_check CHECK constraint. POST
-- /v1/invocations/{id}/replay stamps this on the freshly enqueued
-- row so the dashboard + CLI can tell at a glance that the row
-- was re-issued (vs an original async_invoke / queue / cron /
-- delayed_task). The replay path is a follow-on async invoke
-- against the original's app/instance, so the row enters the
-- normal pending → dispatching → completed/failed lifecycle —
-- only the Source column distinguishes it.
--
-- Source CHECK expansion:
--   before: async_invoke, queue, delayed_task, cron
--   after:  async_invoke, queue, delayed_task, cron, replay
--
-- No new index: replays are rare relative to async_invoke / cron
-- (the customer hits the button manually), and the dashboard's
-- per-app filter on Source is a low-cardinality scan that doesn't
-- need a dedicated index. If abuse surfaces (a script replaying
-- every failed invocation in a loop), the observability table
-- already covers it via Source='replay' + Attempts>1.
--
-- Replay-safety: DO-block guard + ALTER … DROP/ADD mirrors the
-- pattern established by 00064_invocations_dead_letter.sql and
-- 00156_apps_auth_default_flip.sql. search_path-relative
-- identifiers throughout (no `public.` prefix — pgtest isolates
-- each test in its own schema and hard-coded `public.` fails
-- 42P01). Idempotent against a DB that already has 'replay' in
-- the CHECK.
--
-- Companion test: 00159_invocations_replay_source_test.go pins
-- the constraint shape (insert succeeds with replay, fails with
-- bogus).

DO $$
BEGIN
    IF EXISTS (
        SELECT 1 FROM pg_catalog.pg_constraint
        WHERE conname = 'invocations_source_check'
          AND conrelid = 'invocations'::regclass
    ) THEN
        ALTER TABLE invocations DROP CONSTRAINT invocations_source_check;
    END IF;

    ALTER TABLE invocations
        ADD CONSTRAINT invocations_source_check
        CHECK (source IN ('async_invoke','queue','delayed_task','cron','replay'));
END$$;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (
        SELECT 1 FROM pg_catalog.pg_constraint
        WHERE conname = 'invocations_source_check'
          AND conrelid = 'invocations'::regclass
    ) THEN
        ALTER TABLE invocations DROP CONSTRAINT invocations_source_check;
    END IF;

    ALTER TABLE invocations
        ADD CONSTRAINT invocations_source_check
        CHECK (source IN ('async_invoke','queue','delayed_task','cron'));
END$$;
-- existing 'replay' rows would now violate the restored CHECK; this
-- is intentional rollback semantics — the operator must drain them
-- before rolling back. A re-run of `goose up` is the recovery path.
-- +goose StatementEnd