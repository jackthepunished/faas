-- 00336_repair_app_secrets_scope.sql — repair the ADR-092 schema/ledger
-- split caused by the historical 00217 slot reservation. Some databases
-- recorded 00217 before the real app_secrets scope migration was moved into
-- that slot, so goose considers the migration applied while the column is
-- still absent. This append-only repair brings those databases to the same
-- shape as 00217 without mutating migration history.

-- +goose Up
-- +goose StatementBegin

ALTER TABLE app_secrets
    ADD COLUMN IF NOT EXISTS scope text NOT NULL DEFAULT 'default';

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_catalog.pg_constraint
        WHERE conname = 'app_secrets_scope_shape'
          AND conrelid = 'app_secrets'::regclass
    ) THEN
        ALTER TABLE app_secrets
            ADD CONSTRAINT app_secrets_scope_shape
            CHECK (scope ~ '^[a-z0-9]([a-z0-9-]{1,38})[a-z0-9]$');
    END IF;
END$$;

DO $$
BEGIN
    IF EXISTS (
        SELECT 1 FROM pg_catalog.pg_constraint
        WHERE conname = 'app_secrets_pkey'
          AND conrelid = 'app_secrets'::regclass
          AND array_length(conkey, 1) = 2
    ) THEN
        ALTER TABLE app_secrets DROP CONSTRAINT app_secrets_pkey;
        ALTER TABLE app_secrets
            ADD CONSTRAINT app_secrets_pkey PRIMARY KEY (app_id, scope, key);
    END IF;
END$$;

CREATE INDEX IF NOT EXISTS app_secrets_account_app_scope_idx
    ON app_secrets (account_id, app_id, scope);

-- +goose StatementEnd

-- +goose Down
-- Forward-only repair: reverting this migration would recreate the broken
-- schema/ledger split. Keep the corrected 00217 shape in place.
-- +goose StatementBegin
SELECT 1;
-- +goose StatementEnd
