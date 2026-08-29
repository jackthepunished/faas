# Affected workload preview & `--exclude`

The affected-workload preview surfaces what a `gregale deploy` will
do to every existing app in your account before it runs, so you can
catch destructive changes (a missing workload in your repo = a
soft-deleted app) and per-deploy exclusions (skip a workload you
don't want to deploy yet) without leaving the CLI.

This is the customer-facing reference. The architecture + ADR are
in [`docs/adr/124-affected-workload-preview-and-exclude.md`](adr/124-affected-workload-preview-and-exclude.md)
and [`docs/repo_decomposition_implementation.md` §7](repo_decomposition_implementation.md#7-follow-on).

## What the partition means

Every scan / apply returns four arrays that together describe
every existing app in your account plus what the scan proposes:

| Array        | What it contains                                                                                  |
|--------------|---------------------------------------------------------------------------------------------------|
| `will_deploy` | Apps that are in the scan set (post-`--only`/post-`--exclude`). New apps = `action: create`; changed = `update`; unchanged = `noop`. |
| `unaffected`  | Apps that exist in your account but are NOT in the scan set. They will NOT be touched by this deploy. |
| `skipped`     | Workloads in the scan that you `--exclude`'d. They will NOT be deployed this run.                 |
| `removed`     | Existing apps (project-scoped) whose `(root_dir, name)` is missing from the scan. They WILL be soft-deleted on apply. |

The partition is the operator's "show me what this deploy does
before I confirm" view. The fields are documented in
[`api/openapi.yaml`](../../api/openapi.yaml) (search
`PlanResponse`).

## When to use `--exclude`

Use `--exclude` when:

- **You're shipping an in-progress refactor.** A workload in your
  repo isn't ready to ship; you want the rest of the deploy to
  proceed without it.
- **You're testing a partial rollout.** You've split a monolith
  into two workloads; you want to deploy the new one without
  removing the old one yet.
- **You want the gate-rescue behavior.** On Free / Hobby plans,
  `--exclude` is one of the workarounds when the scan would push
  you over the plan cap (see [Gate rescue](#gate-rescue)).

```bash
# Skip two workloads on this deploy only.
gregale scan --tarball=./my-app.tgz --project-slug=demo --exclude=risky-migration,legacy-job

# Apply with the same exclusions.
gregale deploy --tarball=./my-app.tgz --project-slug=demo --exclude=risky-migration,legacy-job
```

The exclusion is **per-deploy** — it applies to this scan/apply
cycle only. To persist it across deploys, use `--persist-exclude`
(see [Persistent exclusions](#persistent-exclusions)).

## When to use `--show-affected`

Use `--show-affected` when you want to see the full partition
before confirming:

```bash
gregale scan --tarball=./my-app.tgz --project-slug=demo --show-affected
```

The default render is terse (workloads + can_apply line).
`--show-affected` adds the four-quadrant partition so you can
visually confirm "this deploy will deploy X, leave Y alone, skip
Z, and soft-delete W".

## Caveats

### Destructive warning

If your `--exclude` produces a destructive subset (i.e. some
existing apps are absent from the scan because you excluded them),
`printPlanText` prints a warning before the apply prompt:

```
! Applying will soft-delete 2 app(s) not present in the scan.
  Run with --show-affected to see exactly which apps.
```

The warning is suppressed under `--show-affected` (you already
see the partition) and under `--json` (machine-readable output).

### `--only` / `--exclude` mutex

`--only` and `--exclude` cannot share a workload slug. If they
overlap, the request is rejected pre-scan with HTTP 409 and code
`exclude_only_overlap`:

```bash
$ gregale deploy --tarball=./x.tgz --only=api --exclude=api
Error: --only and --exclude share workload(s): api
```

### `exclude_unknown_slug`

Slugs passed to `--exclude` must exist in the scan. Unknown slugs
trip HTTP 400 with code `exclude_unknown_slug`:

```bash
$ gregale deploy --tarball=./x.tgz --exclude=does-not-exist
Error: exclude slug is not a workload in this commit; unknown: does-not-exist
```

This is a defence-in-depth check; the apply path filters
excluded workloads out of `filteredW` *before* reconcile runs, so
a typo on `--exclude` cannot accidentally soft-delete an
existing app.

### Gate rescue

On Free / Hobby plans, if the pre-exclude gate would block the
deploy (apps or crons over the plan cap), `--exclude` can rescue
the gate when the exclusion shrinks the workload set below the
cap. The server stamps two wire fields on the response:

- `gate_rescued_by_exclude: true` — `--exclude` flipped a blocked
  gate to allowed.
- `can_apply_reasons: [...]` — the pre-exclude reasons (why the
  gate would have blocked).

The CLI surfaces this as a `Gate rescued by --exclude (pre-exclude
would have blocked); reasons: plan_apps_over_limit` line on both
the scan and the apply response.

If the gate is blocked pre-exclude AND post-exclude did not
rescue, the apply returns HTTP 403 with code `plan_gate_blocked`.

## Persistent exclusions

`--persist-exclude` records each `--exclude` slug into the
`deployment_scope_exclusions` table. A subsequent
`gregale deploy` without `--exclude` honors the persisted set
automatically (the operator's "I excluded this for the long haul"
intent).

```bash
# First deploy: persist the exclusion.
gregale deploy --tarball=./x.tgz --project-slug=demo \
    --exclude=risky-migration --persist-exclude

# Subsequent deploys: no --exclude needed; risky-migration stays
# excluded automatically.
gregale deploy --tarball=./x.tgz --project-slug=demo
```

The apply response surfaces the carried-forward set via
`persisted_exclusions: ["risky-migration"]`; the dashboard and
the audit log (kind=`project.scope.excluded`) make the
carry-forward observable for SOC 2 CC7.2 paper trail.

Persisted exclusions are scoped to `(account, project)` and live
for 90 days (reaped by the janitor after that boundary). The
audit log keeps the durable record.

## Wire shapes

### `PlanResponse` (preview)

```json
{
  "project_slug": "demo",
  "scan_source": "compose",
  "tier": "compose",
  "can_apply": true,
  "gate_rescued_by_exclude": false,
  "can_apply_reasons": [],
  "will_deploy": [
    {"name": "api",    "root_dir": "",       "class": "http", "action": "create"},
    {"name": "worker", "root_dir": "",       "class": "http", "action": "update"}
  ],
  "unaffected": [
    {"name": "legacy-job", "root_dir": "jobs/legacy", "class": "http", "action": "noop", "id": "0123456789abcdef0123456789abcdef"}
  ],
  "skipped":  [],
  "removed":  []
}
```

### `ApplyResponse` (apply)

```json
{
  "project_id": "0123456789abcdef0123456789abcdef",
  "apps": [
    {"slug": "api",    "id": "0123456789abcdef0123456789abcdef"},
    {"slug": "worker", "id": "0123456789abcdef0123456789abcdef"}
  ],
  "scan_source": "compose",
  "can_apply": true,
  "gate_rescued_by_exclude": false,
  "will_deploy": [{"name": "api", "action": "create"}],
  "unaffected": [],
  "skipped":    [],
  "removed":    [],
  "persisted_exclusions": []
}
```

## Related

- [`docs/adr/124-affected-workload-preview-and-exclude.md`](adr/124-affected-workload-preview-and-exclude.md) — ADR with the closed-set partition rules
- [`docs/repo_decomposition_implementation.md` §7](repo_decomposition_implementation.md#7-follow-on) — follow-on notes
- [`api/openapi.yaml`](../../api/openapi.yaml) — wire shape source of truth (search `PlanResponse`, `ApplyResponse`)