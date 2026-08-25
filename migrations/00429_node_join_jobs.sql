-- filename: 00429_node_join_jobs.sql
-- +goose Up
-- +goose StatementBegin
--
-- Durable provider-neutral compute-node adoption state. A join is an
-- operator workflow, not a single SSH command: it can be interrupted after
-- staging or release installation and must resume without two operators
-- converging the same node concurrently.
CREATE TABLE IF NOT EXISTS node_join_jobs (
    id                uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    node_name         text NOT NULL UNIQUE,
    database_node     text NOT NULL,
    ssh_host          text NOT NULL,
    manifest_hash     text NOT NULL,
    release_git_sha   text NOT NULL,
    phase             text NOT NULL DEFAULT 'planned'
        CHECK (phase IN ('planned', 'preflight', 'converging', 'verifying', 'active', 'failed', 'rolled_back')),
    attempt           integer NOT NULL DEFAULT 0 CHECK (attempt >= 0),
    last_error        text,
    lease_owner       text,
    lease_expires_at  timestamptz,
    created_at        timestamptz NOT NULL DEFAULT now(),
    updated_at        timestamptz NOT NULL DEFAULT now(),
    completed_at      timestamptz
);

CREATE INDEX IF NOT EXISTS node_join_jobs_phase_idx
    ON node_join_jobs (phase, updated_at DESC);
CREATE INDEX IF NOT EXISTS node_join_jobs_lease_idx
    ON node_join_jobs (lease_expires_at)
    WHERE lease_owner IS NOT NULL;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS node_join_jobs_lease_idx;
DROP INDEX IF EXISTS node_join_jobs_phase_idx;
DROP TABLE IF EXISTS node_join_jobs;
-- +goose StatementEnd
