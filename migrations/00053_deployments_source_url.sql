-- +goose Up
-- +goose StatementBegin
-- filename: 00053_deployments_source_url.sql
--
-- 00053_deployments_source_url.sql — Tier 3 (issue #197 B3.10 schema half).
--
-- Slot 53 was free on main at PR-#322 landing time; we renumbered from
-- 51 to 53 to avoid colliding with migrations/00051_crons_app_full_idx.sql
-- (PR #340), which also wanted 51. See PR #345 (CI slot-collision fix).
--
-- Adds two columns to deployments:
--
--   source_url — the canonical upstream URL the build was triggered
--                from. For githubd-triggered deploys this is the repo +
--                branch ("https://github.com/acme/app@main"); for
--                registry pulls it is the OCI ref; for tarball /
--                dockerfile deploys it is "" (SourcePath is the spool
--                path on disk; no upstream URL). Optional today; the
--                populator lands in Phase 2 (B3.10 read half).
--
--   commit_sha — the upstream commit SHA the build was triggered
--                from, when known. githubd already passes commitSHA to
--                the CreateDeployment callback
--                (pkg/githubd/service.go:35); Phase 2 reads it and
--                stamps this column. Length-bounded (7..64 hex chars;
--                sha1 fits at 40, sha256 at 64) and regex-anchored to
--                lowercase hex so a pathological 1 MB string OR a
--                64-character string of 'g' is rejected at the DB
--                layer rather than blowing up the row.
--
-- Both columns are nullable. Pre-existing rows from before this
-- migration are unaffected (the "no upstream URL" case is the common
-- one for image: deploys). ADR-038 (Phase 2) names the producer; the
-- reader is /v1/builds/{id}/provenance.
--
-- Idempotent (ADD COLUMN IF NOT EXISTS + DO-block-guarded
-- ADD CONSTRAINT, mirroring 00047_crons_created_at.sql's
-- convention) so re-running this migration on a DB where the
-- columns and constraint already exist but the goose row was
-- lost is a no-op. The CD job on 2026-07-27 hit the
-- non-idempotent shape on the production box: schema present,
-- version row missing — the bare ADD COLUMN tripped SQLSTATE
-- 42701. The shape here closes the recurrence vector; the
-- companion second-MigrateUp assertion in
-- 00053_deployments_source_url_test.go pins the contract at
-- PR time.
--
-- Down: drops both columns. No data is lost in steady state because
-- both fields are recomputable from the deployment trigger (re-run
-- the githubd createDeployment callback or re-derive source_url from
-- the app config).

ALTER TABLE deployments
    ADD COLUMN IF NOT EXISTS source_url text,
    ADD COLUMN IF NOT EXISTS commit_sha text;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_catalog.pg_constraint
        WHERE conname = 'deployments_commit_sha_shape_chk'
          AND conrelid = 'deployments'::regclass
    ) THEN
        ALTER TABLE deployments
            ADD CONSTRAINT deployments_commit_sha_shape_chk
                CHECK (commit_sha IS NULL
                       OR (char_length(commit_sha) BETWEEN 7 AND 64
                           AND commit_sha ~ '^[0-9a-f]+$'));
    END IF;
END$$;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE deployments
    DROP CONSTRAINT IF EXISTS deployments_commit_sha_shape_chk;
ALTER TABLE deployments
    DROP COLUMN IF EXISTS commit_sha;
ALTER TABLE deployments
    DROP COLUMN IF EXISTS source_url;
-- +goose StatementEnd
