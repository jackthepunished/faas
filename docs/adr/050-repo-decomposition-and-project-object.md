# ADR-050 · Repo decomposition: `projects` object + multi-workload auto-provision

- **Status:** proposed
- **Date:** 2026-07-29
- **Decision:** Introduce a first-class **`projects`** object (one project =
  one `(install_id, repo_full_name)` pair) and make `apps` members of a
  project, each pinned to a `root_dir` + `workload_name` + `workload_class`.
  Move the `(install_id, repo_full_name)` uniqueness constraint **off `apps`
  and onto `projects`** — this is the single change that makes one-repo →
  N-workloads expressible. Add `pkg/reposcan`, a pure language-independent
  scanner that reads the *declarative* workload sources already present in a
  repo (compose / Procfile / k8s / render / workspace manifests / directory
  convention) and emits a candidate workload list. `faas deploy` renders that
  list as a table, takes **one confirmation**, and provisions the whole set —
  project, apps, and cron rows — in a single transaction. Push dispatch fans
  out across project members, **path-filtered by `root_dir`**.

  **The repo is the source of truth, continuously.** Every push to the
  production branch re-scans and *reconciles* the project to what the repo now
  declares: a workload that appeared is created, a workload that disappeared is
  removed, a workload whose command / dockerfile / schedule changed is updated.
  This runs unattended — there is no human at a webhook. Interactive
  `faas deploy` performs the same reconcile but renders it as a **diff** first
  (`+ added`, `~ changed`, `- removed`, unchanged) and applies it on one
  keypress.
- **Why:** Today the platform's data model asserts *one repo is one app*, and
  it asserts it in the schema, not in code:

  ```sql
  -- migrations/00007_github_binding.sql:34
  create unique index if not exists apps_github_install_repo_uniq
    on apps (github_install_id, github_repo_full_name)
  ```

  A second app bound to the same repo violates that index. There is also no
  `root_dir` / `subdir` / workspace column anywhere across the 98 existing
  migrations, so even if the index were dropped, two apps from one repo could
  not name which subdirectory each builds from. Every competitor treats "point
  at a repo, get all your services" as table stakes; we cannot express it at
  all. The forcing reason is structural, and no amount of CLI or builderd work
  routes around it.

  The **language-independence** requirement is satisfied by *never parsing
  customer source code*. Anyone who already runs six services has already
  described them to some other tool — `docker-compose.yml`, a `Procfile`, k8s
  manifests, a `render.yaml`. Reading those declarations is uniform across Go,
  Rust, Ruby, Java, Bun, Deno and anything released next year, and it requires
  no per-language parser and no registry that rots.

- **Consequences:**
  - **New migration `00074_projects_and_workloads.sql`.** Draft PR #428
    (extend-metering) claims slots 67, 68, 69, 70 and 73; main is at 66. Per
    ADR-041 this ADR's PR reserves 74 at open time and renumbers on rebase if
    74 is taken by then.
    - New `projects` table: `id`, `account_id` (FK, cascade), `slug`,
      `repo_full_name`, `production_branch`, `install_id`, `scan_source`,
      timestamps. `unique (account_id, slug)`; partial
      `unique (install_id, repo_full_name)`.
    - `apps` gains `project_id` (nullable FK), `root_dir` (default `''` =
      repo root), `workload_name`, `workload_class`, `start_command`.
    - `workload_class` CHECK: `http|graphql|grpc|job|worker` (every state
      column has a CHECK — CLAUDE.md conventions).
    - Partial `unique (project_id, workload_name)` — one workload name per
      project. Deliberately **not** `(project_id, root_dir)`: a `Procfile`
      with `web:` and `worker:` produces two workloads from one directory.
    - `drop index apps_github_install_repo_uniq` — superseded by the
      `projects` constraint.
    - Backfill: every app with a non-null `(github_install_id,
      github_repo_full_name)` gets a synthesized project (`scan_source =
      'single'`, project slug = app slug) and `project_id` + `workload_name`
      populated. Safe because the dropped unique index guarantees at most one
      app per `(install, repo)` today, so no project-slug collision is
      possible. Replay-safe per the 00053 convention: `IF NOT EXISTS`
      throughout, `on conflict do nothing` on the insert, idempotent `UPDATE`.
  - **New `pkg/reposcan`.** Pure: `Scan(fs.FS) (Result, error)`. No network,
    no exec, no Postgres — testable with `fstest.MapFS`, table-driven.
    Emits `[]Workload` (name, root dir, dockerfile, command, class hint,
    schedule, ports, env **keys only**, source provenance, tier) plus
    `[]Managed` and `[]Warning`. Four tiers, highest wins on conflict, lower
    tiers enrich empty fields only; output sorted by name so the confirm table
    and its golden tests are deterministic.
  - **Stateful services are surfaced, never provisioned.** A compose file
    naming `postgres` / `redis` / `mysql` / `mongo` / `clickhouse` /
    `rabbitmq` / `kafka` / … yields a `Managed` entry with an env hint
    (`DATABASE_URL`, `REDIS_URL`), not a workload. This is the existing
    stateless contract (`stateless_only_violation`, ADR-047, `docs/storage.md`,
    the `rest-api-postgres` template) applied at discovery time instead of at
    deploy-accept, so the customer learns it from a table rather than a 422.
  - **`k8s` `StatefulSet` is refused** for the same reason; `Deployment` →
    server class, `CronJob` → job class carrying `.spec.schedule`.
  - **Cron schedules become discoverable.** The schedule of a job cannot be
    observed at runtime — it is intent, not behaviour — but it is very often
    *declared*: `CronJob.spec.schedule`, `render.yaml` `cronJobs[].schedule`,
    `serverless.yml` `events[].schedule`. Discovered schedules create `crons`
    rows under the existing per-plan `cron_limit_per_app` /
    `cron_limit_per_account` ceilings in `pkg/api/limits.go`.
  - **Quota is evaluated before any write.** `len(Workloads)` is checked
    against `limits.For(plan).DeployedApps` minus existing apps. Over-quota is
    an RFC 7807 limit error carrying limit + observed + docs URL, and
    **nothing is created** — a partially-provisioned project is worse than a
    refusal. Free (`DeployedApps: 1`) cannot hold a multi-service repo, and
    the table says so before the prompt.
  - **Push dispatch changes shape.** `apps_github_install_repo_branch_idx`
    (00050) is a lookup that assumes one owning app; it becomes a project
    lookup plus a fan-out over members. Each member rebuilds only if a changed
    path falls under its `root_dir`; a change at repo root outside every
    member's `root_dir` (lockfile, root Dockerfile, CI config) rebuilds all.
    GitHub truncates the push payload's file list at 20 commits / 3000 files —
    on truncation, rebuild all.
  - **Reconcile is the push semantic, not append.** `Reconcile(project,
    scanResult)` diffs declared workloads against project members and emits
    `create` / `update` / `remove` actions. Three guards make an unattended
    destructive reconcile safe, and each is a correctness rule rather than a
    hedge:
    1. **Never reconcile to empty.** A scan returning zero workloads is a
       *failed scan* (compose file renamed, corrupted, moved behind a build
       step), not an instruction to delete the project. Zero-workload results
       abort the reconcile and raise an alert; they never remove.
    2. **Production branch only.** A push to a feature branch never mutates
       project membership. Reconcile keys off
       `projects.production_branch`.
    3. **Scan-source stability.** If the tier that produced the previous scan
       disappears (project was Tier-1 compose, now only Tier-3 convention
       matches), abort and alert rather than silently re-deriving the whole
       project from a weaker signal.
  - **Removal is a real removal, and it takes dependent state with it.**
    Removing a workload deletes its app row, and with it that app's
    `app_envs` rows, custom-domain bindings, and `crons` rows. This is the
    stated intent — the repo is truth — and it is recorded: every reconcile
    action emits an audit event (`project.workload.added`,
    `project.workload.removed`, `project.workload.changed`) via
    `pkg/audit.Auditor.Emit`, carrying the triggering commit SHA. Removal is
    the one action a customer cannot undo by reverting a commit — the app
    comes back, its secrets do not.
  - **Quota is re-checked on every reconcile, not just first provision.** A
    push that adds a sixth service to a Hobby project (`DeployedApps: 5`)
    applies the removals and updates, skips the creates, and reports the limit
    problem. A push must never leave the project half-applied in a way that
    depends on member ordering, so creates are evaluated as a set.
  - **Builder throughput becomes a first-class concern.** Builder slots are 1
    guaranteed + 1 opportunistic (never outranking tenant wakes). A six-member
    project serializes across those slots, so provisioning enqueues rather
    than fanning out unbounded, and the CLI reports queue position.
  - **`workload_class` starts as a scan hint and is later corrected by the
    probe boot** (Phase 4, `docs/repo_decomposition_implementation.md`). The
    probe touches `guest/init` and `pkg/fcvm` readiness and therefore
    **requires its own ADR-051 before code**, written at that gate per the
    `docs/scale_out_and_workload_classes.md` convention.
  - `worker` class is exempt from idle reaping — parking a queue consumer
    breaks it. This is the `service` workload class that
    `docs/scale_out_and_workload_classes.md` D4 anticipated, arrived at by
    discovery rather than declaration.
  - New CLI surface: `faas scan` (dry-run, provisions nothing), `faas deploy`
    gains `--yes` (CI), `--json` (machine-readable plan), `--only <name>`.
  - Standalone apps keep working unchanged: `project_id` is nullable and
    `root_dir` defaults to `''`.

- **Rejected alternatives:**
  - **Group by repo, no `projects` object.** Derive membership from
    `apps.github_repo_full_name`. Cheaper — no new table — but there is
    nowhere to hang project-level state (scan source, production branch,
    install id, future project-level env), the repo string becomes a de-facto
    primary key, and renaming a repo orphans the group. Rejected: the grouping
    is a real entity with its own lifecycle.
  - **Parse customer source per language** (AST-walk for `app.listen`,
    `@app.route`, decorators). This is the "language problem" restated — a
    parser per language, per framework, per version, permanently behind. It is
    also the only approach that breaks on a language we have never seen.
    Rejected outright; reading existing declarative config is uniform and
    complete for the repos that actually have multiple workloads.
  - **Require a `faas.yaml` manifest enumerating workloads.** Trivial to
    implement and completely defeats the goal: the customer does the
    discovery. Rejected as the *primary* path; an override file remains open
    as a later escape hatch for repos with no declarative source at all.
  - **Probe-only discovery (no static scan).** Runtime observation classifies
    a workload beautifully but cannot *enumerate* — you cannot boot what you
    have not yet identified. Static scan and probe are complementary: scan
    answers "how many and where", probe answers "what is each one".
  - **Report drift on push and wait for confirmation** (append-only: create
    nothing, remove nothing, just flag that the repo and the project diverged).
    Safer, and wrong: it means the deployed set silently stops matching the
    repo, so the customer has two sources of truth and a permanent backlog of
    "confirm this drift" chores. The whole promise is that the repo *is* the
    deployment. Rejected — the safety is recovered instead through the three
    reconcile guards (never-reconcile-to-empty, production-branch-only,
    scan-source stability), which block the failure modes that made
    auto-removal frightening without reintroducing a confirmation queue.
  - **Soft-delete removed workloads (park + retain for N days).** Tempting
    because removal destroys env vars and domain bindings. Rejected as the
    default: a parked-but-billed-nothing ghost app that still holds its slug,
    its domain, and a quota slot is a worse surprise than a clean removal, and
    it makes `DeployedApps` accounting ambiguous. The audit trail plus the
    interactive `-` diff line is the mitigation.
  - **Deploy Tier-3 (convention) guesses without confirmation.** Maximally
    automatic and the most likely to create five wrong apps on a customer's
    account and consume their plan quota. The confirmation table costs one
    keypress and makes every tier safe to guess from.
  - **`unique (project_id, root_dir)` instead of `(project_id,
    workload_name)`.** Rejected: a root `Procfile` with `web:` and `worker:`
    is one directory and two workloads, and it is one of the most common
    shapes in the wild.
  - **Deploy arbitrary prebuilt images named in a compose `image:` key.**
    Trips the two-drive `FROM`-base constraint (`pkg/oci/image.go`); arbitrary
    base images need content-addressed base sharing, a separate ADR
    (`docs/scale_out_and_workload_classes.md`, re-evaluation triggers).
    Non-datastore `image:`-only services are skipped with a warning.

## Cross-references

- Blocking constraint: `migrations/00007_github_binding.sql:34`
  (`apps_github_install_repo_uniq`), `migrations/00050_github_bindings_account.sql:61`
  (push-dispatch index).
- Spec §4.5/§9 (build pipeline), §6 (state machine), §14 (milestone gates),
  §17 G13 (stateless-only).
- ADR-003 (builds run inside ephemeral builder microVMs — the slot budget this
  ADR's fan-out competes for), ADR-005 (cold boot always works), ADR-012
  (githubd / push-to-deploy), ADR-041 (migration slot reservation), ADR-047
  (stateless runtime advisory — the contract `Managed` entries enforce at
  discovery time).
- `docs/scale_out_and_workload_classes.md` D4 (`service` workload class),
  `docs/storage.md` (managed-service pattern), `pkg/api/limits.go`
  (`DeployedApps`, `cron_limit_per_app`, `cron_limit_per_account`).
- Companion: `docs/repo_decomposition_implementation.md` (phased plan).
- Requires at its gate: **ADR-051** (probe-boot workload classification).
