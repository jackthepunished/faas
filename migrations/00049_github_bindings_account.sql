-- +goose Up
-- +goose StatementBegin

-- 00049_github_bindings_account.sql — PR-B (ADR-012 closure).
--
-- Until now the (account ↔ app ↔ installation ↔ repo, branch) binding
-- lived only in githubd's in-memory sync.Map (pkg/githubd/realservice.go).
-- The schema work shipped in 00007 captured (install_id, repo,
-- production_branch) on apps, but the dashboard's "this account owns
-- this install" relationship was reconstructable only by walking the
-- in-memory map, which evaporates on process restart. PR-B moves the
-- source of truth to Postgres:
--
--   * apps.github_install_binding_id   — deterministic id (e.g. the
--                                        "bind-<appID>-<repo>" form
--                                        RealService emitted pre-PR-B)
--                                        so the bind flow can be
--                                        idempotent on retry.
--   * apps.github_install_account_id   — FK to accounts.id. ON DELETE
--                                        SET NULL because account
--                                        deletion is the GDPR §17 path
--                                        and we must not delete the
--                                        app's repository link along
--                                        with the account.
--   * apps.github_install_linked_at    — when the bind was last
--                                        upserted (re-set on each
--                                        BindAppRepo call so the
--                                        dashboard's "connected on"
--                                        pill has a single source).
--
-- The pre-existing (install_id, repo) unique partial index from 00007
-- is unchanged — it's the install-↔-repo uniqueness invariant. The new
-- account-scoped unique partial index covers the (account, binding)
-- tuple which the dashboard list path needs cheap.
--
-- All DDL uses IF NOT EXISTS so re-applying this migration against a
-- box that already has the columns (e.g. a partial-applied deploy) is
-- idempotent — see the recipe in
-- MEMORY.md/cd-deploy-pre-existing-migration-failure.md.

alter table apps
  add column if not exists github_install_binding_id  text,
  add column if not exists github_install_account_id  uuid
    references accounts(id) on delete set null,
  add column if not exists github_install_linked_at   timestamptz;

create unique index if not exists apps_github_install_account_uniq
  on apps (github_install_account_id, github_install_binding_id)
  where github_install_account_id is not null
    and github_install_binding_id is not null;

create index if not exists apps_github_install_account_idx
  on apps (github_install_account_id)
  where github_install_account_id is not null;

-- Webhook dispatch lookup. githubd's push receiver joins on
-- (github_repo_full_name, github_production_branch) to find the
-- owning app; this partial index keeps the join off the heap for
-- repos that aren't yet connected (the partial WHERE mirrors the
-- join's null-exclusion).
create index if not exists apps_github_install_repo_branch_idx
  on apps (github_repo_full_name, github_production_branch)
  where github_repo_full_name is not null
    and github_production_branch is not null;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
drop index if exists apps_github_install_repo_branch_idx;
drop index if exists apps_github_install_account_idx;
drop index if exists apps_github_install_account_uniq;
alter table apps
  drop column if exists github_install_linked_at,
  drop column if exists github_install_account_id,
  drop column if exists github_install_binding_id;
-- +goose StatementEnd
