-- +goose Up
-- +goose StatementBegin

-- filename: 00074_projects_and_workloads.sql
--
-- 00074_projects_and_workloads.sql — ADR-050, Phase 1 (schema + project object).
--
-- Lands the foundation every later phase reads from. No scanner, no CLI,
-- no API endpoint — just the projects table, five new columns on apps, the
-- (account_id, slug) unique that lets multi-workload repos be expressed,
-- and the backfill that converts existing bound apps into one-member
-- projects. Standalone apps are unaffected: apps.project_id is nullable
-- and apps.workload_name defaults to ''.
--
-- Companion: docs/adr/050-repo-decomposition-and-project-object.md
-- and docs/repo_decomposition_implementation.md §2.
--
-- Replay-safety (the contract `migrations/replay_safety_test.go`
-- asserts): every DDL is `IF NOT EXISTS` / DO-block-guarded /
-- idempotent INSERT. The schema below re-applies cleanly against a
-- box that has the schema applied but no goose ledger row (the
-- drift mode documented at migrations/replay_safety_test.go:1-30).

create table if not exists projects (
    id                uuid primary key default gen_random_uuid(),
    account_id        uuid not null references accounts(id) on delete cascade,
    slug              text not null,
    repo_full_name    text,
    production_branch text,
    install_id        bigint,
    scan_source       text not null default 'unknown',
    created_at        timestamptz not null default now(),
    updated_at        timestamptz not null default now(),
    constraint projects_slug_shape check
        (slug ~ '^[a-z0-9][a-z0-9-]{0,62}$'),
    constraint projects_scan_source_chk check (scan_source in
        ('compose','procfile','k8s','render','fly','serverless',
         'workspace','convention','single','unknown')),
    constraint projects_account_slug_uniq unique (account_id, slug)
);

-- Pushed up by githubd's dispatch; one project per install+repo. The
-- partial-WHERE lets standalone (non-bound) projects share the table
-- without an `install_id` and without colliding on (0, '').
create unique index if not exists projects_install_repo_uniq
  on projects (install_id, repo_full_name)
  where install_id is not null and repo_full_name is not null;

-- apps gains 5 columns. All idempotent.
--
-- project_id: nullable, FK ON DELETE SET NULL so a project's hard
--   delete (Phase 5) orphans apps rather than cascading. ON DELETE
--   SET NULL is the contract that lets an operator "reinstall" a
--   repo without nuking every deployed app.
--
-- root_dir: defaults to '' (repo root). Standalone apps keep deploying
--   with root_dir=''.
--
-- workload_name: defaults to '' (unbound / standalone). The unique
--   constraint on (project_id, workload_name) is partial — only fires
--   for bound apps. A root Procfile with `web:` and `worker:` is one
--   directory and two workloads; the constraint is keyed by name, not
--   root_dir, which is what makes that shape expressible.
--
-- workload_class: defaulted to 'http' so a customer who never scans
--   or probes a workload lands on the most permissive default. The
--   CHECK constraint below mirrors WorkloadClass in pkg/state/types.go.
--   Phase 2 (reposcan) overwrites this with a hint; Phase 4
--   (characterization boot, ADR-051) re-derives from observation.
--
-- start_command: optional explicit override. NULL = fall back to the
--   OCI image's CMD, the existing behaviour.
alter table apps
  add column if not exists project_id     uuid references projects(id) on delete set null,
  add column if not exists root_dir       text not null default '',
  add column if not exists workload_name  text not null default '',
  add column if not exists workload_class text not null default 'http',
  add column if not exists start_command  text;

-- CHECK on apps.workload_class. DO-block-guarded so a drifted box
-- (constraint present, goose row missing) re-applies cleanly without
-- tripping SQLSTATE 42710 on the second `ADD CONSTRAINT`. Mirrors the
-- shape used by 00053_deployments_source_url.sql:57-70.
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_catalog.pg_constraint
        WHERE conname = 'apps_workload_class_chk'
          AND conrelid = 'apps'::regclass
    ) THEN
        ALTER TABLE apps
            ADD CONSTRAINT apps_workload_class_chk
                CHECK (workload_class IN ('http','graphql','grpc','job','worker'));
    END IF;
END$$;

-- Partial unique on (project_id, workload_name). One workload name per
-- project. Deliberately not (project_id, root_dir): see the comment in
-- docs/repo_decomposition_implementation.md:124-128.
create unique index if not exists apps_project_workload_uniq
  on apps (project_id, workload_name)
  where project_id is not null;

-- The original (install_id, repo_full_name) 1:1 binding on apps is
-- superseded by projects_install_repo_uniq. Dropping this index is
-- what unlocks multi-workload repos: before this migration the
-- schema asserted exactly one app per (install, repo), which is the
-- structural blocker Phase 5 reconcile removes.
--
-- The push-dispatch index apps_github_install_repo_branch_idx
-- (migrations/00050_github_bindings_account.sql:61) survives.
drop index if exists apps_github_install_repo_uniq;

-- Single-source the scan_source ranking. pg_tier_rank(text) returns
-- the monotonic-upgrade rank used by SetProjectScanSource (Phase 5
-- reconcile). The rank table must mirror ScanSource in
-- pkg/state/types.go: compose=8 (the strongest tier discovered),
-- procfile=6, k8s=8, render=8, fly=8, serverless=8, workspace=4,
-- convention=2, single=1, unknown=0. CREATE OR REPLACE makes the
-- function block idempotent — the box can re-apply on a drifted
-- schema without touching the body if the rank table doesn't change.
-- Mirror in Go lives in pkg/state/types.go:tierRank.
CREATE OR REPLACE FUNCTION pg_tier_rank(tier text) RETURNS int AS $$
    SELECT CASE tier
        WHEN 'compose'    THEN 8
        WHEN 'k8s'        THEN 8
        WHEN 'render'     THEN 8
        WHEN 'fly'        THEN 8
        WHEN 'serverless' THEN 8
        WHEN 'procfile'   THEN 6
        WHEN 'workspace'  THEN 4
        WHEN 'convention' THEN 2
        WHEN 'single'     THEN 1
        ELSE 0
    END
$$ LANGUAGE sql IMMUTABLE;

-- Backfill: one project per (install_id, repo_full_name), then
-- stamp the bound apps' project_id + workload_name.
--
-- Slug-shape guard: apps.slug has historically allowed broader
-- characters than the strict projects-slug regex
-- (`^[a-z0-9][a-z0-9-]{0,62}$`). The WHERE clause below filters to
-- apps whose slug already matches. Apps with historical outlier
-- slugs (uppercase, dots, longer) stay in the standalone path —
-- project_id NULL, workload_name = '' — until an operator renames
-- them via RenameApp. The carve-out is documented here, not
-- silently swallowed: the count is observable from
-- `select count(*) from apps where github_install_id is not null
--   and github_repo_full_name is not null and project_id is null`.
--
-- `on conflict (account_id, slug) do nothing` plus the partial
-- unique on (install_id, repo_full_name) keeps re-runs idempotent:
-- a second pass is a no-op.
insert into projects
    (account_id, slug, repo_full_name, production_branch, install_id, scan_source)
select distinct on (a.github_install_id, a.github_repo_full_name)
       a.account_id, a.slug, a.github_repo_full_name,
       a.github_production_branch, a.github_install_id, 'single'
from apps a
where a.github_repo_full_name is not null
  and a.github_install_id is not null
  and a.slug ~ '^[a-z0-9][a-z0-9-]{0,62}$'
on conflict (account_id, slug) do nothing;

-- Stamp the app-side FK + workload_name from the synthesized
-- projects. workload_name defaults to the app's existing slug so
-- the URL contract is preserved bit-for-bit: existing customers'
-- `{slug}.apps.DOMAIN` URLs keep routing to the same app row.
update apps a
   set project_id    = p.id,
       workload_name = a.slug
  from projects p
 where p.install_id       = a.github_install_id
   and p.repo_full_name   = a.github_repo_full_name
   and a.project_id is null;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
-- Down mirrors up; restore the dropped index so a rolled-back box
-- keeps the pre-00074 invariant that the schema could enforce
-- (no second app per (install, repo)).
alter table apps drop constraint if exists apps_workload_class_chk;
drop index if exists apps_project_workload_uniq;
alter table apps
  drop column if exists start_command,
  drop column if exists workload_class,
  drop column if exists workload_name,
  drop column if exists root_dir,
  drop column if exists project_id;
drop index if exists projects_install_repo_uniq;
drop table if exists projects;
drop function if exists pg_tier_rank(text);
create unique index if not exists apps_github_install_repo_uniq
  on apps (github_install_id, github_repo_full_name)
  where github_install_id is not null and github_repo_full_name is not null;
-- +goose StatementEnd
