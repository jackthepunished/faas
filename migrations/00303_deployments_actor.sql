-- +goose Up
-- +goose StatementBegin
-- filename: 00303_deployments_actor.sql
--
-- issue #606 — Persist the deployer identity on `public.deployments`.
-- PR #984 (issue #977 / ADR-116) added the human-readable `deployed_by`
-- text column (auto-captured from git config / github pusher / action
-- actor). This migration adds the four structured columns PR #984
-- deliberately did not cover (the two are orthogonal: PR #984 stamps
-- a name for human-readability, this migration stamps the machine-
-- readable attribution needed for SOC 2 CC7.2 / GDPR):
--
--   deployed_by_user_id  — UUID FK to accounts.id. Nullable
--                          because (a) anonymous / unauthenticated
--                          CLI deploys predate the FK relationship
--                          (PR-D cluster, issue #879 PR-D), and
--                          (b) GitHub-push deploys are not
--                          attributable to a local Gregale account.
--                          The dashboard renders the resolved name
--                          via JOIN; the column stores the FK. The
--                          Gregale data model is account-centric
--                          (no users table — see 00001_init.sql),
--                          so the FK points at accounts(id) not
--                          a non-existent users(id).
--
--   deployed_via         — TEXT NOT NULL with a closed-set CHECK
--                          ('api' | 'cli' | 'dashboard' | 'github'
--                          | 'operator'). Defaults to 'api' so
--                          pre-feature rows stay valid without a
--                          backfill. The CHECK mirrors the
--                          deployments_parked_reason pattern
--                          (migration 00157) and uses the
--                          DROP+ADD widening shape for replay
--                          safety on a DB that lost the goose
--                          version row but kept the column.
--
--   deployed_from_ip     — INET, nullable. Stamped from
--                          r.RemoteAddr via the same loopback+XFF
--                          trust contract the auth-limit bucket
--                          uses (pkg/middleware.ClientIP). The
--                          column is observability data, not a
--                          security gate — the trust contract is
--                          documented in the apid handler that
--                          stamps it (PR-E1.2).
--
--   pusher_login         — TEXT, nullable. Distinct from
--                          `deployed_by` (PR #984): `deployed_by`
--                          carries the resolved human-readable name,
--                          `pusher_login` carries the raw GitHub
--                          login string (e.g. `poyrazK`) so the
--                          audit reader can disambiguate a renamed
--                          / deleted GitHub user from a stale
--                          `deployed_by` label. Both can be NULL.
--
-- All four columns are added nullable (or, in `deployed_via`'s case,
-- NOT NULL with a server-side default of 'api'). Pre-feature
-- deployments stay valid:
--
--   deployed_by_user_id  NULL  (anonymous / pre-FK / GitHub-push)
--   deployed_via         'api' (default)
--   deployed_from_ip     NULL
--   pusher_login         NULL
--
-- No backfill is needed (PR #984's deployed_by backfill handles the
-- human-readable case; the structured columns are forward-only).
-- The `deployed_via` CHECK uses the DROP+ADD widening shape so the
-- migration is replay-safe on a DB that already has the column but
-- lost the goose version row (the CD-job-2026-07-27 SQLSTATE 42701
-- tripwire from 00053's docstring).

ALTER TABLE deployments
    ADD COLUMN IF NOT EXISTS deployed_by_user_id uuid,
    ADD COLUMN IF NOT EXISTS deployed_via        text NOT NULL DEFAULT 'api',
    ADD COLUMN IF NOT EXISTS deployed_from_ip    inet,
    ADD COLUMN IF NOT EXISTS pusher_login        text;

ALTER TABLE deployments
    DROP CONSTRAINT IF EXISTS deployments_deployed_via_set_chk;
ALTER TABLE deployments
    ADD  CONSTRAINT deployments_deployed_via_set_chk
        CHECK (deployed_via IN
               ('api', 'cli', 'dashboard', 'github', 'operator'));

-- FK to accounts(id). ON DELETE SET NULL — if an account is GDPR-
-- erased, the deployment row must keep its reason / tag /
-- created_at, but the attribution column nulls out (matches the
-- audit row's subject=NULL convention on account deletion,
-- audit.go:43 + issue #286). The FK is added via ALTER TABLE …
-- ADD CONSTRAINT in a separate statement so the constraint can
-- be dropped+readded in the same replay-safe shape as the CHECK.
ALTER TABLE deployments
    DROP CONSTRAINT IF EXISTS deployments_deployed_by_user_id_fk;
ALTER TABLE deployments
    ADD  CONSTRAINT deployments_deployed_by_user_id_fk
        FOREIGN KEY (deployed_by_user_id) REFERENCES accounts(id)
        ON DELETE SET NULL;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE deployments DROP CONSTRAINT IF EXISTS deployments_deployed_by_user_id_fk;
ALTER TABLE deployments DROP CONSTRAINT IF EXISTS deployments_deployed_via_set_chk;
ALTER TABLE deployments DROP COLUMN IF EXISTS pusher_login;
ALTER TABLE deployments DROP COLUMN IF EXISTS deployed_from_ip;
ALTER TABLE deployments DROP COLUMN IF EXISTS deployed_via;
ALTER TABLE deployments DROP COLUMN IF EXISTS deployed_by_user_id;
-- +goose StatementEnd
