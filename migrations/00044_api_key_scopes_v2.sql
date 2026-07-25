-- +goose Up
-- +goose StatementBegin
--
-- 00044_api_key_scopes_v2.sql — IAM-1 (ADR-034 rev2).
--
-- Replaces the coarse closed vocabulary (admin | read | write) with
-- the fine-grained set (admin | apps:read | deploy:write |
-- secrets:read | secrets:write | usage:read) so a CI key can mint
-- with deploy-only scope and not inherit the broadest possible read.
--
-- The Up transaction does three things atomically so a partial
-- failure can't leave the table half-backfilled:
--
--   1. Backfill every legacy row. admin stays admin; read expands
--      to {apps:read, usage:read, secrets:read}; write expands to
--      {deploy:write, secrets:write}; the duplicate-case
--      (read+write+admin) collapses to the union via array_agg(distinct).
--
--   2. Emit one key.scopes_changed audit row per affected key
--      (actor='migration', not 'apid', so an operator investigating
--      the audit log can see the migration cut over time). Payload
--      carries {key_id, from: [old], to: [new]}.
--
--   3. Add a DB CHECK constraint closing the enum to the new six
--      values. <@ means {admin, deploy:write} is still allowed;
--      cardinality > 0 rejects a row that somehow lost its scopes
--      (defense in depth).
--
-- Down: drops the CHECK constraint only. The backfill is NOT reversed
-- (we can't distinguish {read} from {write} after the migration ran).
-- Customers rolling back must manually re-mint keys. Documented in
-- the release notes.
--
-- The DEFAULT '{admin}' on api_keys.scopes is preserved (no-op SQL;
-- comment only) — every existing row has scopes populated by 00036,
-- but the default is defensive for any future INSERT that omits the
-- column.

WITH snapshot AS (
    SELECT id, account_id, scopes AS old_scopes
      FROM api_keys
     WHERE scopes && ARRAY['read','write']::text[]
     FOR UPDATE
),
backfilled AS (
    UPDATE api_keys k
       SET scopes = (
           SELECT array_agg(DISTINCT v)
             FROM unnest(
                 ARRAY_CAT(
                     CASE WHEN 'admin' = ANY(s.old_scopes) THEN ARRAY['admin']::text[] ELSE ARRAY[]::text[] END,
                     CASE WHEN 'write' = ANY(s.old_scopes) THEN ARRAY['deploy:write','secrets:write']::text[] ELSE ARRAY[]::text[] END,
                     CASE WHEN 'read'  = ANY(s.old_scopes) THEN ARRAY['apps:read','usage:read','secrets:read']::text[] ELSE ARRAY[]::text[] END
                 )
             ) AS v
           )
      FROM snapshot s
     WHERE k.id = s.id
     RETURNING k.id, k.account_id, k.scopes AS new_scopes, s.old_scopes
)
INSERT INTO events (actor, kind, subject_id, data)
SELECT 'migration', 'key.scopes_changed', account_id,
       jsonb_build_object('key_id', id, 'from', old_scopes, 'to', new_scopes)
  FROM backfilled;

-- +goose StatementEnd

ALTER TABLE api_keys
    ADD CONSTRAINT api_keys_scopes_vocab_chk
        CHECK (scopes <@ ARRAY['admin','deploy:write','secrets:read','secrets:write','usage:read','apps:read']::text[]
               AND cardinality(scopes) > 0);

-- +goose Down
-- +goose StatementBegin
ALTER TABLE api_keys
    DROP CONSTRAINT IF EXISTS api_keys_scopes_vocab_chk;
-- +goose StatementEnd
