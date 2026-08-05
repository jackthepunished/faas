-- +goose Up
-- +goose StatementBegin
--
-- 00135_api_keys_provenance.sql — IAM hardening mega-PR (logical change 2).
--
-- Three advisory columns land on api_keys so SOC 2 audit lineage can
-- answer "who minted this key from which IP, and which key is its
-- rotation predecessor?" without joining through Loki.
--
--   * created_ip   inet — best-effort client IP at mint time
--                     (clientIPFromRequest output, post-Cloudflare/CDN
--                     normalized). NULL for pre-PR rows.
--   * created_ua   text — best-effort User-Agent at mint time.
--                     NULL for pre-PR rows. No length cap; the live
--                     UA is bounded by the HTTP framework's defaults
--                     and the logsanitize package strips control
--                     characters before this column is written.
--   * parent_key_id uuid — FK to api_keys(id). For the new row
--                     produced by a rotation. ON DELETE SET NULL keeps
--                     the lineage alive even if the predecessor is
--                     hard-deleted in a future PR.
--
-- Why NULLABLE + advisory (no NOT NULL, no CHECK):
--
--   * Pre-PR rows are the historical truth — they have no IP, no UA,
--     no parent. Backfilling fake values would feed the audit
--     pipeline garbage rows that look identical to real mints.
--   * The unix-socket code path (gatewayd-internal → apid, internal
--     admin tools) does not have a meaningful client IP. A NOT NULL
--     constraint would force a sentinel like '0.0.0.0' which is
--     indistinguishable from a real zero-address. NULL is the correct
--     "we don't know" marker.
--   * The audit `key.created` / `api_key.created` / `key.rotated` /
--     `api_key.rotated` rows (§"audit payload" in the ADR-034 update)
--     carry the same provenance as the column, so the absence of a
--     column value is never a source-of-truth gap.
--
-- Why NO backfill:
--   * Pre-PR rows have no recoverable IP/UA. Backfilling from
--     pg_stat_activity (the historical database-side IP) would
--     stamp the server's localhost instead of the real client.
--   * parent_key_id is derivable from the existing rotated_from_id
--     column for non-atomic rotations, but the FK references rows
--     that may have been hard-deleted; backfilling would 23502.
--     Leave NULL.
--
-- Why replay-safe (`IF NOT EXISTS`):
--   * Goose replay-safety contract (ADR-041). A second `goose up`
--     on the same DB is a no-op instead of a 42710 error.
--
-- Slot 135 — chained after 00134_api_keys_org_bound. No reservation
-- fence needed unless a sibling PR claims 135 between PR-creation
-- and merge (the cross-PR slot gate will fire then and the rename
-- is mechanical per migration-slot-renumber-at-pr-creation).

ALTER TABLE api_keys ADD COLUMN IF NOT EXISTS created_ip    inet;
ALTER TABLE api_keys ADD COLUMN IF NOT EXISTS created_ua    text;
ALTER TABLE api_keys ADD COLUMN IF NOT EXISTS parent_key_id uuid
  REFERENCES api_keys(id) ON DELETE SET NULL;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
-- Reverse: drop the FK first (parent_key_id), then the two
-- standalone columns. The cascade is the dependency order — the
-- FK constraint on parent_key_id is the only one that binds.
ALTER TABLE api_keys DROP COLUMN IF EXISTS parent_key_id;
ALTER TABLE api_keys DROP COLUMN IF EXISTS created_ua;
ALTER TABLE api_keys DROP COLUMN IF EXISTS created_ip;
-- +goose StatementEnd
