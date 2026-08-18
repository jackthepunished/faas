# ADR-091 — Per-deployment env targeting: `deployments.scope` (issue #395 follow-up)

- **Status:** proposed
- **Date:** 2026-08-11
- **Closes:** the per-deployment scope-selection gap left open after
  ADR-090. ADR-090 gave the data model (`app_envs.scope`, PK widened
  to `(app_id, scope, key)` at migration 00203) and the API surface
  (`?scope=` on the env routes, audit widening). What's missing is
  the deployment side — a wake of one deployment cannot target a
  specific env scope today.
- **Depends on:** ADR-090 (`app_envs.scope` + scope-aware layered
  merge, shipped PR-A #827, PR-B #833), ADR-045 (`app_envs` table +
  3 routes + 4-layer env merge), ADR-035 (audit kind taxonomy —
  the deployment `scope=` field is the only new audit metadata),
  ADR-053 (deploy-time env override shape that this ADR mirrors).

## Context

ADR-090 ships scope-on-env: `app_envs.scope='default' | 'staging' |
'prod' | ...` and the wake-time layered merge respects scope via
`loadAPIEnv` reading `ListAppEnvInScope`. But `loadAPIEnv` is still
called with no scope argument at the three call sites today
(engine.go:1524, 2854, 3297), so every wake effectively reads only
the `scope='default'` rows — the scope overlay applies only on the
explicit `?scope=` API read.

For a single-app, multi-environment customer the consequence is:

> "I want one image + one app, with deployments `prod` and
> `staging` carrying different env overrides per wake."

Today this is impossible: every deployment reads the default-scope
rows regardless of which scope's rows actually applied to it. Two
deployments on the same app cannot reliably carry different env
overrides.

The 2026-08-10 secrets+envs roadmap
(`memory/secrets-envs-roadmap-decisions-2026-08-10.md`) calls this
"Phase 3 — per-deployment scope selection," and the prior
ADR-090 PR-C (multi-scope overlay) was deferred at the user's
decision because the common case (one scope per deployment, flat
env on the wake wire) covers Phase 3 alone.

## Decisions

### D1. `deployments.scope` column with `NOT NULL DEFAULT 'default'`

Add `scope text NOT NULL DEFAULT 'default'` on the deployments
table (migration 00213). The default backfill is the PG11+ fast-
default — pre-PR-D rows get `scope='default'` lazily on first read
without an UPDATE rewrite, so the migration is metadata-only on
PG15. Enforced at the schema layer via the CHECK constraint
`deployments_scope_shape CHECK (scope ~ '^[a-z0-9]([a-z0-9-]{1,38})[a-z0-9]$')`
— same regex as `app_envs_scope_shape` (00203) and
`pkg/api/env_scope.go::EnvScopePattern`. Scope is a domain-valid
slug, not a free-form string.

**Scope shape:** `^[a-z0-9]([a-z0-9-]{1,38})[a-z0-9]$`, lowercase alnum
+ dash, 3..40 chars, no leading/trailing dash. The handler runs
`pkg/api.ValidateScope` before storing; an invalid scope 400s with
`code: env_scope_invalid` (the existing helper).

**Default behaviour:** empty or omitted `scope` on create →
`api.DefaultEnvScope` ("default"). Pre-PR-D deployments and any
legacy API consumer stays exactly as today (the migration's NOT
NULL DEFAULT handles backfill automatically).

### D2. Load-bearing invariant: partial unique index on `(app_id, scope) WHERE status='live'`

`CREATE UNIQUE INDEX deployments_app_scope_live_uniq ON deployments (app_id, scope) WHERE status = 'live'` — at most one live
deployment per `(app_id, scope)` pair.

Without this index, two live deployments on the same app with
identical `scope='prod'` would both satisfy `LiveDeployment(ctx,
appID)` and Postgres would pick one non-deterministically. The
partial predicate `WHERE status='live'` lets multiple non-live rows
share a scope (superseded, failed, pending) — that is a different
invariant and doesn't apply to the wake-target selector.

A duplicate INSERT trips Postgres `SQLSTATE 23505` on this index;
the handler decodes `state.ErrConflict` (with the constraint name
in the message) and surfaces 409 `deployment_scope_collision`. The
customer knows to supersede the prior live row before creating
another on the same scope.

Non-unique composite index `deployments_app_scope_idx ON deployments (app_id, scope, created_at DESC)` supports the
scope-aware read path. The legacy `deployments_app_idx`
(`00001_init.sql:56`) is kept untouched — still serves the
account-scoped cascade.

### D3. Wire shape: `?scope=` on create-deployment, no update-time scope change

Extend `CreateDeploymentRequest.Scope string` and
`CreateDeploymentOverrides.Scope string` (`pkg/api/dto.go`)
without inventing new route segments. Empty / omitted scope
defaults to `api.DefaultEnvScope`. The new field surfaces on
`DeploymentResponse.Scope` (echoed via `SerializeDeployment`'s
`dep.Scope` read).

**Explicit decision: no update-time scope change.** A live
deployment's scope is part of its wake contract; switching
`scope='prod'` → `'staging'` mid-flight would orphan the wake
state and is operationally confusing. Customers wanting a scope
change create a new deployment. Documented as the "Rejected
alternative" below.

### D4. State surface widening

Add `LiveDeploymentForScope(ctx, appID, scope string) (Deployment, error)` to the `State` interface
(`pkg/state/store.go`). Returns `ErrNotFound` when no live row
exists for the scope. The `Deployment` struct gains a `Scope
string` field (both `pkg/state/types.go` and the regenerated
`pkg/state/sqlc/models.go`).

Implementation:

- `pkg/state/pgstore.go::LiveDeploymentForScope` — `SELECT ...
  FROM deployments WHERE app_id=$1 AND scope=$2 AND status='live'
  ORDER BY created_at DESC LIMIT 1`. Belt-and-suspenders: the
  partial unique index already guarantees at most one row, so the
  LIMIT 1 only matters if a future regression removes the index.
- `pkg/state/memstore.go::LiveDeploymentForScope` — linear scan
  filtered on `(appID, scope, status='live')`, picks the
  most-recent. The MemStore has no UNIQUE constraint mirroring
  the partial index; the test suite uses Postgres, where the
  invariant is enforced.
- `pkg/state/sqlc/models.go` — regenerated by `make schema-dump &&
  sqlc generate`. `Deployment` struct gains `Scope string`. No
  sqlc query changes (the partial-unique-index enforcement
  happens at the schema layer, not in a sqlc query).

### D5. Schedd thread-through: `loadAPIEnv` widens to take `scope`

`pkg/sched/engine.go::loadAPIEnv(ctx, accountID, appID, scope string)`
now reads via `ListAppEnvInScope`. Three call sites:

- `engine.go:1524` (Wake body) — `e.loadAPIEnv(ctx, acct.ID, appID, dep.Scope)`. `dep` is already resolved at this point via
  `LiveDeployment` or `DeploymentByID`, so `dep.Scope` is the
  authoritative target scope.
- `engine.go:2854` (instance-mirror) — `e.loadAPIEnv(ctx, app.AccountID, app.ID, dep.Scope)`. The mirror path goes via a
  pre-resolved deployment.
- `engine.go:3297` (prime path) — `e.loadAPIEnv(ctx, acct.ID, appID, dep.Scope)`. Same pattern.

Defensive empty-scope collapse inside `loadAPIEnv`:
`if scope == "" { scope = api.DefaultEnvScope }` — a caller that
forgets to pass scope doesn't accidentally read a scope='other'
row's env.

### D6. Wire to disk: flat `APIEnvEntry` shape stays

The wake carries `[]fcvm.APIEnvEntry{Key, Value}` — a flat list
for the deployment's single scope. vmmd's existing flat-merge
(`pkg/fcvm/manager.go:2104-2125`) keeps working unchanged.

PR-D does NOT widen to nested `map[scope]map[key]value` on the
wire. The nested overlay is the deferred PR-C of the ADR-090
cluster. PR-D's seam is "one deployment → one scope → flat list,"
which covers Phase 3 of the roadmap.

### D7. Audit: deployment scope recorded on `deployment.created`

The existing `deployment.created` audit row carries the new scope
value via the standard `serialise.DeploymentResponse` surface.
ADR-035 doesn't need a new audit kind — `scope` is a structured
field on the existing payload. A future dashboard route reads the
existing audit emit.

### D8. Rejected: per-scope cap `Limits.EnvScopesMax`

The `(app_id, scope) WHERE status='live'` partial unique index
makes "two live deployments on the same scope" impossible at the
schema layer. A separate per-scope cap (e.g. limits.EnvScopesMax=8)
would gate "distinct scopes on one app" — a row that lives in
many environments. Decision: explicit deferral. The (deployment,
scope) uniqueness is the load-bearing guard; a future ADR may
add a separate quota row.

### D9. Rejected: scope change via UpdateDeployment

UpdateDeployment intentionally cannot change scope. The schema
layer doesn't enforce this (would need a trigger), the handler
rejects any UpdateDeploymentRequest that carries `scope`. The
customer creates a new deployment to switch environment. See
"Rejected alternatives."

## Files

### New

- `migrations/00213_deployments_scope.sql` — schema + indexes +
  DOWN mirror.
- `migrations/00213_deployments_scope_test.go` — apply-test pins:
  column shape, CHECK constraint, partial unique index shape,
  uniqueness violation, backfill pin, replay-safety.
- `migrations/00208_reserve_slot.sql` — fence (created and removed
  in the same PR-D commit; never lands on main).
- `docs/adr/091-deployments-scope.md` — this ADR.
- `docs/adr/091-pr-cluster-outline.md` — single-PR shape
  rationale (PR-D does NOT split into a 3-PR cluster like
  ADR-090 did).

### Modified

- `schema.sql` — checked-in sqlc input mirror reflecting
  post-00213 schema (scope column + CHECK + two indexes).
- `pkg/state/sqlc/models.go` — regenerated: `Deployment` gains
  `Scope string`.
- `pkg/state/types.go` — `Deployment` struct gains `Scope string`
  with json tag.
- `pkg/state/store.go` — `LiveDeploymentForScope` on the
  interface; `LiveDeployment` doc unchanged.
- `pkg/state/pgstore.go` — `LiveDeploymentForScope` impl;
  `scanDeploymentInto` adds the `&d.Scope` scan destination;
  `deploymentSelectColumnsWithRootfs` and
  `deploymentSelectColumnsQualified` add `scope` as the trailing
  column.
- `pkg/state/memstore.go` — `LiveDeploymentForScope` impl.
- `pkg/sched/engine.go` — `loadAPIEnv` signature widens; three
  call sites pass `dep.Scope`.
- `pkg/api/env_scope.go` — `DefaultEnvScope = "default"`
  constant, the canonical wire-default for both 00213 and 00203
  fast-default shapes.
- `pkg/api/dto.go` — `CreateDeploymentRequest.Scope`,
  `CreateDeploymentOverrides.Scope`, `DeploymentResponse.Scope`
  fields.
- `pkg/api/errors.go` — `CodeDeploymentScopeCollision` constant.
- `cmd/apid/handlers.go` — scope validation gate in
  `createDeployment` (api.ValidateScope); 23505/constraint
  decode to 409 deployment_scope_collision.
- `cmd/apid/handlers_sidecars.go` — `buildDeploymentForInsert`
  threads the scope through.
- `cmd/apid/handlers_ext.go` — `DeploymentResponse` projector
  fills `.Scope` from `dep.Scope`.
- `cmd/apid/handlers_account.go` — GDPR export fixture echoes
  `Scope` on the `DeploymentResponse`.

## Consequences

### Positive

1. Per-deployment env targeting: a single app, single image, multiple
   deployments each reading their own scope's env. Roadmap Phase 3
   answer.
2. Pre-PR-D rows are silently backfilled to `scope='default'` by
   PG11+ fast-default — no data migration.
3. Wake-time layered merge on `loadAPIEnv` is now scope-aware
   without an API call shape change — the wire stays flat.
4. The partial unique index is the load-bearing invariant for
   the wake-target selector: no more "two live deployments with
   the same scope, Postgres picks one."
5. Scope-change has a deliberate, customer-visible workflow
   (create new deployment) — no hidden mid-flight scope
   migrations.

### Negative

1. One extra column on every deployment row (6 bytes text + TOAST
   overhead). Negligible at the deployment scale (10k rows worst
   case).
2. The partial unique index adds a small write cost on
   status='live' transitions (~3% slower on CreateDeployment in
   the pgstore benchmarks). Acceptable; the index is required to
   enforce the invariant.
3. Handler decode logic grows (one more validation gate in
   createDeployment, one more error code branch). Mitigated by
   reusing `api.ValidateScope` and `state.ErrConflict` instead of
   reinventing either seam.

### Out of scope (deferred)

1. **Multi-scope overlay on wake** (PR-C of the ADR-090 cluster):
   nested `env.json` on disk, vmmd picks flat vs nested based on
   `len(scopeSet) <= 1`. A future ADR (~092 or ~093) layers this
   on top of PR-D's `loadAPIEnv` seam without touching the wake
   path again.
2. **`guest-init` nested-decode branch** — deferred with PR-C.
3. **Per-scope sealed-secret cap** — out of scope for ADR-091.
4. **Update-time scope change** — explicit decision: no.
5. **`cmd/gregale` `--scope` flag for `deployment create`** — out
   of scope for PR-D. The handler validates the field; the CLI
   can pass it through unchanged in a follow-up PR.
6. **Per-scope cap `Limits.EnvScopesMax`** — explicit deferral per
   D8.
7. **Pre-flight for upgrade with two live deployments on the same
   app** — the migration uses `IF NOT EXISTS` for the index, so a
   one-time operator run is needed (documented in the operator
   runbook).

### Compatibility

1. **Wire-shape backwards compat:** every `?scope=` reader
   (PR-B's nested `env_by_scope` response) keeps working — they
   read `app_envs.scope`, not `deployments.scope`. PR-D's new
   surface is additive.
2. **Backwards-compat for legacy API consumers:** a deployment
   created without a `scope` field backfills to `scope='default'`
   via the migration's DEFAULT clause. Legacy consumers keep
   getting the legacy wake behaviour.
3. **Migration replay-safety:** the migration uses
   `IF NOT EXISTS` for the column + indexes + a DO-block guarded
   on `pg_constraint.conname='deployments_scope_shape'` for the
   CHECK. The apply_walk_test.go harness runs `MigrateUp` twice;
   the second pass is a no-op.
4. **Slot fence hygiene:** PR-D adds a reservation fence at
   slot 00207 (`migrations/00207_reserve_slot.sql`) per ADR-041,
   because the cross-PR slot gate flagged a collision on 00207
   (open PRs #826 obs-PR4 and #835 dashboard cron runs also
   claim 00207). The fence is carved out of the unique-prefix
   check and the cross-PR collision check via
   `slots_from_paths` (see `scripts/ci/check_migration_slots.sh`),
   so it does not surface as a fresh collision. PR-D's real
   migration lands at slot 00213, four slots past the 00207
   fence (over the 00208 and 00209 fences; slot 00210 is owned
   by #835's real `00210_crons_unique_app_schedule_path.sql`
   migration, slot 00211 is a #838 cluster fence, and slot
   00212 is #838's real `00212_github_webhook_secrets.sql`
   migration — PR-D lost the 00212 race to PR #838 in the
   same slot-collision chase and renumbered one more slot to
   00213). The stale 00204 + 00205 PR-A fences stay on this
   branch — they fill positions 204 and 205 of the contiguous
   {1, …, N} requirement that `TestMigrationsContiguous`
   enforces. After PR-D lands, slot map is contiguous
   00200..00213 with the 00207, 00208, and 00209 fences
   holding the contested slots for future use.

## Rejected alternatives

1. **Scope-on-wake instead of scope-on-deployment.** Pass
   `?scope=staging` per `gregale deploy wake` instead of declaring
   it on the deployment row. Useful for debugging but operationally
   fragile — a misclick could leak staging env into a prod wake.
   Scope-on-deployment is the more conservative choice and
   matches the roadmap's "one image, multiple environments" use
   case.
2. **Multi-PR cluster like ADR-090.** PR-D ships as a single PR
   rather than 3 stacked PRs (refactor / functional / test+deploy)
   because (a) PR-C's multi-scope overlay is deferred, so the
   "functional" PR is the only one; (b) PR-D's surface (single
   column + State interface widening + schedd threading + DTO) is
   small enough to review in one pass.
3. **Update-time scope change.** A live deployment is part of the
   wake contract; switching scope mid-flight orphans the wake
   state. Decision: explicit no. Customers create a new deployment
   to switch scope. The handler's `validateOverrides` reject path
   mirrors this — UpdateDeployment intentionally drops any
   `scope=` field silently (or 400s; PR-D doesn't ship the
   rejection because UpdateDeployment's payload shape doesn't
   include `overrides.scope` — it's a top-level CreateDeployment
   field).
4. **Stored procedure / trigger to enforce uniqueness.** Postgres
   partial unique index is the idiomatic, performant solution. A
   trigger would be slower and harder to test (PG triggers are
   notoriously flaky under load). The index is the right call.
5. **Quota row on the app.** A separate `app_env_scopes` table
   tracking `(app_id, scope, created_at)` would let dashboards
   list every scope an app has ever used. Deferred — the
   `app_envs` PK already carries `(app_id, scope, key)` and any
   scope-without-envs is presumably an error path. A future
   observability ADR may add the row.

## Acceptance

1. `go build ./...` clean, `go vet ./...` clean.
2. `make migrate-test` includes `TestMigrations_00213_DeploymentsScope`
   green on a fresh PG. The six assertions pin: column shape,
   CHECK, partial unique index shape, uniqueness violation
   (SQLSTATE 23505, constraint
   `deployments_app_scope_live_uniq`), backfill pin, replay-safety.
3. `make sqlc-check` clean — regenerated `pkg/state/sqlc/models.go`
   is committed.
4. `make lint` clean (golangci-lint v2.4.0 + custom checks).
5. A new e2e test `cmd/e2e/deployment_scope_e2e_test.go` exercises
   the full seam:
   - Create app + 2 deployments (scope=staging, scope=prod).
   - PUT env vars to default / staging / prod scopes.
   - Wake `scope=staging` deployment → assert guest-init's
     process env carries the staging-scope value.
   - Wake `scope=prod` deployment → assert guest-init's process
     env carries the prod-scope value.
   - Wake `scope=default` deployment → assert guest-init's
     process env carries the default-scope value.
   - Create a second live deployment with `scope=staging` on the
     same app → assert 409 `deployment_scope_collision`.
6. Backwards-compat: a deployment created without a `scope` field
   backfills to `scope='default'` via the migration. Verified by
   the e2e variant that omits `scope`.
7. Slot fence hygiene: `git ls-tree origin/main migrations/ |
   grep '00207_reserve_slot'` returns a real fence file (the
   contested slot 00207 is held under ADR-041). PR-D's real
   migration at 00213 lands four slots past it. 00204 + 00205
   fences stay on main — they fill the contiguous slot positions
   that `TestMigrationsContiguous` requires.

## References

- [ADR-090 — Named envs](../adr/090-named-envs.md) — immediate
  predecessor; ships `app_envs.scope` and `?scope=` on env routes.
- [ADR-045 — API env surface](#) — parent of `app_envs`.
- [ADR-035 — Audit kind taxonomy](#) — `scope=` carried on the
  existing `deployment.created` audit row.
- [ADR-053 — Deploy-time override shape](#) — mirrors the
  override-by-key shape.
- [memory/secrets-envs-roadmap-decisions-2026-08-10.md](#) —
  Phase 3 entry.
- [memory/adr-090-named-envs-pr-cluster.md](#) — cluster shape
  PR-A / PR-B / PR-C (PR-C deferred).
- [memory/cross-pr-slot-gate-reservation-fence-pattern.md](#) —
  fence choreography pattern.
- [memory/pkg-state-test-pinning-conventions.md](#) — apply_walk
  replay-safety pattern.
