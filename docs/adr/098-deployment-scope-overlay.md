# ADR-098 amendment — Per-deployment scope overlay on `data_upstreams` (issue #954)

- **Status:** accepted
- **Date:** 2026-08-18
- **Closes:** issue #954 (`data_upstreams` PK widens to include
  `deployment_scope`). Tied to the §9.A cluster PR #894 (capture +
  chooser observability) and #960 (operator CLI).
- **Depends on:** ADR-090 named envs (the `scope` column on `app_envs`),
  ADR-091 named-env deployments (`deployments.scope` widening), ADR-098
  §9.A (the original `data_upstreams` capture surface).

## Context

ADR-098 §9.A shipped `data_upstreams` keyed on
`(app_id, scope, kind, host, port)`. `scope` is the env-scope from
ADR-090 — a string per `(app_id, key)` env row indicating which named
env the row belongs to (default / staging / prod / …).

When a customer runs multiple deployments of the same app (ADR-091
`deployments.scope`), each deployment can target a different physical
dependency. A staging deployment uses the staging database; a
production deployment uses the production database. Both env-classifier
observations land in `data_upstreams` and collide on the dedupe key,
collapsing to one `(app_id, postgres, host, port)` row — typically the
staging row, which is the first observed.

The schedd chooser (PR-D `pkg/sched/upstream_affinity.go`) collapses
on the same axis. Once collapsed, the chooser cannot apply per-
deployment bias: it sees one bias for the entire app, regardless of
which deployment is waking.

## Decision

Widen the dedupe UNIQUE INDEX from
`(app_id, scope, kind, host, port)` to
`(app_id, scope, deployment_scope, kind, host, port)` and stamp
`deployment_scope` from the apid writer. Mirror every place that
currently keys on `(app_id, scope, …)` for an
`data_upstreams`-distinguishing axis.

### D1. Schema — `migrations/00281_data_upstreams_deployment_scope.sql`

- `ADD COLUMN deployment_scope text NOT NULL DEFAULT 'default'` —
  backfills every pre-amendment row with `'default'`.
- New CHECK `data_upstreams_deployment_scope_shape` mirrors
  `data_upstreams_scope_check` (3..40 chars, `[a-z0-9-]`,
  `[a-z0-9]([a-z0-9-]{1,38})[a-z0-9]$`).
- `DROP INDEX data_upstreams_dedupe_uniq` +
  `CREATE UNIQUE INDEX data_upstreams_dedupe_uniq
   ON data_upstreams (app_id, scope, deployment_scope, kind, host, port)`.
- `data_upstreams_notify()` widens the pg_notify pipe-payload from 6
  to 7 fields: `app_id|scope|deployment_scope|kind|host|port|op`. Schedd
  does NOT currently LISTEN on `data_upstreams_changed` (per ADR §D2
  the chooser reads synchronously at wake, not via pg_notify), so the
  widening is dormant today — a future PR that turns on subscribers
  lands its parser in lockstep with this format.

The down-migration reverses the index first, then drops the column,
and restores the 6-field pipe format. Forward-only on the wire
change; the hazard is theoretical until the subscriber path is
re-introduced.

### D2. Writer — `pkg/state/queries.sql::InsertDataUpstream`

- Column list adds `deployment_scope` ($6 → `VALUES ... $5, $6, $7, …`).
- `ON CONFLICT` widens to
  `(app_id, scope, deployment_scope, kind, host, port)` so the
  dedupe-merge key matches the new UNIQUE INDEX.
- `DO UPDATE` adds `deployment_scope = EXCLUDED.deployment_scope`
  so a re-classification that re-stamps the deployment overlay
  refreshes the row's deployment identity.

`ListDataUpstreamsByApp` gains `sqlc.arg('cursor_deployment_scope')`
for `?deployment_scope=` server-side filtering. `ListAppUpstreamProbeScores`
(the JOIN-collapsed query consumed by the chooser) widens its predicate
to `deployment_scope = sqlc.arg('deployment_scope')::text`.

### D3. Writer-time contract — apid

The env classifier (`cmd/apid/handlers_env.go:runEnvClassifier`)
resolves `deployment_scope` via the existing
`pgstore.LiveDeploymentForScope(ctx, appID, scope)` shim
(`pkg/state/pgstore.go:4111`, backed by partial UNIQUE index
`deployments_app_scope_live_uniq` from migration 00213). On
`ErrNotFound` (no live deployment targets the env-scope, i.e. the
customer has not yet created one), fall back to `defaultEnvScope =
"default"` — the migration's DEFAULT matches.

The explicit POST handler (`cmd/apid/handlers_upstreams.go`)
accepts optional `deployment_scope` in the request body
(`PutDataUpstreamRequest.DeploymentScope`). When omitted, defaults
to `"default"` via the same fallback chain.

### D4. Chooser — `pkg/sched/upstream_affinity.go`

The cache map widens from `map[string]upstreamAffinityEntry` keyed
on `appID` to a composite key
`appDeploymentKeyOf(appID, deploymentScope)` — a string concat
`appID + "\x00" + deploymentScope`. Matches the per-deployment
ledger precedent at `pkg/sched/admission.go:294`. `Score` and
`Refresh` gain a `deploymentScope` argument; cold-path callers pass
`"default"` so single-deployment apps still hit their existing
upstream set.

### D5. Schedd wake call site — `pkg/sched/engine.go`

`dep.ID` is already in scope at the wake call site
(`pkg/sched/engine.go:1576`). Thread `dep.ID` into the chooser as
the new `deploymentScope` argument with `defaultDeploymentScope =
"default"` fallback for the cold-path branch where `dep` is nil.

### D6. GET filter — `cmd/apid/handlers_upstreams.go`

`GET /v1/apps/{slug}/upstreams` accepts optional `?deployment_scope=`
forwarded to `s.store.ListDataUpstreamsByApp`. Same shape as
`?scope=` already plumbed. The wrapped response envelope
(`DataUpstreamListResponse{Upstreams, Count, Quota_max}`) is
preserved — PR #960's `gregale inspect <slug> --upstreams` quota stamp
depends on the unchanged envelope shape.

### D7. Audit kinds

All three `data_upstream.*` audit kinds (`inferred`, `created`,
`deleted`) carry `deployment_scope` in the payload. Adding a key to
the audit map is non-breaking — downstream consumers ignore unknown
keys or merge into per-kind structs.

### D8. CLI — `cmd/gregale`

PR #960 (`gregale inspect <slug> --upstreams`) renders the new
`deployment_scope` column as a new column in the tabular output. The
`--json` envelope preserves the per-row shape (the wire DTO gains
`deployment_scope`).

## Consequences

- **Quota stamp is unchanged.** `DataPlacementHintsPerApp`
  (`pkg/api/limits.go:569`) remains a per-app cap. Per-deployment
  bias is implicit — different `deployment_scope` rows occupy
  separate slots but still share the cap. Documented in the limits
  table comment.
- **pg_notify wire change.** Forward-only on the 6→7 field pipe
  payload, but no consumer subscribes to `data_upstreams_changed`
  today (per ADR §D2 the chooser reads synchronously at wake). The
  widened pipe is dormant; the down-migration's recreation of the
  6-field function exists for replay-safety symmetry, not because
  a subscriber depends on it.
- **Chooser cache key collisions.** The new key is
  `appID + "\x00" + deploymentScope`. Pre-amendment entries keyed on
  `appID` alone are not load-bearing at restart (cache TTL is short
  via `api.UpstreamAffinityTTL`).
- **Migration backfill.** The `DEFAULT 'default'` stamp backfills
  existing rows. A live re-classification re-stamps the deployment
  overlay when env values shift — runtime mechanism.

## Rollback

The down-migration reverses the index first
(`DROP INDEX … ; CREATE UNIQUE INDEX … ON … (app_id, scope, kind, host, port)`),
then drops the column, then restores the 6-field pg_notify function.
The pipe-format reverse is for replay-safety symmetry, not because
any consumer reads the 6-field payload today (schedd does not
LISTEN on `data_upstreams_changed` — see §D1 step 5).

Single-deployment apps see no behavior change: the migration's
`DEFAULT 'default'` stamp matches the apid writer's fallback
(`defaultEnvScope = "default"`), and the schedd cold-path wake
threads `defaultDeploymentScope = "default"` when the engine has
no `dep` row in scope. The whole widening is observable only when
a customer has actually created two deployments with distinct
`scope` values.
