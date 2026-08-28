-- filename: 00506_repair_static_egress_schema.sql
-- +goose Up
-- +goose StatementBegin

-- Production repair for the static-egress schema.
--
-- Migrations 00336 and 00337 were previously used as reservation slots on
-- some deployed databases.  Their version numbers were later reused for the
-- real static-egress DDL, but goose records only the version number, not the
-- migration contents.  A database that had already recorded those slots
-- therefore skipped the real DDL forever.  The live pgstore then queried
-- columns and a table that did not exist, making GET /v1/apps fail with the
-- misleading capacity_unavailable response.
--
-- This migration is deliberately append-only and idempotent.  It repairs the
-- objects in place without changing or inventing customer data, and its Down
-- migration is a no-op so a rollback cannot destroy repaired state.

ALTER TABLE apps
    ADD COLUMN IF NOT EXISTS static_egress_ip inet NULL;

ALTER TABLE apps
    ADD COLUMN IF NOT EXISTS static_egress_ip_set_at timestamptz NULL;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1
          FROM pg_catalog.pg_constraint
         WHERE conname = 'apps_static_egress_ip_family_check'
           AND conrelid = 'apps'::regclass
    ) THEN
        ALTER TABLE apps
            ADD CONSTRAINT apps_static_egress_ip_family_check
            CHECK (static_egress_ip IS NULL OR family(static_egress_ip) = 4);
    END IF;
END
$$;

CREATE UNIQUE INDEX IF NOT EXISTS apps_static_egress_ip_key
    ON apps (static_egress_ip)
    WHERE static_egress_ip IS NOT NULL;

CREATE TABLE IF NOT EXISTS provisioned_static_egress_ips (
    account_id  uuid        NOT NULL,
    customer_ip inet        NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (account_id, customer_ip),
    CONSTRAINT provisioned_static_egress_ips_family_check
        CHECK (family(customer_ip) = 4)
);

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1
          FROM pg_catalog.pg_constraint
         WHERE conrelid = 'provisioned_static_egress_ips'::regclass
           AND contype = 'p'
    ) THEN
        ALTER TABLE provisioned_static_egress_ips
            ADD CONSTRAINT provisioned_static_egress_ips_pkey
            PRIMARY KEY (account_id, customer_ip);
    END IF;
END
$$;

-- CREATE TABLE IF NOT EXISTS is sufficient for the production failure (the
-- table is absent), but guard the named constraint as well so a partially
-- restored table is repaired instead of silently remaining unenforced.
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1
          FROM pg_catalog.pg_constraint
         WHERE conname = 'provisioned_static_egress_ips_family_check'
           AND conrelid = 'provisioned_static_egress_ips'::regclass
    ) THEN
        ALTER TABLE provisioned_static_egress_ips
            ADD CONSTRAINT provisioned_static_egress_ips_family_check
            CHECK (family(customer_ip) = 4);
    END IF;
END
$$;

CREATE INDEX IF NOT EXISTS provisioned_static_egress_ips_customer_ip_idx
    ON provisioned_static_egress_ips (customer_ip);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

-- This repair is intentionally irreversible.  Dropping these objects would
-- destroy live static-egress assignments and undo the feature migrations
-- that originally owned them.
SELECT 1;

-- +goose StatementEnd
