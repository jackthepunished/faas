-- +goose Up
-- +goose StatementBegin

-- Per-app private-registry Basic Auth (issue #461 / ADR-062). One row
-- per (app_id, registry_host); the password is sealed at rest via
-- secretbox.SealBytes with namespace="registry_creds". Username stays
-- in plaintext (metadata, like a secret key) — mirroring the AppSecret
-- precedent where each value is sealed independently and metadata is
-- not.
--
-- Why (app_id, registry) UNIQUE: apid's PUT is an upsert (ON CONFLICT).
-- Two different apps may share a registry (and vice versa); within an
-- app, one row per registry.
--
-- Why FK on both account_id AND app_id: the cascade lever is the app
-- delete (free-tier delete path), but the account scope is needed for
-- cross-account IDOR-safety in pgstore.GetAppRegistryCredential (the
-- query scopes by (account_id, app_id, registry)). Keeping both FKs
-- gives the schema-level guarantee.
--
-- Why length(registry) <= 253: RFC 1035 max hostname length. Port
-- suffixes make this slightly generous; the API layer's
-- normalizeRegistryHost enforces a stricter shape and the CHECK is a
-- safety net for direct INSERTs.

create table if not exists app_registry_credentials (
  id                 uuid        primary key default gen_random_uuid(),
  account_id         uuid        not null references accounts(id) on delete cascade,
  app_id             uuid        not null references apps(id)     on delete cascade,
  registry           text        not null,
  username           text        not null,
  password_encrypted bytea       not null,
  created_at         timestamptz not null default now(),
  updated_at         timestamptz not null default now(),
  last_used_at       timestamptz,
  constraint app_registry_credentials_registry_chk
    check (length(registry) > 0 and length(registry) <= 253),
  constraint app_registry_credentials_username_chk
    check (length(username) > 0 and length(username) <= 256),
  constraint app_registry_credentials_password_chk
    check (length(password_encrypted) > 0),
  constraint app_registry_credentials_app_registry_uq
    unique (app_id, registry)
);

create index if not exists app_registry_credentials_account_idx
  on app_registry_credentials (account_id);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
drop table if exists app_registry_credentials;
-- +goose StatementEnd