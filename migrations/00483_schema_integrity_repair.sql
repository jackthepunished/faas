-- filename: 00483_schema_integrity_repair.sql
-- +goose Up
-- +goose StatementBegin

-- Production repair for databases whose goose ledger reached the
-- corresponding feature migrations while their DDL was lost (restored
-- dump, partial deploy, or an out-of-band schema edit). This migration is
-- intentionally append-only and idempotent: it repairs the two objects
-- currently required by the live scheduler/API without replaying or
-- editing migrations 00411 and 00434.

-- 00411: the scheduler and deployment readers require this persisted
-- liveness counter. ADD COLUMN IF NOT EXISTS also repairs a partially
-- applied table without changing existing values.
ALTER TABLE deployments
    ADD COLUMN IF NOT EXISTS liveness_restart_count INT NOT NULL DEFAULT 0;

ALTER TABLE deployments
    DROP CONSTRAINT IF EXISTS deployments_liveness_restart_count_nonneg_chk;
ALTER TABLE deployments
    ADD CONSTRAINT deployments_liveness_restart_count_nonneg_chk
        CHECK (liveness_restart_count >= 0);

-- 00434: schedd and gatewayd-internal use this singleton row for the
-- cluster-wide internal-service signing key. The operator populates the
-- row separately; repairing the table must not invent credentials.
CREATE TABLE IF NOT EXISTS cluster_signing_keys (
    id              int          PRIMARY KEY DEFAULT 1 CHECK (id = 1),
    key_id          text         NOT NULL CHECK (key_id ~ '^[A-Za-z0-9_-]{22}$'),
    public_key_pem  text         NOT NULL CHECK (public_key_pem LIKE '-----BEGIN PUBLIC KEY-----%'),
    sealed_blob     bytea        NOT NULL,
    created_at      timestamptz  NOT NULL DEFAULT now(),
    rotated_at      timestamptz,
    retired_at      timestamptz
);

CREATE OR REPLACE FUNCTION cluster_signing_keys_notify() RETURNS trigger AS $$
BEGIN
    PERFORM pg_notify('cluster_signing_keys_changed', TG_TABLE_NAME);
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS cluster_signing_keys_changed_trg ON cluster_signing_keys;
CREATE TRIGGER cluster_signing_keys_changed_trg
AFTER INSERT OR UPDATE OR DELETE ON cluster_signing_keys
FOR EACH STATEMENT EXECUTE FUNCTION cluster_signing_keys_notify();

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

-- The repair is deliberately a no-op on downgrade. Removing these objects
-- would destroy live data and would incorrectly undo the feature migrations
-- that originally owned them.
SELECT 1;

-- +goose StatementEnd
