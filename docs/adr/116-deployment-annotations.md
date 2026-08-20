# ADR-116 · Deployment annotations (issue #977)

- **Status:** PR-cluster in design; mega-PR opens after this ADR merges.
- **Date:** 2026-08-18
- **Issue:** [#977](https://github.com/poyrazK/faas/issues/977)
- **Supersedes:** none.
- **Related:** ADR-038 (provenance / commit_sha + source_url, the
  precedent for stamping upstream git context onto the deployment row),
  ADR-048 (deployment.kind metering, the closed-set CHECK precedent),
  ADR-094 (event_kind dispatch — the trigger_source vocabulary mirrors
  this), ADR-091 (named-envs / per-deployment `scope`, the
  closed-set-on-text-column shape that `tag` reuses),
  spec §4.6 (deploy state machine, the only writer to `deployments`),
  spec §11 (audit trail, the `events` table that already carries
  {app_id, deployment_id, repo, ref, source_sha, install_id, supersedes}
  in its `data` jsonb — annotations extend this payload).

## Context

Today Gregale's deployment history tells the operator "a row was
inserted" but not **who**, **why**, or **from where**. The data is
captured at every wire boundary (git SHA on the CLI; pusher/ref/PR on
the githubd webhook; commit message on the GitHub Action) but is
deliberately dropped at `cmd/apid/githubd_bridge.go:268-275` (the
load-bearing comment that promises a `pusher` column "if/when a future
migration adds columns").

What we already have on the wire and the row:

- `deployments.commit_sha` (migration 00053, closed-set hex regex
  enforced by `deployments_commit_sha_shape_chk`).
- `deployments.source_url` (same migration).
- `deployments.kind` (`image | tarball | dockerfile | github |
  preview`, closed-set from migration 00085 + 00220).
- The audit row `events` carries `actor=<daemon>`, `kind=<deploy.*>`,
  `data={app_id, deployment_id, repo, ref, source_sha, install_id,
  supersedes}` — but it is not joined to the deployment DTO and the
  CLI does not surface it.

What we are missing:

- A **user-supplied annotation** on each deployment (free text +
  optional tag) that shows up in `gregale deployments` and on the
  dashboard so the operator can answer "what changed and why did we
  ship it" without grepping GitHub Actions logs.
- **Auto-captured provenance** (deployed_by, pr_number) so the audit
  trail is populated without operator effort.

Issue body requests exactly this:

> Automatically show:
> Deployed by: Poyraz
> Commit: fix checkout timeout
> Pull request: #142
> Reason: GitHub push
> Allow users to add:
> "Emergency rollback after payment provider incident."

## Decision

### D1 — Surface: 4 new columns on `deployments`

| Column        | Type   | Nullable | Closed-set / cap                              | Source                                    |
|---------------|--------|----------|-----------------------------------------------|-------------------------------------------|
| `reason`      | text   | yes      | `length(reason) <= 280`                       | CLI `--reason`, Action `reason:` input    |
| `tag`         | text   | yes      | `IN ('incident_recovery', 'hotfix', 'scheduled_maintenance', 'compliance_hold', 'partner_request')` | CLI `--tag`, Action `tag:` input |
| `deployed_by` | text   | yes      | —                                             | CLI auto-captures `git config user.name`; githubd stamps `pusher.name`; Action stamps `${{ github.actor }}`. Operator can override with `--deployed-by`. |
| `pr_number`   | int    | yes      | `> 0`                                         | githubd stamps `pull_request.number`; Action defaults to `${{ github.event.pull_request.number }}` when present. Push-to-main with no PR leaves NULL. |

Migration: `migrations/00315_deployments_annotation.sql`, slot 00315
(next free above 00314, post-PR #986 ADR-120 domain doctor merge). Replaces the `00288_reserve_slot.sql` fence
when the mega-PR ships.

The CHECK constraints follow the `00157_deployments_parked_reason.sql`
precedent (`DROP CONSTRAINT IF EXISTS` + `ADD CONSTRAINT`, the
`IF NOT EXISTS` guards on every ADD COLUMN to keep the migration
idempotent against SQLSTATE 42701 / 42710). The `length(reason) <= 280`
CHECK mirrors the closed-set `00053_deployments_source_url.sql` style
but with a simpler prose-accepts-anything shape (no regex needed).

### D2 — `reason` shape: free text, ≤280 chars

Free-form prose, not a closed enum. The literal example in the issue
body ("Emergency rollback after payment provider incident") is a
sentence, not a tag value — the closed-set alternative was considered
and rejected because it kills the prose use case.

A separate closed-set `tag` column is provided for grouping/filtering
(an operator can search `gregale deployments --tag incident_recovery`
or render a dashboard widget by tag without losing the free-form
prose).

Cap rationale: 280 chars is the tweet / one-line commit subject
standard. Long enough for a meaningful sentence, short enough that an
operator is forced to be concise.

### D3 — `tag` shape: optional closed-set enum

```
incident_recovery    | hotfix | scheduled_maintenance
compliance_hold      | partner_request
```

Mirrors the `parked_reason` precedent at migration 00157 — closed-set
vocabulary is enforced at the schema layer, not in the application
code. The CLI's mutex check at `cmd/gregale/commands2.go:885` rejects
unknown values before the wire is hit.

The set is small and pragmatic. Adding a new tag is a single migration
that drops + re-adds the CHECK (the same shape as
`migrations/00264_deployments_secret_findings.sql:58-65` widening
`deployments_scan_status_chk`).

### D4 — Auto-capture policy for `deployed_by`

The wire boundary always offers a free signal:

| Path    | Signal                                    | Operator can override |
|---------|-------------------------------------------|------------------------|
| CLI     | `git config user.name` (in-repo)          | `--deployed-by NAME`   |
| githubd | `PushEvent.Pusher.Name` / `Sender.Login`  | n/a (server-side)      |
| Action  | `${{ github.actor }}` (default input)     | `with: deployed-by:`   |
| Dashboard / API | — (no git context)                | caller-supplied        |

Never required. The DB column is nullable; the wire DTO carries it
`omitempty`. If no signal exists the column is NULL and the dashboard
renders no chip.

The denormalization decision is **stamp the human-readable name at
write time** (not a UUID that joins at read time) so deployment
history survives if a GitHub user is renamed/deleted. The audit row
continues to carry the daemon-name `actor="apid"` (per the existing
precedent at `cmd/apid/audit.go:43`) and the account UUID is in
`events.subject` — annotations do not change audit conventions.

### D5 — `pr_number` scope

Set only when the wire offers it free:

- githubd `pull_request` events (PR preview provisioning): the
  `Number` field on `PullRequestEvent` (`pkg/githubd/event.go:51`).
- GitHub Action: `${{ github.event.pull_request.number }}` (the Action
  input defaults to this when the event is a `pull_request`).
- API / dashboard / direct CLI source-tarball deploys: NULL (no
  inferred PR).

**Push events on a branch with an open PR do NOT trigger a GitHub API
lookup** to infer the PR number. This was considered and rejected
because:
1. Adds latency + a token call to every push event.
2. The lookup would race against PR open/close events on GitHub.
3. Customers can supply the PR via the Action path with the
   `pull_request` event if they want explicit linkage.

If customers ask for inferred PR numbers in a follow-up, ADR-118 will
scope it (likely a githubd-side cache + the existing
`api.github.com/repos/{repo}/pulls?head={branch}` query).

### D6 — Dashboard + CLI rendering policy

- **Dashboard detail page** (`/dashboard/apps/{slug}/deployments/{id}`)
  shows full reason text in a `<dl>` block under the header; tag as a
  badge; deployed_by as a `<code>` chip (mirroring
  `audit_events.html:91-102`); PR number as a link to GitHub when the
  parent app has a `repo_full_name`. Hidden when empty.
- **Dashboard list view** (app_detail.html) shows a single combined
  "Annotation" column: tag chip + truncated reason preview (40
  chars).
- **CLI history table** (`gregale deployments`) shows
  `id | app | status | commit | by | pr | tag | reason | created`.
  Truncate reason to 40 chars in the table; full text via
  `gregale deployment <id>`.
- **CLI single-fetch** (`gregale deployment <id>`) shows all 4 fields
  as `key: value` lines.

The CLI gets a `-w` / `--wide` flag that disables truncation and shows
the full reason column. (Today's `commands_deployments.go` has no
`-w`; the new flag is added in this PR.)

### D7 — Audit emit enrichment

`auditSourceRefDeploy` (`cmd/apid/handlers_source_ref.go:217-241`) and
`auditLocalTarballDeploy` (`cmd/apid/handlers_source_tarball.go:164-185`)
gain 4 new keys in their `data{}` payload:

```go
"reason":      req.Reason,
"tag":         req.Tag,
"deployed_by": req.DeployedBy,
"pr_number":   req.PRNumber,
```

The audit row's `actor` stays `"apid"` (the daemon convention; see
`cmd/apid/audit.go:43`). The audit pipeline does not become a source
of human attribution — annotations are the durable record on the
deployment row itself.

### D8 — Wire DTO changes

| DTO                            | New fields                                                |
|--------------------------------|-----------------------------------------------------------|
| `CreateDeploymentRequest`      | `Reason *string`, `Tag *string`, `DeployedBy *string`, `PRNumber *int` (all `omitempty`) |
| `DeploymentResponse`           | `Reason string`, `Tag string`, `DeployedBy string`, `PRNumber int` (all `omitempty`) |
| `SourceTarballDeployRequest`   | same 4 fields, `omitempty`                                |
| `SourceRefDeployRequest`       | same 4 fields, `omitempty` (the GitHub Action path)       |

`omitempty` is required for backwards compatibility — pre-feature
deployments return their old wire shape with all 4 new fields
absent. No wire-version bump.

### D9 — Out of scope (non-goals)

- Editing annotations after the fact (`UpdateDeploymentRequest`). The
  column is stamp-on-create only; the operator is expected to ship a
  new deployment with a corrected reason if they typo'd.
- Slack / email notifications when an annotation contains keywords.
  Issue body does not request it; punt to ADR-118 if customers ask.
- Linking annotations across deployments ("reverts #dpl_xyz"). Punt.
- Free-form tags / labels beyond the closed `tag` enum. The `reason`
  column covers the prose use case; if customers want arbitrary
  key/value metadata, that's a follow-up that adds a `jsonb metadata`
  column.
- PR-number inference for push-to-main via the GitHub API. D5.
- The preview-deploy wire path (per the comment at
  `pkg/githubd/service.go:715-718` "preview deploy is tracked
  separately per ADR-095 §'open issues'"). The annotations land
  first; the preview-deploy wire that consumes them lands later in
  its own PR-cluster.
- No new plan/limit entry in `pkg/api/limits.go`. Annotations are
  free-form metadata, not quotas.

## Migration shape

```sql
-- migrations/00315_deployments_annotation.sql
ALTER TABLE deployments
    ADD COLUMN IF NOT EXISTS reason      text,
    ADD COLUMN IF NOT EXISTS tag         text,
    ADD COLUMN IF NOT EXISTS deployed_by text,
    ADD COLUMN IF NOT EXISTS pr_number   int;

ALTER TABLE deployments
    DROP CONSTRAINT IF EXISTS deployments_reason_len_chk;
ALTER TABLE deployments
    ADD  CONSTRAINT deployments_reason_len_chk
        CHECK (reason IS NULL OR length(reason) <= 280);

ALTER TABLE deployments
    DROP CONSTRAINT IF EXISTS deployments_tag_set_chk;
ALTER TABLE deployments
    ADD  CONSTRAINT deployments_tag_set_chk
        CHECK (tag IS NULL OR tag IN
               ('incident_recovery', 'hotfix', 'scheduled_maintenance',
                'compliance_hold', 'partner_request'));

ALTER TABLE deployments
    DROP CONSTRAINT IF EXISTS deployments_pr_number_positive_chk;
ALTER TABLE deployments
    ADD  CONSTRAINT deployments_pr_number_positive_chk
        CHECK (pr_number IS NULL OR pr_number > 0);
```

`reason` and `deployed_by` accept any printable text (including
newlines). `tag` is closed-set. `pr_number` is positive or NULL.

## Verification

The mega-PR passes when:
1. Migration replay-safety: `db.MigrateUp` called twice is a no-op
   the second time (SQLSTATE 42701/42710 vectors closed by
   `IF EXISTS` / `IF NOT EXISTS`).
2. Schema regen + sqlc clean: `make schema-dump && make sqlc-generate
   && git diff --exit-code schema.sql pkg/state/sqlc/` is empty.
3. Spec + SDK regen clean: `make spec-check && make sdk-gen &&
   make sdk-check` is empty.
4. Lint + gofmt: `make lint` clean on golangci-lint v2.4.0 and
   gofmt 1.25.12 strict.
5. Unit tests: `make test` race-clean; new pgstore roundtrip test +
   handler extension + githubd bridge test + CLI table-layout pin
   all pass.
6. e2e: `make e2e` with `e2eMigrationTarget=288` (bumped in
   `pkg/e2etest/harness.go`); new `signed_deploy_e2e_test.go` +
   `githubd_push_e2e_test.go` both green.
7. Manual smoke: `gregale deploy --reason "test" --tag hotfix
   --tarball x.tgz` against a local apid; `gregale deployments --wide`
   shows the row with all 4 fields. Dashboard renders the
   annotation block.
8. Backwards-compat: pre-feature deployments return their old wire
   shape with all 4 new fields `omitempty`-absent.

## Mega-PR shape

5 atomic commits + 1-2 review-fix follow-ups, each reviewable in ~6
min:

1. **Migration + state layer** — adds the 4 columns, extends the
   Deployment struct + 3 SELECT projections + scanDeploymentInto +
   INSERT, updates apply_walk_test.go, ships the migration _test.go.
   Includes `make schema-dump` + `make sqlc-generate` regen.
2. **DTO + OpenAPI + SDK regen** — adds the 4 fields to 4 DTOs +
   client forwarders + api/openapi.yaml + pkg/apid/openapi.yaml.
   Includes `make spec-check` + `make sdk-gen` + `make sdk-check`.
3. **CLI flags + auto-capture + table renderer** — adds `--reason`,
   `--tag`, `--deployed-by` flags to cmdDeployTarball; adds
   `gitConfigUser` / `gitUserName` helpers to git_local.go; wires
   zero-config path; extends commands_deployments.go table +
   single-fetch render; bumps cli_meta.go.
4. **apid handlers + audit emit + githubd bridge** — extends
   handlers_source_ref.go + handlers_source_tarball.go to read +
   stamp the 4 fields via apidsource.Enqueue; updates
   githubd_bridge.go:268-275 comment + passes Pusher→DeployedBy +
   EventKind→TriggerSource + Number→PRNumber; enriches the audit
   data{} maps.
5. **GitHub Action + dashboard** — adds the 4 inputs to action.yml,
   forwards via run.sh; adds 4 fields to dashboard.DeploymentItem +
   dashboardDeploymentItem helper; renders on
   deployment_detail.html:65 + app_detail.html:128.
6. **(commit 6) Tests + e2e + CI green-pack** — adds
   `TestPgStore_DeploymentAnnotationRoundtrip` + githubd bridge
   test + CLI layout pin + `signed_deploy_e2e_test.go` +
   `githubd_push_e2e_test.go`; bumps `e2eMigrationTarget` to 288.

Review-fix follow-ups (separate commits, addressed inline in review):
goofmt drift, spec-check spec-sync drift, sdk-coverage drift if any
new DTO field is missing a typed Client method.

PR template (`.github/pull_request_template.md`) `## Verification`
checklist gates every box.
