-- +goose Up
-- +goose StatementBegin
--
-- 00127_api_keys_org_bound.sql — IAM-6 (issue #190 / ADR-061, PR 6).
--
-- The `api_keys.org_id` column was added nullable in migration 00099
-- (alongside the partial index `api_keys_org_id_idx` ON (org_id)
-- WHERE org_id IS NOT NULL, and the FK
-- `api_keys_org_id_fkey` ON (org_id) REFERENCES orgs(id) ON DELETE
-- RESTRICT). The column was landed ahead of the Store layer wiring
-- so PR 6 could ship a single NOT NULL flip once the dual-write
-- (issue 6 endpoint surface: /v1/orgs/{slug}/keys/* + the legacy
-- /v1/keys/* mint path that stamps org_id = caller's personal org)
-- is in place.
--
-- Today's shape: every api_key row belongs to some account and
-- (since PR 3, migration 00105) every account owns a personal org,
-- so every existing row has a deterministic org_id to stamp. PR 6
-- flips the column from NULL to NOT NULL after the backfill so
-- the Store layer can stop threading the nullable-or-not question
-- through every read path.
--
-- Two-step shape:
--
--   1. UPDATE … FROM orgs backfills every row whose org_id IS NULL
--      with the matching personal org id (the partial unique
--      `orgs_one_personal_per_account_uniq` from 00099 guarantees
--      at most one personal org per account, so the join is
--      deterministic). The `WHERE k.org_id IS NULL` predicate is
--      the replay guard — a second MigrateUp is a no-op
--      (ADR-041).
--
--   2. ALTER TABLE … SET NOT NULL flips the column. Safe because
--      step 1 backfilled every row. The Store layer's new
--      CreateOrgAPIKey method always passes org_id (the
--      loadOrg middleware stamps principal.Membership.OrgID onto
--      the request), and the legacy /v1/keys POST handler now
--      routes through CreateOrgAPIKey with org_id = caller's
--      personal org (so direct INSERTs from the legacy
--      CreateAPIKey / CreateAPIKeyWithExpiry methods are no
--      longer possible after the Store layer ships its CompileTime
--      guarantees — see pkg/state/pgstore.go API key methods).
--
-- Why we do NOT drop the existing FK:
--
--   ON DELETE RESTRICT (added in 00099) is the safety property
--   PR 8's GDPR org-purge relies on. Hard-deleting a personal org
--   will fail loudly if any keys still reference it; the operator
--   must explicitly revoke the keys first. ON DELETE CASCADE would
--   silently nuke keys when an admin clicks "purge", breaking the
--   "no implicit data loss" CLAUDE.md invariant.
--
-- Why we do NOT add a partial unique on (org_id, prefix):
--
--   The api_keys table does not have a `prefix` column today —
--   only the full 32-byte `key_sha256` hash. Adding a prefix column
--   is its own migration (not PR 6 scope). Collisions on any
--   8-character plaintext prefix within one org are birthday-bound
--   at ~2^32 keys/org, well past any plausible quota (the largest
--   plan's per-account key cap is comfortably under 1K keys). Skip.
--
-- Slot 127 — chained after 00126_pg_ratelimit (the last landed on
-- origin/main). No reservation fence needed unless a sibling PR
-- claims 127 between PR-creation and merge (the cross-PR slot gate
-- will fire then and the rename is mechanical per
-- migration-slot-renumber-at-pr-creation).

-- 1. Backfill: every pre-PR-6 api_keys row gets org_id = the
--    account's personal org id. The join against orgs is
--    deterministic via the partial unique
--    `orgs_one_personal_per_account_uniq` (added in 00099) on
--    (personal_owner_account_id) WHERE personal_org = true.
UPDATE api_keys k
   SET org_id = o.id
  FROM orgs o
 WHERE o.personal_owner_account_id = k.account_id
   AND o.personal_org = true
   AND k.org_id IS NULL;

-- 2. NOT NULL flip. Safe because step 1 backfilled every row.
--    After this point the Store layer guarantees a non-null org_id
--    on every INSERT; the ScanAPIKey helper extends from 11 to 12
--    columns and the apiKeys struct picks up the new field.
ALTER TABLE api_keys
      ALTER COLUMN org_id SET NOT NULL;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
-- Reverse: drop NOT NULL only. The backfill UPDATE has no inverse —
-- a downgrade leaves every org_id value intact, so legacy
-- account-scoped code paths regain nullable semantics on a
-- downgrade without losing data. The FK stays in place (part of
-- 00099, not this migration).
ALTER TABLE api_keys
      ALTER COLUMN org_id DROP NOT NULL;
-- +goose StatementEnd
