# ADR-124 · Affected-workloads preview with `--exclude`

- **Status:** Accepted (2026-08-24)
- **Date:** 2026-08-21
- **Scope:** `pkg/api/dto.go`, `cmd/apid/scan_service.go`, `cmd/gregale/{commands2.go,commands_decompose.go,commands_diff.go,client.go}` → `cmd/apid/handlers_dashboard.go`, `pkg/dashboard/{views,templates}/`.

## Context

Today Gregale can scan a tarball/repo and tell the operator which
**workloads** the project would create/update (`gregale scan` →
`PlanResponse.Workloads`), and the single-app `gregale deploy --diff`
can preview one app's deploy. There is **no mechanism that, given a
commit, lists the customer's existing apps that the commit will
redeploy, distinguishes them from apps it will leave alone, and lets
the operator drop one before applying**.

The natural ask — "this commit will deploy `payments-api`,
`invoice-worker`; `users-api` and `nightly-report` are unaffected" —
has no surface on the platform.

The closest existing surfaces are narrower:

- `pkg/deploydiff` — per-app `POST /v1/apps/{slug}/diff`. Single app,
  no notion of "apps in the rest of the account."
- `gregale deploy --only a,b,c` — allowlist on a multi-app apply. There
  is **no inverse `--exclude`**, and the response does not partition
  "to be touched" from "to be left alone."
- `gregale scan --show-affected` — does not exist; `printPlanText`
  emits one section (`Workloads`), server's `PlanResponse.Workloads`
  carries no `existing_app_id` field, and the only existing
  cross-reference between an account's apps and a scan's workloads is
  internal to `pkg/reconcile/diff.workloadDiff` (`pkg/reconcile/diff.go:59-129`).

Domain Doctor (ADR-120), Stages (ADR-117), and Annotations (ADR-116)
are unrelated: they cover single-deploy telemetry, DNS/Cert
observability, and per-deploy `tag/reason/pr_number` metadata,
respectively.

The architectural decision is: **where does this live**, and **what is
its identity?** Two reasonable placements.

### Why this is an ADR (not a follow-up issue)

The decision establishes three customer-visible behaviours that have
no precedent on the platform:

1. A new **partition shape** on `PlanResponse` — the wire gains
   `will_deploy`, `unaffected`, and `removed` slices that **touch
   every app in the account**, not just workloads discovered in the
   scan. Future consumers (SDKs, dashboards, the docs portal) will
   expect this shape.
2. A new **operator-facing flag** `--exclude` that mirrors `--only`
   but inverts its semantics. Like `--only`, it routes through the
   multipart `only` form field shape on the wire — but its semantics are
   "skip these workloads" not "only apply these," and that flips the
   trust model on the apply path (currently the scan-filter is the
   only gate; now the apply path must also reject the excluded slugs
   even if they re-enter via a stale scan).
3. A new **match key** wiring — the existing `(RootDir, Name)` key
   from `pkg/reposcan.Workload.Key()` (the same `workloadKey` struct
   used by `pkg/reconcile/diff.go:65-73`) becomes a wire-level
   guarantee that **client and server agree**. Without an ADR, the
   next surface that wants to project account-scoped state onto a
   scan would re-derive the match key and risk disagreement.

### Out of scope (explicit)

- **Server-side ref fetch.** apid does not hold GitHub install
  credentials; the dashboard preview form accepts a tarball upload
  in this PR. A future ADR can extend `scanService` to consume
  `?ref=…` once apid is allowed to reach githubd.
- **Persisting `exclude` history per project** — that requires a new
  table. Defer to a follow-up audit PR.
- **Cross-account preview.**
- **PR preview environments (ADR-095)** — orthogonal (per-PR
  ephemeral app rows).

## Decision

### 1. Wire DTO delta (`pkg/api/dto.go`)

Three new shapes, all in `pkg/api/dto.go`:

```go
type PlanAffectedApp struct {
    Slug         string `json:"slug"`
    ID           string `json:"id,omitempty"`           // empty iff Action == "create"
    Action       string `json:"action"`                 // "create" | "update" | "remove" | "noop"
    ExistingRoot string `json:"existing_root_dir,omitempty"`
}

type PlanWorkload struct {
    // ... existing fields unchanged ...
    Action        string `json:"action,omitempty"`
    ExistingAppID string `json:"existing_app_id,omitempty"`
}

type PlanResponse struct {
    // ... existing fields unchanged ...
    WillDeploy []PlanAffectedApp `json:"will_deploy"`
    Unaffected []PlanAffectedApp `json:"unaffected"`
    Removed    []string          `json:"removed,omitempty"`
}
```

`Action` is closed-vocabulary: `"create"` (scan workload, no existing
app), `"update"` (scan workload, existing app with same `(RootDir, Name)`),
`"remove"` (existing app, no scan workload, NOT protected by `--exclude`),
`"noop"` (existing app and scan workload match, but `--exclude` marks
the operator wants to skip it). The same vocabulary is reused on
`PlanWorkload.Action` for symmetry.

`Removed` is a flat `[]string` of slugs only (not `PlanAffectedApp`)
because removal is a one-way action — there is no per-row editable
metadata to surface. The dashboard renders the same shape.

### 2. Match key: `(RootDir, Name)`

The partition uses the same `workloadKey{RootDir, Name}` tuple that
`pkg/reconcile/diff.go:65-73` already builds. **The server is the only
authority on this mapping.** Clients MUST NOT match by `slug` alone —
two scans producing the same workload name but different `root_dir`
are NOT treated as the same existing app (e.g. a mono-repo
`services/api` vs `apps/api`). The wire DTO surfaces both `Name` and
`RootDir` per row for clarity.

### 3. Server: extend `scanService` in place

Extend `cmd/apid/scan_service.go:317` (`scanService`) to compute the
partition **after the existing workload filter at `:392-401`** but
**before `reconcileSvc.Reconcile` at `:751`**, so the apply path
cannot sneak an excluded slug past a stale scan filter. Implementation
notes:

- Use the `s.store.AppsForAccount(ctx, acct.ID)` query (filter
  `deleted_at IS NULL`); if not present, raw SQL via the same
  pgxpool the existing `store` exposes.
- Run `pkg/reconcile.diff.workloadDiff` semantics inline. **Do not
  fork**: project the same partition into `WillDeploy`/`Unaffected`/
  `Removed`.
- When `apply=true` AND `req.Exclude` non-empty: drop `reposcan.Workload`
  rows whose slug is in `excludeSet` before `reconcile.Reconcile`. The
  operator-side `WillDeploy[*].Action` for the excluded slugs becomes
  `"noop"`, and they appear in a new `Skipped []PlanAffectedApp` field
  on the response so the dashboard can render "excluded by operator."

  `pkg/reconcile.workloadDiff` itself takes the `exclude` set as a
  parameter (added in the post-#1026 audit fix). The handler threads
  `req.Exclude` through `Reconcile(..., excludeList)`; the engine
  builds `excludeSet[strings.ToLower(TrimSpace(name))]` and filters
  `existing` before the removes loop. Without this filter, an
  operator `--exclude=foo` for an EXISTING app foo would still
  emit a remove Action — `applyActions.applyRemove` would call
  `SoftDeleteAppCascade(foo)` and silently override the operator's
  intent. The post-#1065 audit pinned this contract at
  `pkg/reconcile/reconcile_test.go::TestReconcile_ExcludePreventsRemove`.

`Removed` is the **destructive** subset and is **project-scoped**
(the project being previewed) rather than account-scoped, while
`Unaffected` is account-scoped (blast-radius view). The shape
itself — `[]string` of slugs, not `[]PlanAffectedApp` — is fixed
in §1 (lines 113-115): removal is a one-way action with no
per-row editable metadata worth surfacing. The preview path stamps
`Removed` from the partition (project-scoped apps whose key isn't
in the scan set); the apply path stamps it from the canonical
`pkg/reconcile.diff.workloadDiff` result. The two paths must agree
on the set, or the operator sees one set pre-apply and a different
set on the apply response — a contradiction that would silently
break the trust contract. Pin at `TestPlanResponse_RemovedShape`
in `pkg/api/characterization_test.go`.

### 4. New request field: `exclude` (multipart)

Mirror the `only` form field (`pkg/api/client.go:947` and
`cmd/apid/scan_service.go:810-817`):

- Multipart field name: `exclude` (sibling of `only`).
- Lowercased + trimmed, same as `only`. Empty list means "no
  exclusion."
- Server rejects with `409 exclude_only_overlap` when
  `intersection(only, exclude) ≠ ∅`.
- Server rejects with `400 exclude_unknown_slug` when a slug in
  `exclude` is not present in the scan's workloads (case-insensitive
  name match). Unknown-slug is a *programming error* surface; the
  operator can fix it by removing the typo. Code is RFC 7807
  `code: "exclude_unknown_slug"`.

### 5. CLI: `--exclude` mirrors `--only`

- `cmd/gregale/commands2.go:806-807` — add `deployExclude := fs.String("exclude", "", …)`.
  Mutex with `--only` (overlap rejected before service call).
- `cmd/gregale/commands_decompose.go:48` — `--exclude` on `scan`.
  `--show-affected bool` (default false) renders the two-section
  table; default keeps the existing single-section plan.
- `printPlanText` (`:265`) — when `--show-affected`, render WillDeploy
  + Unaffected tables, strikethrough on locally-excluded slugs.
- `confirmPlan` (`:316`) — extend the prompt to
  "Apply N workloads (excluded: a, b)? [y/N]".
- `pkg/api/client.go:857,884` — `ScanProject`/`ApplyProjectPlan`
  accept `exclude []string` and write the multipart field at
  `:921-953`.

### 6. Dashboard: project-scoped preview page

Four new dashboard routes (handlers in `cmd/apid/handlers_dashboard.go`,
registered in `cmd/apid/server.go`):

| Route | Handler | Auth |
|---|---|---|
| `GET /dashboard/projects` | `projectList` | `ScopesReadSurface` |
| `GET /dashboard/projects/{slug}/preview` | `projectPreviewForm` | `ScopesReadSurface` |
| `POST /dashboard/projects/{slug}/preview` | `projectPreviewSubmit` | `ScopesReadSurface` |
| `POST /dashboard/projects/{slug}/preview/apply` | `projectPreviewApply` | `ScopesDeployWriteSurface` |

The preview form accepts a multipart `tarball` upload (cap 100 MB)
plus a text `exclude` field. The render path is `printPlanText` →
`pkg/dashboard/views/project_preview.go`, which renders two tables
(`Will deploy` with `<input type=checkbox name=exclude value=SLG>`
per row, `Unaffected` read-only). Form action posts back with the
union of checked boxes as `exclude`.

### 7. No DB migration

This PR is purely derived state — no new tables, no schema delta.
Skip `migrations/` and skip the slot fence dance entirely. A future
PR persisting `exclude` history (`deployment_scope_exclusions`
table) lands its own migration.

## Consequences

### Positive

- Operators see the **exact blast radius** of a commit before
  applying — WillDeploy + Unaffected, with `--exclude` to opt out of
  one row. Mirrors the user-facing claim in the dashboard mock.
- CLI and dashboard render the **same partition** — single source of
  truth in `scanService`.
- `(RootDir, Name)` match key is now a **wire guarantee** documented
  in `pkg/reposcan.Workload.Key()` and the `PlanWorkload` DTO. Future
  surfaces (PR preview, scheduled deploys) can reuse it.

### Negative / risks

- **Account-wide enumeration**: the new `PlanResponse.Unaffected`
  list enumerates every non-deleted app in the account. The endpoint
  is `authLimited` + `requireMFA` + `ScopesDeployWriteSurface`, same
  as the existing scan route, so the auth boundary does not change
  — but a future PR moving scan to a read-only scope MUST also gate
  `Unaffected` on that scope, or strip the field.
- **`Removed` is destructive**: a "remove" entry in `WillDeploy`
  triggers `SoftDeleteAppCascade` on the apply path. If the operator
  sees `Unaffected` and forgets `Removed`, they may be surprised by
  gone-by-end-of-deploy apps. The dashboard render will surface
  `Removed` as a third table with a distinct visual treatment.
- **`--exclude` is typed once**: a typo on the slug
  (`payments-apii`) returns `code:"exclude_unknown_slug"` and aborts
  the deploy. This is intentional — silent ignore would be worse.
- **`Skipped` field is new** on `PlanResponse`. SDKs that don't
  ignore unknown fields will see it appear; their CI runs will need
  a codegen refresh. Recorded in PR description.

### Operational

- No new metric names — the existing `gateway_wake_latency_seconds`
  / `snapshot_fleet_avg_mb` cluster is untouched. New operations:
  trace logs at `cmd/apid/scan_service.go` carry
  `logger.With("exclude_count", len(req.Exclude), "will_deploy", len(resp.WillDeploy))`
  so the design is observable in production.
- No new quota; no new limit table row. `pkg/api/limits.go` unchanged.

## References

- `pkg/reconcile/diff.go:59-129` — `workloadDiff`, the existing
  partition. Cite this as the single source of truth for the match
  key.
- `pkg/reposcan/scan.go:90` — `Workload.Key()` returns
  `workloadKey{RootDir, Name}`.
- `pkg/api/dto.go:3546-3610` — current `PlanWorkload` /
  `PlanResponse`.
- `cmd/apid/scan_service.go:317,759,810-817,883-895` — partition
  seam, multipart parser, plan-token minter.
- `pkg/api/client.go:857,884,921-953` — current `--only` thread.
- `cmd/gregale/commands2.go:806-807` — current `--only` flag.
- `cmd/gregale/commands_decompose.go:265,316,333` — current
  printPlanText / confirmPlan / planProblem.
- `cmd/apid/handlers_decompose.go:34,65` — `scanProject` /
  `applyProject`, both call `scanService`.
- `cmd/apid/server.go:990,1155-1156` — route registration.
- `pkg/dashboard/dashboard.go:36,905` — `Page` struct + `parseTemplates`.
- `pkg/dashboard/views/render.go:274` — `template.HTML` cast
  precedent (only relevant if new view ships a `template.HTML`).
- ADR-050 (repo decomposition + project object) — the wider context.
- ADR-095 (PR preview environments) — explicitly out of scope.

## Acceptance notes (2026-08-24)

ADR-124 ships via a 4-PR cluster:

- **PR #1065** (head `0b4cf07f4`, MERGEABLE CLEAN 2026-08-23) —
  initial surface; closed 5 ship-blockers + 4 code-review followup
  fixes for the affected-workloads preview.
- **PR-A** (rescue wire surface end-to-end + Prometheus counter) —
  closes caveat #1 (CLI didn't render rescue wire fields) and
  caveat #2 (no Prometheus metric for `plan_gate_rescued_by_exclude`).
  Adds `apid_plan_gate_rescued_by_exclude_total{plan,reason}` with
  12 pre-instantiated series; registers `--exclude` + `--show-affected`
  in the CLI manifest so `gregale man` / completion ship.
- **PR-B** (persistent `--exclude` history via
  `deployment_scope_exclusions` migration 00418) — closes caveat
  #3. `--persist-exclude` records slugs; subsequent deploys without
  `--exclude` honor the persisted set automatically. Audit kind
  `project.scope.excluded` per slug for SOC 2 CC7.2 paper trail.
- **PR-C** (e2e coverage for excluded workload path) — closes
  caveat #4. Extends `cmd/e2e/scan_project_exclude_e2e_test.go`
  and `cmd/e2e/scan_project_partition_e2e_test.go` to cover the
  brand-new excluded path end-to-end.
- **PR-D** (OpenAPI examples + customer-facing
  `docs/affected-workload-preview.md`) — closes caveat #5. ADR
  status flipped from Proposed to Accepted with this section.

ADR-127 (sister ADR for persistence, PR-B commit 6) is OPTIONAL;
the persistence design is captured in 00488_deployment_scope_exclusions.sql
header without a separate ADR file.
