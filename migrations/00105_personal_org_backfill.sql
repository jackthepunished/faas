-- +goose Up
-- Personal-org backfill (issue #190 / IAM-6 / ADR-061, PR 3).
-- For every account created before PR 3, create the matching
-- personal org + owner membership atomically. Idempotent: the
-- partial unique `orgs_one_personal_per_account_uniq` (added in
-- migration 00099) is the SQL-level tripwire for any concurrent
-- caller, and ON CONFLICT DO NOTHING provides replay-safety for
-- re-running the backfill (ADR-041).
--
-- The slug is the deterministic uuid-v5 derivation
-- (PersonalOrgSlug in pkg/state/types.go) — same shape as the
-- CreateAccountWithPersonalOrg helper, so a freshly created
-- account's personal org has the same slug as a backfilled
-- account's personal org would. Matches the check
-- `orgs_slug_shape` (`^[a-z0-9][a-z0-9-]{1,30}[a-z0-9]$`):
-- "u-" + 12 hex chars = 14 chars total, well inside the 3..32
-- window.
--
-- Slot 101 — companion reservation file 00101_reserve_slot.sql is
-- git-rm'd post-merge per ADR-041.
-- +goose StatementBegin

-- 1. Personal orgs for accounts that don't have one yet. The
--    LEFT JOIN against orgs (filtered to personal_org=true) is the
--    replay-safe shape: a re-run finds no rows where o.id IS NULL.
--    ON CONFLICT DO NOTHING handles the partial-unique tripwire
--    (a future direct-SQL caller could violate the LEFT JOIN
--    invariant; the partial unique is the floor).
INSERT INTO orgs (
    id, slug, name, personal_org, personal_owner_account_id,
    plan, status, created_at, updated_at
)
SELECT
    gen_random_uuid(),
    'u-' || substring(replace(a.id::text, '-', '') from 1 for 12),
    'Personal',
    true,
    a.id,
    a.plan,
    a.status,
    a.created_at,
    now()
FROM accounts a
LEFT JOIN orgs o
    ON o.personal_owner_account_id = a.id AND o.personal_org = true
WHERE o.id IS NULL
ON CONFLICT DO NOTHING;

-- 2. Owner memberships for accounts that have a personal org but
--    no owner membership yet. Same LEFT-JOIN idempotency shape.
INSERT INTO org_memberships (org_id, account_id, role, invited_by_account_id)
SELECT
    o.id,
    a.id,
    'owner',
    NULL
FROM accounts a
JOIN orgs o
    ON o.personal_owner_account_id = a.id AND o.personal_org = true
LEFT JOIN org_memberships m
    ON m.org_id = o.id AND m.account_id = a.id
WHERE m.org_id IS NULL
ON CONFLICT DO NOTHING;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
-- Reverse the backfill: drop every personal org (the only orgs
-- created by this migration) and their owner memberships. The
-- downstream tables (orgs, org_memberships) themselves are NOT
-- dropped — that is migration 00099's responsibility.
DELETE FROM org_memberships
 WHERE org_id IN (SELECT id FROM orgs WHERE personal_org = true);
DELETE FROM orgs WHERE personal_org = true;
-- +goose StatementEnd
