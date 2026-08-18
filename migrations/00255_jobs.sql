-- filename: 00255_jobs.sql
-- +goose Up
-- +goose StatementBegin

-- ADR-099 jobs (run-to-completion workloads) — three tables:
--
--   public.jobs       — one row per customer-defined job definition
--                       (the named Cloud Run-style workload). Lives
--                       independently of any run; deleting a job
--                       cascades to its runs + tasks per ADR-099
--                       §Decision 1.
--   public.job_runs   — one row per execution attempt (manual,
--                       scheduled, or triggered). Carries the
--                       runtime inputs (env overrides, parallelism,
--                       timeout, retry policy) and the aggregate
--                       status that the run-level fan-in recomputes
--                       on every task transition (ADR-099 §Decision 2).
--   public.job_tasks  — one row per scheduled unit of work
--                       (task_index, 0..tasks-1). Carries the
--                       individual lifecycle (queued, claimed,
--                       succeeded, failed, timeout, cancelled, oom)
--                       and the instance_id once schedd dispatches
--                       it (PR-C). The job_tasks_ready_idx partial
--                       index is the dispatch-tick hot path
--                       (ADR-099 §Decision 6).
--
-- Default-OFF per ADR-099 §Decision 9: no production code path
-- writes or reads these tables until PR-B (sqlc) wires the
-- CRUD and PR-C (schedd) wires the dispatch tick. PR-A's
-- only runtime effect is the table DDL.
--
-- Slot reservation: this slot is in the range 00255-00263, fenced
-- by the ADR-099 jobs PR-cluster (issue #879 / ADR-099). Sibling
-- fences at 00259-00263 hold PR-B/C/D/E/F headroom. Mirrors the
-- cross-PR fence pattern from PR-0391 carve-out +
-- PR #867 (cross-pr-slot-fence-pagination-gate).
--
-- The cluster initially targeted 00245-00253 but renumbered
-- after PR #864 (edge-rules budget) bumped to 00245 first,
-- making 00255 the next free slot. The fence at 00244
-- co-occupies with #887's real 00244 edge-rules throttle
-- (both no-op SELECT 1; the loser's renumber follows
-- cross-pr-slot-gate-reservation-fence-pattern).
--
-- Replay safety: every CREATE uses IF NOT EXISTS, every index
-- uses IF NOT EXISTS, and the down-migration uses DROP TABLE IF
-- EXISTS. The replay-safety harness
-- (TestNewMigrationsAreReplaySafe in
-- migrations/replay_safety_test.go) applies the migration twice
-- in a single tx and pins the second pass as a no-op.

----------------------------------------------------------------------
-- public.jobs — job definition (ADR-099 §Decision 1)
----------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS jobs (
    id              uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    account_id      uuid        NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    -- 'app' / 'function' — mirrors apps.type; lets a single
    -- account own both kinds without two tables. The CHECK
    -- below is the closed vocabulary.
    kind            text        NOT NULL,
    -- Job name (slug, customer-facing). Unique per account.
    -- The dashboard + CLI render by name; the wire layer
    -- accepts name OR id (PR-D).
    name            text        NOT NULL,
    -- image_ref is the digest-pinned OCI reference the job
    -- executes, same shape as deployments.image_digest.
    -- ADR-099 §Decision 4: the build pipeline (builderd)
    -- produces this; jobs reference the same OCI layout as
    -- apps. Reusing the existing imaged auth / digest-pinning
    -- machinery (open question #3 in the imp plan).
    image_ref       text        NOT NULL,
    -- Plan RAM allocation in MB. Mirrors apps.ram_mb; the
    -- same vmmd cgroup scope formula applies
    -- (memory.max = ram_mb + 8 MB per spec §11).
    ram_mb          int         NOT NULL,
    -- task_timeout_s is the per-task watchdog. Distinct
    -- from the spec §6.1 5s `snapshot_and_park` idle
    -- watchdog — jobs use a per-task deadline (PR-C
    -- guest/init/job_supervisor.go).
    task_timeout_s  int         NOT NULL,
    -- max_parallelism caps the in-flight task count per
    -- run (NOT per account — see plan open question #2;
    -- the per-account RAM ceiling is the natural brake).
    max_parallelism int         NOT NULL,
    -- retry_max = 0 disables retry. CHECK bounds prevent
    -- a misconfigured job from retrying forever (the
    -- run-level fan-in caps dead-letter at retry_max+1
    -- attempts).
    retry_max       int         NOT NULL,
    -- env_overrides is the per-job env override map (the
    -- customer-facing knob for "this run should use
    -- DATABASE_URL=..."). jsonb because the key/value set
    -- is open-vocabulary and the dashboard renders it
    -- verbatim.
    env_overrides   jsonb       NOT NULL DEFAULT '{}'::jsonb,
    -- status: 'active' (new runs accepted) | 'paused'
    -- (existing runs continue, new runs rejected at
    -- POST /v1/jobs/{name}/runs) | 'deleted' (soft
    -- tombstone; the schedule tick skips deleted jobs).
    -- Hard delete is the rare operator path; the soft
    -- tombstone lets the operator console render "job
    -- deleted 3 days ago" without losing the run history.
    status          text        NOT NULL DEFAULT 'active',
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT jobs_kind_check
        CHECK (kind IN ('app','function')),
    CONSTRAINT jobs_name_check
        -- Mirror apps.slug shape: 3..40 chars, [a-z0-9-],
        -- no leading/trailing dash. Unique per account.
        CHECK (name ~ '^[a-z0-9]([a-z0-9-]{1,38})[a-z0-9]$'),
    CONSTRAINT jobs_ram_mb_check
        CHECK (ram_mb > 0),
    CONSTRAINT jobs_task_timeout_s_check
        CHECK (task_timeout_s BETWEEN 1 AND 86400),
    CONSTRAINT jobs_max_parallelism_check
        CHECK (max_parallelism BETWEEN 1 AND 1000),
    CONSTRAINT jobs_retry_max_check
        CHECK (retry_max BETWEEN 0 AND 10),
    CONSTRAINT jobs_status_check
        CHECK (status IN ('active','paused','deleted'))
);

-- Per-account job list (the dashboard's primary index).
-- Mirrors apps_account_idx.
CREATE INDEX IF NOT EXISTS jobs_account_idx
    ON jobs (account_id, created_at DESC);

-- Per-account job-by-name lookup (POST /v1/jobs/{name}/runs).
-- The wire layer accepts name OR id; the name path uses
-- this UNIQUE.
CREATE UNIQUE INDEX IF NOT EXISTS jobs_account_name_uniq
    ON jobs (account_id, name) WHERE status <> 'deleted';

----------------------------------------------------------------------
-- public.job_runs — execution attempt (ADR-099 §Decision 1+2)
----------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS job_runs (
    id              uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    -- FK to jobs; ON DELETE CASCADE matches the ADR-099
    -- §Decision 1 "deleting a job cascades to its runs"
    -- contract.
    job_id          uuid        NOT NULL REFERENCES jobs(id) ON DELETE CASCADE,
    account_id      uuid        NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    -- 'manual' / 'scheduled' / 'triggered' (webhook). The
    -- distinction drives the dashboard tab + the runbook
    -- triggers (PR-F).
    trigger_kind    text        NOT NULL,
    -- Per-run env overrides (the customer-facing knob for
    -- "this run should use DATABASE_URL=..."). Merged on
    -- top of jobs.env_overrides at task-dispatch time
    -- (PR-C). jsonb shape mirrors jobs.env_overrides.
    env_overrides   jsonb       NOT NULL DEFAULT '{}'::jsonb,
    -- tasks is the total task count for this run
    -- (0 < tasks <= 100000, mirroring the plan open
    -- question #4 which caps individual job sizes). The
    -- fan-in aggregator (PR-B) walks job_tasks to derive
    -- succeeded/failed/cancelled counts; the per-run
    -- total lives here for fast dashboard rendering.
    tasks           int         NOT NULL,
    -- parallelism = jobs.max_parallelism at run start; can
    -- be reduced mid-run (PR-C's update endpoint) but not
    -- increased (would re-introduce capacity-planning
    -- surprises).
    parallelism     int         NOT NULL,
    -- retry_max override: if non-NULL, overrides
    -- jobs.retry_max for this run only (lets the customer
    -- "retry_max=0 this time" without editing the job).
    retry_max       int             NULL,
    -- task_timeout_s override, same shape.
    task_timeout_s  int             NULL,
    -- aggregate_status: 'queued' / 'running' / 'succeeded'
    -- / 'failed' / 'cancelled' / 'dead_letter'.
    -- Recomputed by PR-B's fan-in aggregator on every
    -- task transition. The dispatch tick (PR-C) reads
    -- the per-status counters to decide when to issue the
    -- task-claimed-batch SELECT FOR UPDATE SKIP LOCKED.
    aggregate_status text       NOT NULL DEFAULT 'queued',
    -- task counters (denormalised for dashboard fast
    -- path). PR-B's RecomputeRunStatus updates all four
    -- atomically in a single transaction per transition.
    tasks_succeeded int         NOT NULL DEFAULT 0,
    tasks_failed    int         NOT NULL DEFAULT 0,
    tasks_cancelled int         NOT NULL DEFAULT 0,
    tasks_running   int         NOT NULL DEFAULT 0,
    -- started_at / finished_at NULL until the run
    -- transitions away from 'queued'.
    started_at      timestamptz     NULL,
    finished_at     timestamptz     NULL,
    created_at      timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT job_runs_trigger_kind_check
        CHECK (trigger_kind IN ('manual','scheduled','triggered')),
    CONSTRAINT job_runs_tasks_check
        CHECK (tasks BETWEEN 1 AND 100000),
    CONSTRAINT job_runs_parallelism_check
        CHECK (parallelism BETWEEN 1 AND 1000),
    CONSTRAINT job_runs_retry_max_check
        CHECK (retry_max IS NULL
            OR retry_max BETWEEN 0 AND 10),
    CONSTRAINT job_runs_task_timeout_s_check
        CHECK (task_timeout_s IS NULL
            OR task_timeout_s BETWEEN 1 AND 86400),
    CONSTRAINT job_runs_aggregate_status_check
        CHECK (aggregate_status IN (
            'queued','running','succeeded',
            'failed','cancelled','dead_letter'
        )),
    CONSTRAINT job_runs_counters_check
        CHECK (tasks_succeeded >= 0 AND tasks_failed >= 0
            AND tasks_cancelled >= 0 AND tasks_running >= 0
            AND tasks_succeeded + tasks_failed + tasks_cancelled
                + tasks_running <= tasks),
    CONSTRAINT job_runs_terminal_pair_chk
        CHECK ((finished_at IS NULL AND aggregate_status IN ('queued','running'))
            OR (finished_at IS NOT NULL AND aggregate_status IN
                ('succeeded','failed','cancelled','dead_letter')))
);

-- Per-account run list (the dashboard's primary index).
-- Mirrors apps_account_idx (account_id, created_at DESC).
CREATE INDEX IF NOT EXISTS job_runs_account_idx
    ON job_runs (account_id, created_at DESC);

-- Per-job run list (the job-detail page).
CREATE INDEX IF NOT EXISTS job_runs_job_idx
    ON job_runs (job_id, created_at DESC);

-- Dispatch-tick filter path: "which runs are still active
-- and need a task batch?" — covered by aggregate_status
-- IN ('queued','running') + a per-account index so the
-- schedd LISTEN worker can scan locally without locking
-- the world. Mirrors the instances_reaper_state_idx
-- precedent.
CREATE INDEX IF NOT EXISTS job_runs_active_idx
    ON job_runs (account_id, id)
    WHERE aggregate_status IN ('queued','running');

----------------------------------------------------------------------
-- public.job_tasks — task lifecycle (ADR-099 §Decision 1+6)
----------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS job_tasks (
    -- Composite PK keeps the per-run task uniqueness and
    -- makes the dispatch-tick SELECT FOR UPDATE SKIP LOCKED
    -- batch claim (PR-B's ClaimTasks) index-only. Mirrors
    -- the instances PK pattern (id-only) but tasks are
    -- addressed by (run_id, task_index), not a fresh uuid.
    run_id          uuid        NOT NULL REFERENCES job_runs(id) ON DELETE CASCADE,
    task_index      int         NOT NULL,
    -- status: 'queued' / 'claimed' / 'succeeded' /
    -- 'failed' / 'timeout' / 'cancelled' / 'oom'. The
    -- dispatch tick transitions queued→claimed; the
    -- terminal states are set by PR-B's MarkTask*
    -- helpers on the post-exit DGRAM path.
    status          text        NOT NULL DEFAULT 'queued',
    -- attempt = 1..retry_max+1; attempt 1 is the first
    -- dispatch, attempt N+1 is the Nth retry. CHECK
    -- bounds prevent runaway retries even if a buggy
    -- caller forgets the per-run cap.
    attempt         int         NOT NULL DEFAULT 1,
    -- instance_id is set when the dispatch tick claims
    -- the task (PR-C). NULL while status='queued' or
    -- 'claimed' but before vmmd reports ready.
    instance_id     uuid            NULL REFERENCES instances(id),
    -- Per-task error class on terminal failure. Closed
    -- vocabulary mirroring the existing
    -- data_upstream_probes.error_class.
    error_class     text            NULL,
    error_message   text            NULL,
    -- started_at / finished_at NULL until the task
    -- transitions away from 'queued'.
    started_at      timestamptz     NULL,
    finished_at     timestamptz     NULL,
    created_at      timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (run_id, task_index),
    CONSTRAINT job_tasks_task_index_check
        CHECK (task_index >= 0),
    CONSTRAINT job_tasks_status_check
        CHECK (status IN (
            'queued','claimed',
            'succeeded','failed',
            'timeout','cancelled','oom'
        )),
    CONSTRAINT job_tasks_attempt_check
        CHECK (attempt BETWEEN 1 AND 11),
    CONSTRAINT job_tasks_error_class_check
        CHECK (error_class IS NULL
            OR error_class IN ('timeout','refused','tls_handshake','dns','unreachable','oom','user_error','infra')),
    CONSTRAINT job_tasks_terminal_pair_chk
        CHECK ((finished_at IS NULL AND status IN ('queued','claimed'))
            OR (finished_at IS NOT NULL AND status IN
                ('succeeded','failed','timeout','cancelled','oom'))),
    CONSTRAINT job_tasks_instance_pair_chk
        -- An instance_id can only exist when the task is
        -- mid-flight (claimed) or terminal. Queued
        -- tasks haven't been dispatched yet.
        CHECK ((instance_id IS NULL AND status = 'queued')
            OR (instance_id IS NOT NULL))
);

-- Dispatch-tick hot path (ADR-099 §Decision 6). PR-B's
-- ClaimTasks does:
--
--   SELECT run_id, task_index FROM job_tasks
--    WHERE status = 'queued'
--    ORDER BY created_at ASC
--    LIMIT $1
--    FOR UPDATE SKIP LOCKED
--
-- The partial index covers both the predicate and the
-- ORDER BY (the `created_at` column is on the table but
-- not in the index — the optimizer will sort). To keep
-- the index hot and small, partial on status='queued'.
CREATE INDEX IF NOT EXISTS job_tasks_ready_idx
    ON job_tasks (created_at ASC, run_id, task_index)
    WHERE status = 'queued';

-- Per-run tasks list (the run-detail page). Mirrors
-- instances_app_idx.
CREATE INDEX IF NOT EXISTS job_tasks_run_idx
    ON job_tasks (run_id, task_index);

-- pg_notify hot-path: per-run task insert/update fires
-- `job_tasks_queued` (PR-B's fan-in) so the schedd
-- LISTEN worker wakes within the 1s tick budget.
-- Drop-before-create per the trigger-replay-safety
-- precedent (memory: trigger-replay-safety-drop-before-create).
DROP TRIGGER IF EXISTS job_tasks_notify_trg ON job_tasks;

CREATE OR REPLACE FUNCTION job_tasks_notify() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    -- Only notify on transitions INTO queued (initial
    -- INSERT) or from queued (claim by the dispatch
    -- tick). Other transitions don't need to wake the
    -- LISTEN worker because they're responses to the
    -- worker's own batch.
    IF (TG_OP = 'INSERT' AND NEW.status = 'queued')
       OR (TG_OP = 'UPDATE' AND OLD.status = 'queued'
           AND NEW.status <> 'queued') THEN
        PERFORM pg_notify(
            'job_tasks_queued',
            format('%s|%s|%s', NEW.run_id, NEW.task_index, TG_OP)
        );
    END IF;
    IF TG_OP = 'DELETE' THEN
        RETURN OLD;
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER job_tasks_notify_trg
    AFTER INSERT OR UPDATE OR DELETE ON job_tasks
    FOR EACH ROW
    EXECUTE FUNCTION job_tasks_notify();

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP TRIGGER IF EXISTS job_tasks_notify_trg ON job_tasks;
DROP FUNCTION IF EXISTS job_tasks_notify();
DROP TABLE IF EXISTS job_tasks;
DROP TABLE IF EXISTS job_runs;
DROP TABLE IF EXISTS jobs;

-- +goose StatementEnd
