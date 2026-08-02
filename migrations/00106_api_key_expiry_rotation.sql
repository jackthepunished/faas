-- +goose Up
-- +goose StatementBegin
--
-- 00106_api_key_expiry_rotation.sql — IAM-5 (issue #189 / ADR draft).
--
-- Three capabilities, all additive:
--
--   1. Per-key expiry (`expires_at`). Existing rows keep
--      `expires_at = NULL` (never expires) so the migration is
--      non-breaking. The auth path is what enforces the gate
--      (state.AuthenticateKey), not the schema. New non-admin
--      keys receive `now() + 365 days` at creation; admin keys
--      stay nullable per the existing admin contract.
--
--   2. A `status` enum ('active' | 'grace' | 'revoked') with a
--      CHECK constraint. Mirrors the existing `accounts.status`
--      and `orgs.status` shapes. `revoked` is the terminal state;
--      `grace` is the post-rotation window where the old key
--      still authenticates. Transitions are validated in code
--      (state.AuthenticateKey + the rotate store op), not by a
--      trigger — the schema is the floor, the store is the wall.
--
--   3. Lineage columns: `revoked_at` (terminal timestamp, set
--      on revoke or atomic rotation or lazy expiry) and
--      `rotated_from_id` (FK to the predecessor on the new key
--      after rotation; nullable on the original key).
--
-- Plus a per-account grace-window override column
-- `accounts.key_grace_window_days`:
--
--   * NULL  → use the 7-day plan default
--   * 0     → atomic rotation; old key revoked immediately
--   * > 0   → explicit days
--
-- No backfill of `expires_at` for existing rows. Legacy admin
-- keys stay non-expiring; legacy non-admin keys become
-- non-expiring until the customer rotates them, at which point
-- the rotation store op mints the new key with the 365-day
-- default. A retroactive backfill would cause a surprise mass
-- expiration and is deliberately out of scope (issue #189 §Why).
--
-- Indexes are conservative:
--
--   * `api_keys_account_status_idx` — covers the
--     "(account_id, status)" lookup used by listKeys (the
--     dashboard's "active vs grace vs revoked" filter).
--   * `api_keys_active_grace_idx` — partial index on the
--     union of the two non-terminal states; speeds up the
--     AuthenticateKey hot path for the common case.
--   * `api_keys_rotated_from_idx` — partial index on the
--     lineage FK for the dashboard's "rotated from" drill-down
--     and the future "find successor" helpers.
--
-- Replay-safe shape (mirrors 00047 / 00053):
--
--   * `ALTER TABLE ... ADD COLUMN IF NOT EXISTS` for every
--     additive column. A box whose schema is ahead of its
--     goose ledger (the 2026-07-27 cd-digitalocean failure
--     pattern) survives a re-run without tripping 42701.
--   * The two CHECK constraints are wrapped in DO blocks that
--     probe `pg_constraint` first; the same rationale.
--   * `CREATE INDEX IF NOT EXISTS` for all three indexes;
--     partial-index syntax is preserved inside the IF NOT
--     EXISTS guard.
--
-- The replay-safety contract is also pinned at PR time by the
-- `db.MigrateUp` second-call assertion in
-- 00106_api_key_expiry_rotation_test.go (same precedent as
-- 00053_deployments_source_url_test.go:171-183).

ALTER TABLE api_keys
    ADD COLUMN IF NOT EXISTS expires_at      timestamp with time zone,
    ADD COLUMN IF NOT EXISTS status          text NOT NULL DEFAULT 'active',
    ADD COLUMN IF NOT EXISTS revoked_at      timestamp with time zone,
    ADD COLUMN IF NOT EXISTS rotated_from_id uuid REFERENCES api_keys(id);

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_catalog.pg_constraint
        WHERE conname = 'api_keys_status_check'
          AND conrelid = 'api_keys'::regclass
    ) THEN
        ALTER TABLE api_keys
            ADD CONSTRAINT api_keys_status_check
            CHECK (status IN ('active', 'grace', 'revoked'));
    END IF;
END$$;

CREATE INDEX IF NOT EXISTS api_keys_account_status_idx
    ON api_keys (account_id, status);
CREATE INDEX IF NOT EXISTS api_keys_active_grace_idx
    ON api_keys (account_id)
    WHERE status IN ('active', 'grace');
CREATE INDEX IF NOT EXISTS api_keys_rotated_from_idx
    ON api_keys (rotated_from_id)
    WHERE rotated_from_id IS NOT NULL;

ALTER TABLE accounts
    ADD COLUMN IF NOT EXISTS key_grace_window_days integer;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_catalog.pg_constraint
        WHERE conname = 'accounts_key_grace_window_days_check'
          AND conrelid = 'accounts'::regclass
    ) THEN
        ALTER TABLE accounts
            ADD CONSTRAINT accounts_key_grace_window_days_check
            CHECK (key_grace_window_days IS NULL OR key_grace_window_days >= 0);
    END IF;
END$$;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE accounts DROP CONSTRAINT IF EXISTS accounts_key_grace_window_days_check;
ALTER TABLE accounts DROP COLUMN IF EXISTS key_grace_window_days;

DROP INDEX IF EXISTS api_keys_rotated_from_idx;
DROP INDEX IF EXISTS api_keys_active_grace_idx;
DROP INDEX IF EXISTS api_keys_account_status_idx;

ALTER TABLE api_keys DROP CONSTRAINT IF EXISTS api_keys_status_check;
ALTER TABLE api_keys DROP COLUMN IF EXISTS rotated_from_id;
ALTER TABLE api_keys DROP COLUMN IF EXISTS revoked_at;
ALTER TABLE api_keys DROP COLUMN IF EXISTS status;
ALTER TABLE api_keys DROP COLUMN IF EXISTS expires_at;
-- +goose StatementEnd
