# ADR-127 · Persistent deployment scope exclusions (`--persist-exclude`)

- **Status:** Accepted (2026-08-24)
- **Date:** 2026-08-24
- **Scope:** `migrations/00418_deployment_scope_exclusions.sql`,
  `pkg/state/{store.go,pgstore_deployment_scope_exclusions.go,memstore_deployment_scope_exclusions.go}`,
  `pkg/reconcile/audit.go`,
  `pkg/api/dto.go`,
  `cmd/apid/{scan_service.go,handlers_decompose.go}`,
  `cmd/gregale/{commands2.go,commands_decompose.go,cli_meta.go}`,
  `pkg/api/client.go`.

## Context

ADR-124 introduced the **per-deploy** `--exclude` flag — the operator's
"I want to skip these workloads this one time" surface, scoped to a
single scan/apply cycle. The 5 production caveats PR cluster
(PR-A..PR-D, 2026-08-23) addresses operator-observability gaps and
publishes a customer-facing doc; **caveat #3** explicitly asked for
persistent exclusion history scoped to `(account, project)`.

The natural ask is "I excluded `risky-migration` last week; I still
want it excluded today; please don't make me retype the slug every
deploy." Today, the only ways to honour that intent are:

1. **Operator discipline** — remember the slug and pass `--exclude`
   on every deploy. Brittle; survives only as long as the operator
   keeps typing it.
2. **Per-deploy exclude set on the request** — exactly what ADR-124
   already does; no persistence, no audit trail, no rollback.

There is no `(account, project)` persistent table; no audit kind for
"this slug was excluded by intent"; no `PersistedExclusions` field on
the apply response so the operator can verify the carry-forward
actually fired.

The architecture decision is: **how does persistent exclusion
interact with the existing per-deploy surface, and what are the FK /
lifecycle guarantees that the soft-delete-by-`status` UPDATE pattern
demands?**

### Why this is an ADR (not a follow-up issue)

Three precedents on this platform push the answer into an ADR:

1. **`status='deleted'` UPDATE is the lifecycle primitive.**
   `pkg/state/pgstore.go:3426` documents: `"Per Phase 5 user decision
   the cascade is status-only — child rows survive for slug-reuse"`.
   The platform never `DELETE`s an app row; it stamps
   `status='deleted'` and lets the janitor reap after a retention
   window. Any new table that hard-FKs `app_id` to `apps(id)` will
   leak orphans forever, because the CASCADE trigger only fires on
   row-level DELETE, not on UPDATE.
2. **Audit trail is a closed-vocabulary contract.** `pkg/reconcile/audit.go`
   exposes `Kind*` constants that downstream consumers (dashboard
   audit log, SOC 2 CC7.2 paper trail) key off of. Adding a new kind
   is a wire change, not a local refactor.
3. **The wire DTO is a stable contract.** `pkg/api/dto.go::PlanResponse`
   and `ApplyResponse` are SDK-regen-trigger fields. Adding
   `PersistedExclusions` requires an OpenAPI update + SDK regen + a
   semver bump note in the PR description.

### Out of scope (explicit)

- **Quota gate on `deployment_scope_exclusions`.** No cap today;
  re-evaluate after customer usage shows a need.
- **Dashboard UI for persisted exclusions.** The dashboard preview
  form already has a text `exclude` field; the `--persist-exclude`
  flag is CLI-only in this PR. Dashboard UI follow-up after CLI is
  validated.
- **Cross-account / cross-project exclude scoping.** ADR-124 is
  single-project; cross-project persistence is out of scope.

## Decision

### 1. New table `deployment_scope_exclusions`

Migration `migrations/00418_deployment_scope_exclusions.sql` (slot
00418; PR-B commit 1, fence at 00417).

```sql
CREATE TABLE deployment_scope_exclusions (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    account_id  uuid NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    project_id  uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    app_id      uuid NOT NULL,                  -- NO FK (see §2)
    slug        text NOT NULL,
    reason      text NOT NULL DEFAULT '',
    created_by  text NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now(),
    CHECK (slug = lower(slug)),
    CHECK (length(slug) > 0),
    UNIQUE (account_id, project_id, slug)
);

CREATE INDEX idx_dse_project_recent
    ON deployment_scope_exclusions (project_id)
    WHERE created_at > now() - interval '90 days';

CREATE INDEX idx_dse_account_recent
    ON deployment_scope_exclusions (account_id)
    WHERE created_at > now() - interval '90 days';
```

`set_updated_at` trigger on UPDATE (mirror of
`00384_mirror_rules.sql:101-112`).

### 2. NO FK to `apps(id)` — soft-delete lifecycle contract

`SoftDeleteAppCascade` is **status-only UPDATE**, not row DELETE
(`pkg/state/pgstore.go:3442-3444`). A row-level `ON DELETE CASCADE`
FK would never fire — orphans accumulate indefinitely.

**Mitigation:** `app_id` is a **snapshot reference** with no
referential integrity. The active invariant is
`UNIQUE (account_id, project_id, slug)`. When an app is soft-deleted,
the corresponding exclusion row is **NOT** cascade-deleted; the
operator's persisted intent survives the app's lifecycle. A janitor
`PurgeOrphanedScopeExclusions(ctx)` is added to the same janitor
sweep that runs `PurgeDeletedApps`; the sweep matches on `app_id` not
in the live `apps` table and is **bounded by the 90-day window** so
it self-clears.

This trade-off is documented in the migration's header comment
block.

### 3. 90-day retention window

The partial indexes at §1 are bound to a 90-day window —
`WHERE created_at > now() - interval '90 days'`. The window is the
operator's "I excluded this for the long haul" intent horizon; the
audit log (`kind=project.scope.excluded`) is the durable record
beyond the 90 days, so re-purging the active table is safe.

The retention window is encoded **only** in the partial-index
predicate (per-row semantics) — there is no scheduled DELETE job in
this PR. The 90-day bound is enforced by `LookupDeploymentScopeExclusions`
which always re-computes `now() - interval '90 days'` server-side;
rows older than that are silently ignored (the durable record is the
audit log).

### 4. New audit kind: `project.scope.excluded`

`pkg/reconcile/audit.go:34` — new constant next to `KindWorkloadSkipped`:

```go
KindProjectScopeExcluded = "project.scope.excluded"
```

Same naming shape (`project.<noun>.<verb>`). Emit on:

1. `--persist-exclude` ingest (the slug is now persisted; new audit
   row per slug).
2. Carry-forward apply (the slug was loaded from the persisted set;
   new audit row per slug with `data.reason = "persisted"`).

Data shape mirrors `KindWorkloadSkipped`:

```json
{
  "kind": "project.scope.excluded",
  "project_id": "…",
  "app_id": "…",
  "workload_name": "risky-migration",
  "reason": "persisted",
  "actor": "user@example.com"
}
```

### 5. New wire field `PersistedExclusions`

`pkg/api/dto.go` — extension to `PlanResponse`:

```go
type PlanResponse struct {
    // ... existing fields unchanged ...
    PersistedExclusions []string `json:"persisted_exclusions,omitempty"`
}
```

`ApplyResponse` embeds `PlanResponse`, so the field is inherited.
**This is a wire change — triggers SDK regen.**

`cmd/apid/scan_service.go` populated at apply-time:

- When `req.Exclude` is empty AND persisted exclusions exist for the
  project, merge persisted slugs into the engine call's `excludeList`.
- Set `PersistedExclusions = persistedSlugs` on the response so the
  operator can verify the carry-forward fired.
- Both carry-forward + per-deploy set are reflected in the `Skipped`
  partition (the existing wire field) so dashboards render the union.

### 6. CLI: `--persist-exclude` is opt-in

`cmd/gregale/commands2.go:806` (deploy) and
`commands_decompose.go:48` (scan) — new flag:

```bash
gregale deploy --tarball=./x.tgz --project-slug=demo \
    --exclude=risky-migration --persist-exclude
```

Default **OFF**. The flag is one-shot: it persists the current
`--exclude` slugs on a successful apply; on a failed apply, no
audit row is emitted and the table is unchanged. There is no
auto-promote to `--persist-exclude` for repeated exclusions — the
operator's intent is explicit.

Manifest entries at `cli_meta.go:349-391,654-663` document the flag
for `gregale man` + completion.

### 7. No quota gate

There is **no per-account cap** on `deployment_scope_exclusions`
rows today. The 90-day partial-index + janitor self-clear
(`PurgeOrphanedScopeExclusions`) bounds growth at ~N where N is
`(unique slugs the operator has excluded in the last 90 days)`. We
do not expect abuse patterns (an operator would have to manually
exhaust slugs); revisit after customer usage shows a need.

## Consequences

### Positive

- **Persistent operator intent is honoured.** A slug excluded once
  with `--persist-exclude` stays excluded on every subsequent
  deploy until the operator clears it.
- **Audit trail is durable.** `kind=project.scope.excluded` rows
  live forever; the active table is the operator's working set,
  the audit log is the SOC 2 CC7.2 paper trail.
- **No orphan accumulation.** The no-FK-to-apps contract + the
  janitor `PurgeOrphanedScopeExclusions` self-clear stale snapshot
  references inside the 90-day horizon.
- **Backward-compatible wire.** The new `PersistedExclusions` field
  is `omitempty`; clients that don't read it see no behaviour
  change.

### Negative / risks

- **Stale `app_id` references.** A persisted exclusion pointing at
  an app that has since been re-deployed (same slug, different
  identity) carries a stale snapshot reference. The audit log is
  the durable record; `app_id` is best-effort.
- **90-day window is implicit.** Operators who expect "forever" may
  be surprised when the active row is reaped after 90 days. The
  audit log persists; the operator must rely on it for old
  exclusions. This is documented in the customer-facing
  `docs/affected-workload-preview.md`.
- **SDK regen churn.** `PersistedExclusions` is a new field;
  downstream SDKs (Go, Node, Python) require a regen commit at the
  end of PR-B.

### Operational

- New audit kind `project.scope.excluded` — closed-set extension,
  no cardinality risk.
- No new Prometheus metric in this PR (the gate-rescue counter
  `apid_plan_gate_rescued_by_exclude_total` lands in PR-A).
- No new quota; no new limit table row. `pkg/api/limits.go`
  unchanged.

## References

- `pkg/state/pgstore.go:3426,3442-3444` — soft-delete by `status`
  UPDATE; the contract that drives the no-FK posture.
- `pkg/reconcile/audit.go:34` — `KindWorkloadSkipped` precedent.
- `migrations/00384_mirror_rules.sql:76-112` — schema + trigger
  template.
- `pkg/state/pgstore_app_webhooks.go:33-253` — CRUD pattern for the
  pgstore file.
- `pkg/api/dto.go:3546-3610` — `PlanResponse` extension seam.
- `cmd/apid/scan_service.go:858-874` — slog emission seam for the
  `persisted=true` flag.
- `cmd/apid/handlers_decompose.go:34,65` — `scanProject` /
  `applyProject` apply-path fallback.
- `cmd/gregale/commands2.go:806` — deploy `--persist-exclude` flag.
- `cmd/gregale/commands_decompose.go:48` — scan `--persist-exclude`
  flag.
- `pkg/api/client.go:857,884,921-953` — `ScanProject` /
  `ApplyProjectPlan` signature extension.
- ADR-124 — affected-workloads preview with `--exclude` (the
  per-deploy precedent this ADR extends).
- ADR-041 — migration slot fence pattern (slot 00417 reservation).
- ADR-050 — repo decomposition + project object (the wider
  context).

## Acceptance notes (2026-08-24)

- Shipped in PR-B (the persistence branch of the ADR-124 follow-up
  cluster), migration 00418 + PgStore CRUD + audit kind + wire
  field + CLI flag. PR-B is the **last PR** in the cluster to touch
  this table; subsequent surfaces that consume the persisted set
  must respect the no-FK contract.
- Soft-delete CASCADE blind spot pinned at
  `pkg/state/pgstore_deployment_scope_exclusions.go::CreateDeploymentScopeExclusion`
  header comment.
- Janitor hook (`PurgeOrphanedScopeExclusions`) NOT in scope of
  PR-B; documented as follow-up in
  `docs/repo_decomposition_implementation.md` §7 follow-on.
