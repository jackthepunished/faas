-- +goose Up
-- +goose StatementBegin
SELECT 'up SQL: 00277 — tenant_surfaces add per_host to cert_kind CHECK';
-- +goose StatementEnd

-- PR-D cert-engine-real-mint commit 5: widen the cert_kind
-- closed-set on tenant_surfaces to include 'per_host'. The
-- per_host value represents the >MaxSANPerCert fallback
-- shape (ADR-100 §"Cert engine shape" line 76): one cert per
-- hostname in the verified set, no SAN bundling, but still
-- served from the same on-disk certmagic storage layout.
--
-- The wrapper at pkg/gateway/cert_issuer_tenant_surface.go
-- today rejects per_host with a clear 'per-host bundler
-- ships in follow-up ADR-114' last_error. Adding the
-- constant now so:
--   - the schema is forward-compat (an operator writing
--     per_host via raw SQL doesn't hit the CHECK)
--   - ADR-114 doesn't need a schema-touching migration
--   - the type-level CertKind closed-set mirrors reality
--
-- The CHECK is inline (anonymous) in 00243_tenant_surfaces.sql
-- so postgres auto-named the constraint
-- (tenant_surfaces_cert_kind_check by convention). We look
-- up the constraint name dynamically via pg_constraint to
-- avoid hard-coding the auto-generated name (a future
-- migration that adds CONSTRAINT name to 00243 would
-- otherwise silently break this DROP).
DO $$
DECLARE
    ck_name text;
BEGIN
    SELECT con.conname INTO ck_name
      FROM pg_constraint con
      JOIN pg_class rel ON rel.oid = con.conrelid
      JOIN pg_attribute att ON att.attrelid = con.conrelid
                            AND att.attnum = ANY(con.conkey)
     WHERE rel.relname = 'tenant_surfaces'
       AND att.attname = 'cert_kind'
       AND con.contype = 'c'
     LIMIT 1;
    IF ck_name IS NOT NULL THEN
        EXECUTE format('ALTER TABLE tenant_surfaces DROP CONSTRAINT %I', ck_name);
    END IF;
END $$;

ALTER TABLE tenant_surfaces
    ADD CONSTRAINT tenant_surfaces_cert_kind_check
    CHECK (cert_kind IN ('per_host_san', 'shared_wildcard', 'per_host'));

-- +goose Down
-- +goose StatementBegin
SELECT 'down SQL: 00277 — tenant_surfaces narrow cert_kind CHECK back to v1';
-- +goose StatementEnd

-- The rollback is conservative: ANY row already written
-- with cert_kind='per_host' would block the narrowing,
-- so we fail loud with a clear error. Production rollbacks
-- of this migration are expected to be paired with a data
-- backfill that flips per_host rows back to per_host_san.
DO $$
DECLARE
    ck_name text;
BEGIN
    SELECT con.conname INTO ck_name
      FROM pg_constraint con
      JOIN pg_class rel ON rel.oid = con.conrelid
      JOIN pg_attribute att ON att.attrelid = con.conrelid
                            AND att.attnum = ANY(con.conkey)
     WHERE rel.relname = 'tenant_surfaces'
       AND att.attname = 'cert_kind'
       AND con.contype = 'c'
     LIMIT 1;
    IF ck_name IS NOT NULL THEN
        EXECUTE format('ALTER TABLE tenant_surfaces DROP CONSTRAINT %I', ck_name);
    END IF;
END $$;

ALTER TABLE tenant_surfaces
    ADD CONSTRAINT tenant_surfaces_cert_kind_check
    CHECK (cert_kind IN ('per_host_san', 'shared_wildcard'));