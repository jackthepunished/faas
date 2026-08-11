# ADR-091 — PR-D cluster outline

## Why a single PR (PR-D) instead of the 3-PR cluster shape used by ADR-090

ADR-090 shipped via PR-A #827 (data model), PR-B #833 (API surface),
and PR-C (deferred — multi-scope overlay). PR-D deliberately does
NOT split the same way:

1. **PR-C is gone.** PR-C's responsibility was the multi-scope
   overlay on wake (nested env.json on disk, vmmd picks flat vs
   nested based on `len(scopeSet) <= 1`). The roadmap's Phase 3
   goal is "one deployment, one scope, flat env" — which is what
   PR-D ships on its own. PR-C's deferred scope doesn't justify a
   separate PR.
2. **PR-D's surface is one cohesive change.** Adding
   `deployments.scope` requires (a) a schema migration, (b) a
   State-interface widening, (c) a schedd thread-through, and (d) a
   DTO surface change. These are reviewable together because they
   cannot land independently — the column needs the surface, the
   surface needs the schedd threading, and the schedd threading
   references the column.
3. **ADR-090's PR-A was an extreme refactor.** It widened the
   app_envs PK from 2 columns to 3 columns, which is the
   load-bearing reason a separate PR makes sense — every read and
   write to that table changes. PR-D's change is additive: a new
   column with a default, plus a single SELECT projection tweak.
   The refactor cost is much smaller.

## What ships in PR-D

In dependency order:

1. **Schema:** `migrations/00213_deployments_scope.sql`
   - ALTER TABLE deployments ADD COLUMN scope (fast-default).
   - Shape CHECK constraint `deployments_scope_shape`.
   - Partial unique index `deployments_app_scope_live_uniq`.
   - Composite index `deployments_app_scope_idx`.
2. **sqlc regen:** `make schema-dump && sqlc generate`.
   `pkg/state/sqlc/models.go` adds `Scope string` to `Deployment`.
3. **State interface:** `pkg/state/store.go::LiveDeploymentForScope`,
   `pkg/state/pgstore.go::LiveDeploymentForScope`,
   `pkg/state/memstore.go::LiveDeploymentForScope`. SELECT
   projection widened via `scanDeploymentInto` + the two
   `deploymentSelectColumns*` consts.
4. **Schedd thread-through:** `pkg/sched/engine.go::loadAPIEnv`
   takes `scope string`; all three call sites pass `dep.Scope`.
   `pkg/api/env_scope.go::DefaultEnvScope = "default"` is the
   canonical default.
5. **DTO surface:** `pkg/api/dto.go::CreateDeploymentRequest.Scope`,
   `CreateDeploymentOverrides.Scope`, `DeploymentResponse.Scope`.
6. **Handler:** `cmd/apid/handlers.go` validates `req.Scope`
   via `api.ValidateScope`; `buildDeploymentForInsert` threads it
   through; `CreateDeployment` errors on a duplicate scope via
   `CodeDeploymentScopeCollision` 409.
7. **Schema dump mirror:** `schema.sql` updated to reflect
   post-00213 state.
8. **Migration test:** `migrations/00213_deployments_scope_test.go`
   with six assertions (column shape, CHECK, partial unique index
   shape, uniqueness violation, backfill pin, replay-safety).
9. **E2E:** new `cmd/e2e/deployment_scope_e2e_test.go` exercises
   the full seam (scope-aware wake + uniqueness collision).
10. **Docs:** this file + `docs/adr/091-deployments-scope.md`.

## Slot fence choreography (the part operators care about)

PR-D's commit sequence:

1. Stage `migrations/00213_deployments_scope.sql` (real).
2. Stage `migrations/00213_deployments_scope_test.go` (test).
3. Stage `migrations/00208_reserve_slot.sql` (fence).
4. `git rm migrations/00208_reserve_slot.sql` (same commit).

The stale 00204 + 00205 PR-A fences STAY on the PR-D branch —
`TestMigrationsContiguous` is strict ("never skip a slot"), and
PR-D does not consume those slots. A future PR that lands a real
migration at 00204 OR 00205 drops the matching fence in its own
commit. The cross-PR slot fence convention
(`memory/cross-pr-slot-gate-reservation-fence-pattern.md`) covers
this — fences are owned by their consumers, not by an unrelated
PR that happens to land in the neighbourhood.

After PR-D lands:
`git ls-tree origin/main migrations/ | grep '00207_reserve_slot'` returns a real fence file (the contested slot 00207 is held
under ADR-041 — see `cross-pr-slot-gate-reservation-fence-pattern`).
The PR-D real migration at 00213 lands four slots past the 00207
fence. Slot map is contiguous 00200..00213 with the 00207, 00208,
and 00209 fences holding the contested slots for future use; slot
00210 is owned by PR #835's real `00210_crons_unique_app_schedule_path.sql`,
slot 00211 is a #838 cluster fence, slot 00212 is owned by PR #838's
real `00212_github_webhook_secrets.sql`.

## What does NOT ship in PR-D (explicit deferrals)

- Multi-scope overlay (PR-C of the original cluster).
- `guest-init` nested-decode branch.
- `cmd/gregale` `--scope` flag for `deployment create`.
- `Limits.EnvScopesMax` per-scope cap.
- Update-time scope change on a live deployment.
- Pre-flight `SELECT 1 FROM deployments WHERE status='live'
  GROUP BY app_id, scope HAVING count(*) > 1` — operator runbook
  note only; the migration's `CREATE UNIQUE INDEX IF NOT EXISTS`
  silently keeps the duplicate pre-PR-D rows.

## Pre-flight operator runbook snippet

If a customer is on main with TWO live deployments on one app
with identical scope (the pre-PR-D wake-target ambiguity), the
00213 migration's `CREATE UNIQUE INDEX IF NOT EXISTS` will NOT
fail loudly on the conflict — it silently keeps the duplicate
rows. The operator must run before applying 00213:

```sql
SELECT app_id, scope, count(*)
FROM deployments
WHERE status='live'
GROUP BY app_id, scope
HAVING count(*) > 1;
```

If any rows come back, manually supersede all but one per
(app_id, scope) via the apid route or `gregale deploy supersede`.
This is a one-time per-customer event; PR-D does NOT ship an
auto-detection CLI.
