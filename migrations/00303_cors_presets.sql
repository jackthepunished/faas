-- filename: 00303_cors_presets.sql
-- +goose Up
-- +goose StatementBegin

-- CORS presets (issue #975 item #4 / Mega-Foundation #979-b). Today
-- every kind=cors edge rule has to repeat the allow_origins /
-- allow_methods / allow_headers / expose_headers /
-- allow_credentials / max_age_seconds tuple inline. Customers
-- shipping the same CORS posture to N apps (typical: an org with
-- a marketing site, a docs site, and a SaaS dashboard that all
-- share the same allowlist) end up with N copies of the same
-- payload. The audit identified this as the #4 missing
-- capability. This migration stands up the data model; the
-- compile-side merge lives in cmd/gatewayd-internal/edge_rules.go
-- and the per-rule cors.preset_id field + the apid CRUD surface
-- land in PR-B (#979-c, slot 00295).

-- Scope semantics: a preset is owned by (account_id, name). The
-- app_id column is NULLABLE — when set, the preset is locked to
-- one app; when NULL, the preset is account-wide (reusable
-- across all of the account's apps). The UNIQUE key uses
-- COALESCE(app_id, '00000000-0000-0000-0000-000000000000') so the
-- same name can exist once per (account, app) tuple. NULL would
-- otherwise have infinite-multiplicity in a unique index per the
-- standard SQL NULL-distinct-from-NULL semantics; COALESCE maps
-- the account-wide case to a sentinel UUID that has no real
-- counterpart in the apps table (gen_random_uuid() returns a v4
-- UUID, never all-zeros), so the index is well-formed.

-- Closed-set enforcement on the wire-side fields (allow_origins,
-- allow_methods, allow_headers, expose_headers) lives in
-- pkg/api/dto.go (the apid write boundary) — this migration only
-- pins the storage shape and the size/name bounds. The
-- CHECKs below are defense-in-depth; the apid path is the
-- canonical gate.

CREATE TABLE IF NOT EXISTS cors_presets (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  account_id uuid NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  app_id uuid REFERENCES apps(id) ON DELETE CASCADE,
  name text NOT NULL,
  description text,
  allow_origins text[] NOT NULL,
  allow_methods text[] NOT NULL,
  allow_headers text[] NOT NULL DEFAULT '{}',
  expose_headers text[] NOT NULL DEFAULT '{}',
  allow_credentials boolean NOT NULL DEFAULT false,
  max_age_seconds integer NOT NULL DEFAULT 600,
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT cors_presets_name_check CHECK (
    length(name) BETWEEN 1 AND 64
  ),
  CONSTRAINT cors_presets_max_age_check CHECK (
    max_age_seconds BETWEEN 0 AND 86400
  )
);

-- COALESCE-backed unique index. SQL does not allow expression-based
-- UNIQUE inside a CREATE TABLE — the constraint must be a
-- separate CREATE UNIQUE INDEX. The COALESCE maps the account-wide
-- case (app_id IS NULL) to a sentinel UUID that has no real
-- counterpart in the apps table (gen_random_uuid() returns a v4
-- UUID, never all-zeros), so the index is well-formed. NULL would
-- otherwise have infinite-multiplicity in a unique index per the
-- standard SQL NULL-distinct-from-NULL semantics; COALESCE is the
-- standard workaround (same pattern as the api_keys COALESCE key).
CREATE UNIQUE INDEX IF NOT EXISTS cors_presets_unique_name
  ON cors_presets (
    account_id,
    COALESCE(app_id, '00000000-0000-0000-0000-000000000000'::uuid),
    name
  );

CREATE INDEX IF NOT EXISTS cors_presets_account_idx ON cors_presets (account_id);

-- updated_at trigger. We don't reuse the edge_rules_set_updated_at
-- function (its name is misleading for a non-edge_rules table) and
-- declaring a table-scoped function is a poor precedent. The body
-- is the same generic "set NEW.updated_at = now()" every other
-- updated_at trigger uses. Inline declaration so the migration
-- has no forward-dependency on a future shared helper.
CREATE OR REPLACE FUNCTION cors_presets_set_updated_at()
  RETURNS trigger
  LANGUAGE plpgsql
AS $$
BEGIN
  NEW.updated_at = now();
  RETURN NEW;
END;
$$;

-- DROP TRIGGER IF EXISTS makes the CREATE TRIGGER replay-safe
-- (a second MigrateUp finds the trigger already there, drops it,
-- and recreates). The same pattern is used in 00192_edge_rules.
DROP TRIGGER IF EXISTS cors_presets_set_updated_at_trg ON cors_presets;
CREATE TRIGGER cors_presets_set_updated_at_trg
  BEFORE UPDATE ON cors_presets
  FOR EACH ROW
  EXECUTE FUNCTION cors_presets_set_updated_at();

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

-- Forward-only migration. Drop order: trigger first, then the
-- function, then the table. The COALESCE-backed UNIQUE index is
-- dropped with the table. Cross-checked against 00293's Down (no
-- data back-fill on reverse — PR-B has not yet added the
-- cors.preset_id field so a reverse is clean).

DROP TRIGGER IF EXISTS cors_presets_set_updated_at_trg ON cors_presets;
DROP FUNCTION IF EXISTS cors_presets_set_updated_at();
DROP TABLE IF EXISTS cors_presets;

-- +goose StatementEnd
