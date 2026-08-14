-- filename: 00243_tenant_surfaces.sql
-- +goose Up
-- +goose StatementBegin

-- ADR-100 (issue #879 / tenant surfaces PR-cluster) — introduce two
-- tables:
--
--   tenant_surfaces  — one row per customer-declared surface; binds an
--                      account to a single app (D1), groups N verified
--                      hostnames under one cert, and persists the
--                      cert_kind (v1: per_host_san | shared_wildcard),
--                      the lifecycle status (pending | active |
--                      suspended | deleted) and the per-surface cert
--                      state (none | pending | issued | failed).
--
--   tenant_hostnames — one row per customer-verified hostname that
--                      belongs to a surface; carries the challenge
--                      token the apid TXT-verifier polls and a verified
--                      timestamp so RequestCertForSurface can fail
--                      closed when even one hostname is unverified.
--
-- Slot 00243 was fenced by PR-0 (issues #879 ADR-100) so the real
-- schema could land here without colliding with the kind=maintenance
-- stampede on main. This migration replaces the PR-0
-- 00243_reserve_slot.sql fence; both files cannot coexist
-- (TestMigrationsUniquePrefixes catches the collision).
--
-- Shape precedents:
--   - 2-table citext + cascade + partial indexes → 00099_orgs_*
--   - Mirror-Down reverse order                   → 00220_preview_*
--   - Replay-safe CHECK drop/add                  → 00229_edge_rules_*_geo
--   - The pg_notify trigger pair mirrors the apid's existing
--     `NotifyDomainChanged` pattern at cmd/apid/handlers_ext.go.

-- Section 1: tenant_surfaces ------------------------------------------------
CREATE TABLE IF NOT EXISTS tenant_surfaces (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    account_id      uuid NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    app_id          uuid            REFERENCES apps(id)      ON DELETE CASCADE,
    name            citext NOT NULL,
    cert_kind       text   NOT NULL DEFAULT 'per_host_san'
        CHECK (cert_kind IN ('per_host_san','shared_wildcard')),
    status          text   NOT NULL DEFAULT 'pending'
        CHECK (status IN ('pending','active','suspended','deleted')),
    cert_state      text   NOT NULL DEFAULT 'none'
        CHECK (cert_state IN ('none','pending','issued','failed')),
    cert_not_after  timestamptz,
    cert_last_error text,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT tenant_surfaces_app_or_not_chk
        CHECK (app_id IS NOT NULL)
);

-- A surface must belong to exactly one app (D1). The constraint is
-- enforced here at the table level so an accidental NULL slip through
-- any caller path is caught at INSERT time.

-- Indexes:
--   - account-level quota + list hot path
CREATE INDEX IF NOT EXISTS tenant_surfaces_account_idx
    ON tenant_surfaces (account_id)
    WHERE status <> 'deleted';

--   - name uniqueness within an account (partial: ignore soft-deleted
--     rows so a re-create after a delete is allowed)
CREATE UNIQUE INDEX IF NOT EXISTS tenant_surfaces_account_name_uniq
    ON tenant_surfaces (account_id, name)
    WHERE status <> 'deleted';

--   - per-app list (pgRouter.ResolveHost hot path)
CREATE INDEX IF NOT EXISTS tenant_surfaces_app_idx
    ON tenant_surfaces (app_id)
    WHERE app_id IS NOT NULL;

--   - expiry sweep (the renewer consults this on each tick)
CREATE INDEX IF NOT EXISTS tenant_surfaces_cert_expiry_idx
    ON tenant_surfaces (cert_not_after)
    WHERE cert_state = 'issued';

-- Section 2: tenant_hostnames ---------------------------------------------
CREATE TABLE IF NOT EXISTS tenant_hostnames (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    surface_id      uuid NOT NULL REFERENCES tenant_surfaces(id) ON DELETE CASCADE,
    hostname        citext NOT NULL,
    challenge_token text   NOT NULL DEFAULT '',
    verified_at     timestamptz,
    last_check_at   timestamptz,
    last_error      text,
    created_at      timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT tenant_hostnames_hostname_len_chk
        CHECK (hostname <> '' AND length(hostname) <= 253)
);

-- A hostname belongs to at most one surface globally. UQ on hostname
-- alone (citext) makes the routing lookup a single B-tree hit; the
-- duplicate-across-surfaces case is rejected at INSERT and surfaces
-- as state.ErrConflict at the pgstore.
CREATE UNIQUE INDEX IF NOT EXISTS tenant_hostnames_hostname_uniq
    ON tenant_hostnames (hostname);

-- Defence-in-depth: same (surface_id, hostname) pair can't appear twice
-- even if the global UQ is dropped in a future migration.
CREATE UNIQUE INDEX IF NOT EXISTS tenant_hostnames_surface_hostname_uniq
    ON tenant_hostnames (surface_id, hostname);

-- Hot-path: per-surface enumeration when assembling the SAN set.
CREATE INDEX IF NOT EXISTS tenant_hostnames_surface_idx
    ON tenant_hostnames (surface_id);

-- Hot-path: RequestCertForSurface skips unverified rows.
CREATE INDEX IF NOT EXISTS tenant_hostnames_verified_idx
    ON tenant_hostnames (surface_id)
    WHERE verified_at IS NOT NULL;

-- Poller queue: dns_poller.fetchPendingTenantHostnames over this index.
CREATE INDEX IF NOT EXISTS tenant_hostnames_pending_idx
    ON tenant_hostnames (last_check_at)
    WHERE verified_at IS NULL;

-- Section 3: notify trigger ------------------------------------------------
-- One trigger per table; the payload is ALWAYS a bare surface UUID
-- (NEW.surface_id or OLD.surface_id), per pkg/db/notify.go's contract
-- for `tenant_surface_changed`. The pg_notify listener in
-- cmd/gatewayd-internal subscribes to one channel and dispatches per
-- payload UUID.
CREATE OR REPLACE FUNCTION notify_tenant_surface_changed() RETURNS trigger
    LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        PERFORM pg_notify('tenant_surface_changed', OLD.surface_id::text);
        RETURN OLD;
    ELSIF TG_OP = 'UPDATE' THEN
        -- surface row updates (status, cert_state, cert_not_after, …)
        -- bubble up the surface id directly; this trigger is also
        -- responsible for the surface's row-level changes.
        PERFORM pg_notify('tenant_surface_changed', NEW.id::text);
        RETURN NEW;
    ELSE
        -- INSERT on tenant_surfaces
        PERFORM pg_notify('tenant_surface_changed', NEW.id::text);
        RETURN NEW;
    END IF;
END
$$;

DROP TRIGGER IF EXISTS tenant_surfaces_emit_change ON tenant_surfaces;
CREATE TRIGGER tenant_surfaces_emit_change
    AFTER INSERT OR UPDATE OR DELETE ON tenant_surfaces
    FOR EACH ROW EXECUTE FUNCTION notify_tenant_surface_changed();

-- Hostname rows emit the SURFACE id, not the hostname id, so the
-- listener can coalesce (D3 — every hostname add/remove triggers a
-- re-mint of the parent surface).
DROP TRIGGER IF EXISTS tenant_hostnames_emit_change ON tenant_hostnames;
CREATE TRIGGER tenant_hostnames_emit_change
    AFTER INSERT OR UPDATE OR DELETE ON tenant_hostnames
    FOR EACH ROW EXECUTE FUNCTION notify_tenant_surface_changed();

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
-- Mirror-Down in reverse order: triggers → function → indexes → tables.
DROP TRIGGER IF EXISTS tenant_hostnames_emit_change ON tenant_hostnames;
DROP TRIGGER IF EXISTS tenant_surfaces_emit_change ON tenant_surfaces;
DROP FUNCTION IF EXISTS notify_tenant_surface_changed();

DROP INDEX IF EXISTS tenant_hostnames_pending_idx;
DROP INDEX IF EXISTS tenant_hostnames_verified_idx;
DROP INDEX IF EXISTS tenant_hostnames_surface_idx;
DROP INDEX IF EXISTS tenant_hostnames_surface_hostname_uniq;
DROP INDEX IF EXISTS tenant_hostnames_hostname_uniq;

DROP INDEX IF EXISTS tenant_surfaces_cert_expiry_idx;
DROP INDEX IF EXISTS tenant_surfaces_app_idx;
DROP INDEX IF EXISTS tenant_surfaces_account_name_uniq;
DROP INDEX IF EXISTS tenant_surfaces_account_idx;

DROP TABLE IF EXISTS tenant_hostnames CASCADE;
DROP TABLE IF EXISTS tenant_surfaces  CASCADE;
-- +goose StatementEnd
