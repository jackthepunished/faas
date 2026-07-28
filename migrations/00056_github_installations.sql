-- +goose Up
-- +goose StatementBegin

-- 00056_github_installations.sql — PR-C (audit-gap closure).
--
-- Until PR-C, githubd's install state lived in an in-memory map
-- (pkg/githubd/realservice.go's `s.installs`). That map evaporated
-- on kill -TERM, and the dashboard's /v1/install/repos/list +
-- /v1/apps/{slug}/install/bind started 502'ing the moment githubd
-- came back up. PR-C moves the source of truth to Postgres:
--
--   * account_id            — PK + FK to accounts(id). ON DELETE
--                              CASCADE: when the owning account is
--                              GDPR-deleted (§17 G2), the install
--                              row goes with it. Distinct from the
--                              apps.github_install_account_id
--                              (PR-B's 00050) shape — that one is
--                              ON DELETE SET NULL because the
--                              repo/app link outlives the user; this
--                              one is the user's own install and
--                              has no survivor.
--   * installation_id       — GitHub's int64. Same value apps.id
--                              was carrying on the (account_id,
--                              repo) side via PR-B.
--   * default_branch        — captured at the OAuth handshake so
--                              the dashboard's bind picker doesn't
--                              need to re-fetch from /installation.
--   * sealed_install_token  — age-armoured install token ("ghs_…"
--                              form). githubd mints via
--                              AppAuth.ExchangeInstallationToken
--                              and seals with pkg/secretbox.SealOne
--                              before persisting. Plaintext never
--                              touches the database.
--   * token_expires_at      — when the sealed token expires
--                              (GitHub's install tokens are 1 h).
--                              Cold-start readers rehydrate via
--                              pkg/secretbox.Open only when
--                              expires_at > now() + 30s; otherwise
--                              they re-mint + re-seal.
--   * sealed_at             — when this row's sealed blob was
--                              last written. Telemetry only (rotation
--                              cadence).
--   * audit_github_login    — §11 paper trail on the durable row:
--                              the GitHub login who owns the
--                              install at seal time. githubd reads
--                              this on cold start to re-verify the
--                              session envelope's expected_login
--                              against the durable record, closing
--                              the audit gap if the session envelope
--                              ever drifts.
--
-- The login_idx supports the §11 cold-start re-verify path that
-- scans by login rather than by account_id (e.g. when an admin
-- needs to enumerate all installs claimed by a specific GitHub
-- user). Single-column index keeps the scan off the heap.
--
-- All DDL uses IF NOT EXISTS so re-applying this migration against
-- a box that already has the table is idempotent — same recipe as
-- 00050 (MEMORY.md/cd-deploy-pre-existing-migration-failure.md).

create table if not exists github_installations (
    account_id            uuid        primary key
                                       references accounts(id) on delete cascade,
    installation_id       bigint      not null,
    default_branch        text        not null,
    sealed_install_token  bytea       not null,
    token_expires_at      timestamptz not null,
    sealed_at             timestamptz not null default now(),
    audit_github_login    text        not null
);

create index if not exists github_installations_login_idx
    on github_installations (audit_github_login);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
drop index if exists github_installations_login_idx;
drop table if exists github_installations;
-- +goose StatementEnd