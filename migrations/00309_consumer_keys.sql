-- filename: 00309_consumer_keys.sql
-- +goose Up
-- +goose StatementBegin

-- Consumer keys (issue #975 item #5 / ADR-120). Today every customer
-- app either ships public (no end-customer auth) or vendors their own
-- auth on top of `api_keys` — but `api_keys` is account-scoped and
-- operator-credential-grade, not customer-of-an-app-grade. This
-- migration stands up the per-(account, app) identity primitive that
-- items #6 (consumer analytics), #7 (route metrics cold-start), and
-- #8 (queryable request logs) pin their stable consumer_id label to.
--
-- Wire format `ck_<8-hex-prefix>_<64-hex-secret>` (see ADR-120 §D2).
-- The (app_id, prefix) composite index narrows the inbound hash
-- compare to ~one row before the SHA-256 comparison.
--
-- Scope vocab (D4) is the closed set {read, write, admin}. The
-- vocabulary is enforced at the apid write boundary (handlers); the
-- CHECK below is defense-in-depth — a typo at the apid gate fails
-- fast, a typo at a manual SQL insert fails the CHECK.

CREATE TABLE IF NOT EXISTS consumer_keys (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  account_id uuid NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  app_id uuid NOT NULL REFERENCES apps(id) ON DELETE CASCADE,
  name text NOT NULL,
  prefix text NOT NULL,
  hashed_secret bytea NOT NULL,
  scopes text[] NOT NULL DEFAULT '{}',
  expires_at timestamptz NULL,
  last_used_at timestamptz NULL,
  revoked_at timestamptz NULL,
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT consumer_keys_name_len_chk CHECK (
    length(name) BETWEEN 1 AND 64
  ),
  CONSTRAINT consumer_keys_prefix_len_chk CHECK (
    length(prefix) BETWEEN 1 AND 16
  ),
  CONSTRAINT consumer_keys_hashed_secret_len_chk CHECK (
    octet_length(hashed_secret) = 32
  ),
  CONSTRAINT consumer_keys_scopes_vocab_chk CHECK (
    scopes <@ ARRAY['read'::text, 'write'::text, 'admin'::text]::text[]
    AND cardinality(scopes) > 0
  ),
  CONSTRAINT consumer_keys_expires_after_created_chk CHECK (
    expires_at IS NULL OR expires_at > created_at
  ),
  CONSTRAINT consumer_keys_revoked_state_chk CHECK (
    (revoked_at IS NULL) OR (revoked_at >= created_at)
  )
);

-- UNIQUE on the user-visible identity. Per ADR-120 §D1, the
-- (account_id, app_id, name) triple is the customer-visible name.
-- The prefix is greppable, NOT a uniqueness key — two apps in the
-- same account can each have a `customer-portal-key` and they don't
-- collide.
CREATE UNIQUE INDEX IF NOT EXISTS consumer_keys_unique_name
  ON consumer_keys (account_id, app_id, name);

-- Gateway-side lookup hot path: every inbound request with a
-- `ck_<prefix>_<secret>` header hits `(app_id, prefix)` to narrow
-- to ~one row before the hash compare. Composite index covers both
-- columns.
CREATE INDEX IF NOT EXISTS consumer_keys_app_prefix_idx
  ON consumer_keys (app_id, prefix);

-- List endpoints: GET /v1/apps/{slug}/consumer-keys filters by
-- (account_id, app_id). The app_id index alone is enough; the
-- account_id list is covered by the unique index above.
CREATE INDEX IF NOT EXISTS consumer_keys_app_idx
  ON consumer_keys (app_id);

-- updated_at trigger. Same shape as 00304_cors_presets.sql: table-
-- scoped function (no forward dependency on a future shared helper),
-- DROP TRIGGER IF EXISTS makes CREATE TRIGGER replay-safe (a second
-- MigrateUp finds the trigger, drops it, recreates).
CREATE OR REPLACE FUNCTION consumer_keys_set_updated_at()
  RETURNS trigger
  LANGUAGE plpgsql
AS $$
BEGIN
  NEW.updated_at = now();
  RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS consumer_keys_set_updated_at_trg ON consumer_keys;
CREATE TRIGGER consumer_keys_set_updated_at_trg
  BEFORE UPDATE ON consumer_keys
  FOR EACH ROW
  EXECUTE FUNCTION consumer_keys_set_updated_at();

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

-- Forward-only migration. Drop order: trigger first, then the
-- function, then the table. The composite index and the unique
-- index are dropped with the table. PR #5-B's
-- apps.consumer_auth_mode column lives in a separate slot (00309)
-- and is NOT dropped here.

DROP TRIGGER IF EXISTS consumer_keys_set_updated_at_trg ON consumer_keys;
DROP FUNCTION IF EXISTS consumer_keys_set_updated_at();
DROP TABLE IF EXISTS consumer_keys;

-- +goose StatementEnd