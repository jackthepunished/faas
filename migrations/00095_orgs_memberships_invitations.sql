-- filename: 00095_orgs_memberships_invitations.sql
-- +goose Up
-- Organizations, memberships, and invitations (issue #190 / IAM-6 / ADR-061).
-- This is the **expansion** phase of the staged rollout: it adds the new
-- tables and the nullable `org_id` columns on every tenant-root table, but
-- no code reads them yet (the column defaults to NULL; pre-PR-3 rows stay
-- NULL until the backfill lands). PR 3 stamps every account's personal
-- org + owner membership and begins the dual-write; PR 5 adds the handlers.
--
-- Slot 95 is the next free slot on `origin/main` (slot 86 landed cosign
-- trusted-publishers per PR #504). The companion reservation file
-- `00096_reserve_slot.sql` is removed post-merge per ADR-041.
--
-- Companion docs:
--   - docs/adr/061-organizations-and-memberships.md (decision)
--   - docs/iam-6-ownership-inventory.md (per-table classification)
--   - docs/faas_implementation_spec.md §4.2 (tenancy), §17 G14 (rollout)

-- Section 1: orgs (tenant root + personal-org flag).
--
-- `personal_org` distinguishes the immutable per-account personal org from
-- a shared non-personal org; it gates PATCH /orgs/{slug}/members and the
-- org-deletion lifecycle (personal orgs cannot accept members, transfer
-- ownership, or be deleted independently of the account).
--
-- `plan` mirrors `accounts.plan` initially (PR 7 keeps them mirrored; PR 9
-- stops mirroring). `status` follows the existing AccountStatus vocabulary
-- so meterd / dunning can pivot onto orgs with no enum fork.
--
-- `provider_customer_id` and `stripe_subscription_item` move here from
-- `accounts.*`; until PR 7 the columns stay NULL on every org row and the
-- Stripe/Paddle code paths continue to read the account columns.
--
-- `deleted_pending` powers the org lifecycle in PR 8 (the
-- deleted_pending → hard-delete flow is owner-only; personal orgs skip it
-- because personal deletion rides the account deletion flow).
--
-- Slug regex mirrors `pkg/api/errors.go::OrgSlugPattern` so the DB
-- constraint and the handler validator cannot drift. The pattern is
-- `^[a-z0-9][a-z0-9-]{1,30}[a-z0-9]$` (3..32 chars, no leading/trailing
-- dash). It is restated inline below because SQL CHECKs cannot import
-- Go constants.
-- +goose StatementBegin
CREATE TABLE IF NOT EXISTS orgs (
    id                          uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    slug                        text        NOT NULL,
    name                        text        NOT NULL,
    personal_org                boolean     NOT NULL DEFAULT false,
    personal_owner_account_id   uuid        REFERENCES accounts(id) ON DELETE CASCADE,
    plan                        text        NOT NULL DEFAULT 'free',
    status                      text        NOT NULL DEFAULT 'active',
    provider_customer_id        text,
    stripe_subscription_item    text,
    deleted_pending             boolean     NOT NULL DEFAULT false,
    created_at                  timestamptz NOT NULL DEFAULT now(),
    updated_at                  timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT orgs_slug_shape
        CHECK (slug ~ '^[a-z0-9][a-z0-9-]{1,30}[a-z0-9]$'),
    CONSTRAINT orgs_personal_owner_link
        -- personal_org rows must point at exactly one account; non-personal
        -- rows must not. The partial unique below pins "exactly one".
        CHECK ((personal_org = true  AND personal_owner_account_id IS NOT NULL) OR
               (personal_org = false AND personal_owner_account_id IS NULL)),
    CONSTRAINT orgs_plan_chk
        CHECK (plan IN ('free','hobby','pro','scale')),
    CONSTRAINT orgs_status_chk
        CHECK (status IN ('active','past_due','suspended','deleted_pending'))
);
-- Case-insensitive slug uniqueness (mirrors projects.slug unique key from
-- slot 00074). Global because org slugs are public routing identifiers
-- under /v1/orgs/{slug} per ADR-061 §"Wire / authorization contract".
CREATE UNIQUE INDEX IF NOT EXISTS orgs_slug_uniq
    ON orgs (lower(slug));
-- Exactly-one-personal-org-per-account partial unique. Postgres treats
-- NULL as distinct under a plain UNIQUE, so multiple non-personal orgs
-- may have personal_owner_account_id = NULL without conflict; the
-- partial-index predicate `WHERE personal_org = true` collapses the
-- uniqueness semantic to "at most one personal org row per account".
-- This is the SQL-level tripwire that catches a future direct-SQL
-- caller bypassing PR 3's backfill (mirrors org_memberships_one_owner_idx
-- for the exactly-one-owner invariant).
CREATE UNIQUE INDEX IF NOT EXISTS orgs_one_personal_per_account_uniq
    ON orgs (personal_owner_account_id)
    WHERE personal_org = true;
-- +goose StatementEnd

-- Section 2: org_memberships.
--
-- PK = (org_id, account_id) so one account has at most one membership in
-- a given org. ON DELETE CASCADE on both FKs so GDPR-erasing an account
-- drops its memberships and deleting an org drops every membership.
--
-- `role` CHECK pins the five values from ADR-061 §Role vocabulary. The
-- handler layer (PR 5) never lets a PATCH set role = 'owner'; that role
-- is reachable only via TransferOwnership.
--
-- `removed_at` (per docs/iam-6-ownership-inventory.md §E) lets
-- `OrgMembersMax` count active members only (`removed_at IS NULL`).
-- `org_memberships_removed_role_chk` blocks the nonsensical
-- `removed_at IS NOT NULL AND role = 'owner'` shape (a removed owner is
-- not an active owner; the exactly-one-owner partial unique below
-- would also reject the row if `removed_at IS NULL`, but the CHECK
-- pins the invariant for free).
--
-- `invited_by_account_id` is `ON DELETE SET NULL` per ADR-061 §D — actor
-- attribution must survive membership / account removal.
-- +goose StatementBegin
CREATE TABLE IF NOT EXISTS org_memberships (
    org_id                uuid        NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
    account_id            uuid        NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    role                  text        NOT NULL,
    invited_by_account_id uuid        REFERENCES accounts(id) ON DELETE SET NULL,
    joined_at             timestamptz NOT NULL DEFAULT now(),
    removed_at            timestamptz,
    PRIMARY KEY (org_id, account_id),
    CONSTRAINT org_memberships_role_chk
        CHECK (role IN ('owner','admin','developer','viewer','billing')),
    CONSTRAINT org_memberships_removed_role_chk
        CHECK (removed_at IS NULL OR role IN ('admin','developer','viewer','billing'))
);
-- Defence-in-depth exactly-one-owner invariant. The handler layer (PR 5)
-- also runs `SELECT … FROM org_memberships WHERE org_id = $1 AND role =
-- 'owner' AND removed_at IS NULL FOR UPDATE` (ADR-061 §Role vocabulary);
-- the partial unique is the SQL-level tripwire that catches a future
-- caller bypassing the store-layer lock.
CREATE UNIQUE INDEX IF NOT EXISTS org_memberships_one_owner_idx
    ON org_memberships (org_id)
    WHERE role = 'owner' AND removed_at IS NULL;
-- Hot-path lookup: "what orgs is this account a member of?" — used by
-- PR 4's `LoadOrg` and PR 5's principal context.
CREATE INDEX IF NOT EXISTS org_memberships_account_idx
    ON org_memberships (account_id)
    WHERE removed_at IS NULL;
-- +goose StatementEnd

-- Section 3: org_invitations.
--
-- Token: 32 random bytes (generated in the PR 5 handler); only the
-- SHA-256 hash is stored, matching the `login_tokens` (slot 00005) and
-- `cli_auth_codes` (slot 00014) shape so PR 5 reuses the existing
-- crypto helpers without a fork.
--
-- `role` is the four non-owner values — owner is by transfer only, never
-- by invitation, per ADR-061 §"Role vocabulary". The CHECK enforces it
-- at the SQL layer.
--
-- State CHECK pins the three valid combinations of `consumed_at` /
-- `revoked_at` (mutually exclusive). `expiry` is independent.
--
-- `invited_by_account_id` and `accepting_account_id` are `ON DELETE SET
-- NULL` per §D — the inviter / acceptor attribution survives account
-- removal.
-- +goose StatementBegin
CREATE TABLE IF NOT EXISTS org_invitations (
    id                     uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id                 uuid        NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
    email                  citext      NOT NULL,
    role                   text        NOT NULL,
    token_hash             bytea       NOT NULL,
    invited_by_account_id  uuid        REFERENCES accounts(id) ON DELETE SET NULL,
    expires_at             timestamptz NOT NULL,
    consumed_at            timestamptz,
    revoked_at             timestamptz,
    accepting_account_id   uuid        REFERENCES accounts(id) ON DELETE SET NULL,
    created_at             timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT org_invitations_token_uniq UNIQUE (token_hash),
    CONSTRAINT org_invitations_role_chk
        CHECK (role IN ('admin','developer','viewer','billing')),
    CONSTRAINT org_invitations_state_chk CHECK (
        (consumed_at IS NULL AND revoked_at IS NULL) OR
        (consumed_at IS NOT NULL AND revoked_at IS NULL) OR
        (consumed_at IS NULL AND revoked_at IS NOT NULL)
    )
);
-- Hot-path lookup: "list pending invitations for an org" — used by
-- the PR 5 GET handler and the invitation-cleanup loop modelled on
-- the existing login-token cleanup pattern. Note: the partial-index
-- predicate drops `expires_at > now()` because `now()` is not
-- IMMUTABLE (Postgres rejects non-immutable predicates in partial
-- indexes — SQLSTATE 42P17). The cleanup loop filters the time at
-- query time; the index still narrows to "un-consumed, un-revoked".
CREATE INDEX IF NOT EXISTS org_invitations_pending_idx
    ON org_invitations (org_id)
    WHERE consumed_at IS NULL AND revoked_at IS NULL;
CREATE INDEX IF NOT EXISTS org_invitations_email_idx
    ON org_invitations (org_id, email)
    WHERE consumed_at IS NULL;
-- +goose StatementEnd

-- Section 4: events.actor_account_id (audit attribution).
--
-- The existing `events` table (slot 00001) has `actor text NOT NULL` with
-- no FK and a `data jsonb` snapshot field. PR 2 adds an OPTIONAL
-- `actor_account_id uuid` reference column on `events` per
-- docs/iam-6-ownership-inventory.md §D. NULL when the actor is a service
-- principal or system identity; pre-PR-2 rows keep `actor text` unchanged.
-- `ON DELETE SET NULL` so the actor attribution survives account
-- deletion — historical events do not get reaped because the actor
-- account was GDPR-erased.
-- +goose StatementBegin
ALTER TABLE events
    ADD COLUMN IF NOT EXISTS actor_account_id uuid
        REFERENCES accounts(id) ON DELETE SET NULL;
CREATE INDEX IF NOT EXISTS events_actor_account_idx
    ON events (actor_account_id)
    WHERE actor_account_id IS NOT NULL;
-- +goose StatementEnd

-- Section 5: nullable org_id on tenant-root tables.
--
-- Section B of docs/iam-6-ownership-inventory.md lists 19 tenant-root
-- tables. Section C rows (`deployments`, `builds`, `snapshots`,
-- `deployment_logs`) deliberately get NO org_id column — they inherit
-- tenancy through app_id and LoadApp is the load-bearing IDOR check.
--
-- Every org_id column is `ON DELETE RESTRICT`: a shared org with live
-- tenant rows cannot be hard-deleted accidentally; the deletion goes
-- through the `deleted_pending` lifecycle in PR 8. RESTRICT is
-- deliberately chosen over SET NULL because orphan tenant rows would
-- break meterd's per-tenant walk in PR 7 (the walk assumes every row
-- has a tenant root).
--
-- The DO-block uses `array[…]` + `format(…, %I)` so a new tenant-root
-- table added in a future migration just needs its name appended; no
-- per-table ALTER lines.
-- +goose StatementBegin
DO $$
DECLARE
    t text;
BEGIN
    FOREACH t IN ARRAY ARRAY[
        'apps',
        'projects',
        'custom_domains',
        'api_keys',
        'instances',
        'usage_minutes',
        'usage_daily',
        'invoices',
        'stripe_push_dedupe',
        'paddle_overage_dedupe',
        'app_secrets',
        'app_envs',
        'alert_rules',
        'recent_build_claims',
        'builder_usage',
        'crons',
        'invocations',
        'github_installations',
        'gdpr_requests'
    ] LOOP
        EXECUTE format(
            'ALTER TABLE %I ADD COLUMN IF NOT EXISTS org_id uuid REFERENCES orgs(id) ON DELETE RESTRICT',
            t);
    END LOOP;
END$$;
-- +goose StatementEnd

-- Section 6: per-table partial indexes for the hot-path "what orgs own
-- this tenant row?" lookup. Partial because pre-PR-3 rows still have
-- org_id = NULL (the backfill hasn't run yet); a full index would be
-- half-empty for two releases.
-- +goose StatementBegin
CREATE INDEX IF NOT EXISTS apps_org_id_idx
    ON apps (org_id) WHERE org_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS projects_org_id_idx
    ON projects (org_id) WHERE org_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS custom_domains_org_id_idx
    ON custom_domains (org_id) WHERE org_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS api_keys_org_id_idx
    ON api_keys (org_id) WHERE org_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS instances_org_id_idx
    ON instances (org_id) WHERE org_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS usage_minutes_org_id_idx
    ON usage_minutes (org_id) WHERE org_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS usage_daily_org_id_idx
    ON usage_daily (org_id) WHERE org_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS invoices_org_id_idx
    ON invoices (org_id) WHERE org_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS stripe_push_dedupe_org_id_idx
    ON stripe_push_dedupe (org_id) WHERE org_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS paddle_overage_dedupe_org_id_idx
    ON paddle_overage_dedupe (org_id) WHERE org_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS app_secrets_org_id_idx
    ON app_secrets (org_id) WHERE org_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS app_envs_org_id_idx
    ON app_envs (org_id) WHERE org_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS alert_rules_org_id_idx
    ON alert_rules (org_id) WHERE org_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS recent_build_claims_org_id_idx
    ON recent_build_claims (org_id) WHERE org_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS builder_usage_org_id_idx
    ON builder_usage (org_id) WHERE org_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS crons_org_id_idx
    ON crons (org_id) WHERE org_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS invocations_org_id_idx
    ON invocations (org_id) WHERE org_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS github_installations_org_id_idx
    ON github_installations (org_id) WHERE org_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS gdpr_requests_org_id_idx
    ON gdpr_requests (org_id) WHERE org_id IS NOT NULL;
-- +goose StatementEnd

-- +goose Down
-- Reverse: drop partial indexes, drop the events column, drop the three
-- new tables, drop the orgs unique indexes, drop orgs. The per-table
-- ALTER TABLE … DROP COLUMN org_id path is intentionally omitted — a
-- forward-only contraction is the safe rollback per ADR-041 (slot 86's
-- down-section comment is the precedent).
-- +goose StatementBegin
DROP INDEX IF EXISTS gdpr_requests_org_id_idx;
DROP INDEX IF EXISTS github_installations_org_id_idx;
DROP INDEX IF EXISTS invocations_org_id_idx;
DROP INDEX IF EXISTS crons_org_id_idx;
DROP INDEX IF EXISTS builder_usage_org_id_idx;
DROP INDEX IF EXISTS recent_build_claims_org_id_idx;
DROP INDEX IF EXISTS alert_rules_org_id_idx;
DROP INDEX IF EXISTS app_envs_org_id_idx;
DROP INDEX IF EXISTS app_secrets_org_id_idx;
DROP INDEX IF EXISTS paddle_overage_dedupe_org_id_idx;
DROP INDEX IF EXISTS stripe_push_dedupe_org_id_idx;
DROP INDEX IF EXISTS invoices_org_id_idx;
DROP INDEX IF EXISTS usage_daily_org_id_idx;
DROP INDEX IF EXISTS usage_minutes_org_id_idx;
DROP INDEX IF EXISTS instances_org_id_idx;
DROP INDEX IF EXISTS api_keys_org_id_idx;
DROP INDEX IF EXISTS custom_domains_org_id_idx;
DROP INDEX IF EXISTS projects_org_id_idx;
DROP INDEX IF EXISTS apps_org_id_idx;
DROP INDEX IF EXISTS events_actor_account_idx;
ALTER TABLE events DROP COLUMN IF EXISTS actor_account_id;
DROP INDEX IF EXISTS org_invitations_email_idx;
DROP INDEX IF EXISTS org_invitations_pending_idx;
DROP TABLE IF EXISTS org_invitations;
DROP INDEX IF EXISTS org_memberships_account_idx;
DROP INDEX IF EXISTS org_memberships_one_owner_idx;
DROP TABLE IF EXISTS org_memberships;
DROP INDEX IF EXISTS orgs_one_personal_per_account_uniq;
ALTER TABLE orgs DROP COLUMN IF EXISTS personal_owner_account_id;
DROP INDEX IF EXISTS orgs_slug_uniq;
DROP TABLE IF EXISTS orgs;
-- +goose StatementEnd