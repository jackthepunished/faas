-- +goose Up
-- +goose StatementBegin

-- 00061_app_envs.sql — issue #395 / ADR-045.
--
-- Per-app plaintext runtime env. Mirrors 00008_app_secrets.sql exactly
-- except: value TEXT plaintext (NOT bytea ciphertext), no sealing, no
-- G2-shaped age key.
--
-- Column rationale
-- ----------------
--   account_id  — FK to accounts(id) ON DELETE CASCADE so a GDPR-erased
--                 account drops its env rows along with the apps.
--                 Different from the secrets table by intent: secrets
--                 table cascades from accounts via the same FK, so this
--                 matches.
--   app_id      — no hard FK. App deletion is async via pgstore.DeleteApp
--                 which cascades env rows manually for the same reason
--                 pgstore.DeleteApp cascades secrets — see 00008 header
--                 for the full rationale.
--   key         — pinned to env-var shape ^[A-Z][A-Z0-9_]*$ ≤128 chars
--                 so the same key can be put into the guest's exec
--                 environ unmodified. Same CHECK constraint as
--                 app_secrets.key (00008:32).
--   value       — plaintext TEXT. env vars are explicitly non-credential
--                 config (issue #395 acceptance #5), so we do NOT seal
--                 them. Anything sensitive belongs in /secrets, not
--                 /env — endpoints point at that contract.
--   timestamps  — created_at / updated_at / on-UPDATE now(). Same shape
--                 as app_secrets.
--
-- Why a separate app_envs table (not a column on apps.manifest):
-- per-app, per-key mutable env has its own quota row in
-- pkg/api/limits.go (EnvVarsMax). Counting keys in a JSONB column
-- requires a fresh SQL query per PUT — putting it in a relational
-- table lets CountAppEnv fold into a counted index scan, mirroring
-- CountAppSecrets.
--
-- Replay-safety: every DDL uses IF NOT EXISTS so re-applying this
-- against a drifted box (DDL landed, goose row missing) is a no-op.

create table if not exists app_envs (
  account_id  uuid        not null references accounts(id) on delete cascade,
  app_id      uuid        not null,
  key         text        not null,
  value       text        not null,
  created_at  timestamptz not null default now(),
  updated_at  timestamptz not null default now(),
  primary key (app_id, key),
  constraint app_envs_key_shape check (key ~ '^[A-Z][A-Z0-9_]*$' and length(key) <= 128)
);

create index if not exists app_envs_account_idx
  on app_envs (account_id);

create index if not exists app_envs_app_idx
  on app_envs (app_id);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
drop table if exists app_envs;
-- +goose StatementEnd
