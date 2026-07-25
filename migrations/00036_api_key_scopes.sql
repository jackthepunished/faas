-- +goose Up
-- Per-key scopes. Every API key carries an explicit set of scopes; the
-- middlewares in cmd/apid reject routes whose required scope is not present.
-- Existing rows are backfilled to '{admin}' so the migration is zero-downtime
-- (today every key is effectively admin — admin is the legacy full-access
-- scope). See ADR-034 for the design rationale.
ALTER TABLE api_keys
    ADD COLUMN scopes text[] NOT NULL DEFAULT '{admin}';

-- +goose Down
ALTER TABLE api_keys DROP COLUMN scopes;
