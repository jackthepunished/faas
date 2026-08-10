# ADR-090 PR-cluster outline

This file is the commit strategy for ADR-090 (named envs / `app_envs.scope`).
It mirrors the ADR-089 PR-cluster pattern (`docs/adr/089-pr-cluster-outline.md`):
three PRs, each independently mergeable, sized to review in ≤10 minutes.

**Slot fence:** drop a `migrations/00XXX_reserve_slot.sql` fence
(idempotent `select 1;`) at slot **193** before PR-A lands. Rename
193 → the canonical slot (which IS 193 since 00192 is the latest real
migration) inside PR-A's migration file at the same time the fence is
removed. The cross-PR slot fence pattern from
`[[cross-pr-slot-gate-reservation-fence-pattern]]` and
`[[cross-pr-slot-gate-pagination-gate]]` applies — pre-check via
`gh api pulls/N/files` + own `git ls-tree origin/<branch>` before
assuming 193 is free on the active PR.

## PR-A — Schema + State surface + scope validator (the only PR that touches data)

**Branch:** `worktree-adr-090-named-envs/pr-a-schema-scope`
**Size:** ~280 LoC across 9 files
**Review time:** ~9 min

**Files:**

| Action | Path | Content |
|---|---|---|
| New | `migrations/00193_app_envs_scope.sql` | `scope text NOT NULL DEFAULT 'default'` + PK widening `(app_id, scope, key)` + scope-shape CHECK + composite `(account_id, app_id, scope)` index. |
| New | `migrations/00193_app_envs_scope_test.go` | Slot-fence + idempotency + default-backfill test (mirrors `00191_app_secrets_kid_test.go`). |
| New | `pkg/api/env_scope.go` | `ValidateScope(s string) *Problem` — rejects `__all__` on the write path; rejects empty + out-of-shape strings; reuses `^[a-z0-9][a-z0-9_-]{0,63}$` regex (same shape as `validSlug()`). |
| New | `pkg/api/env_scope_test.go` | Round-trip valid scopes + rejected reserved name + rejected shape cases. |
| Modified | `pkg/state/store.go` | `AppEnv` struct gains `Scope string`; `UpsertAppEnv` / `DeleteAppEnv` / `ListAppEnv` / `CountAppEnv` widen to accept / return scope; `envKey` in `memstore.go` mirrors the new PK. |
| Modified | `pkg/state/pgstore.go` | Queries rewrite: `ON CONFLICT (app_id, scope, key)` for Upsert; WHERE clauses widen to include `scope`; ORDER BY adds `scope` for deterministic sort. |
| Modified | `pkg/state/memstore.go` | Mirror. |
| Modified | `pkg/fcvm/manager.go` | `APIEnvEntry` struct gains `Scope string` field. `stageAPIEnv` widens to filter-by-scope (D4 — overlay logic lives here, not schedd). |
| Modified | `api/proto/onebox/faas/vmmd/v1/vmmd.proto` | `APIEnvEntry` proto gains `string scope = 2;` (proto3 default-empty == legacy semantics; vmmdgrpc clients built against the old proto map empty → "default"). |

**Why this is PR-A:** schema + state surface + scope validator + the
data-model widening. Nothing in this PR is user-facing. Reviewers can
verify the data model + the proto regeneration + the upsert SQL
without reading any HTTP or wake-time code. The migration sits on top
of migration 00099 (orgs expansion) — that migration added a nullable
`org_id` and did NOT change the PK; this is the first PK widening
since 00061.

**Acceptance:**

- `make test` — unit tests pass (scope validator, pgstore Upsert with
  scope, memstore mirror, proto round-trip).
- `make migrate-test` — migration 00193 reads clean on a fresh PG,
  idempotent on re-run, default backfills `scope='default'` on every
  existing row.
- `make proto-check` — regenerated Go proto matches the new
  `APIEnvEntry` shape; vmmdgrpc client + apid client rebuild clean.

**Risk:** the PK widening forces an ACCESS EXCLUSIVE lock on
`app_envs` for the duration of the ALTER TABLE … DROP/ADD CONSTRAINT.
The migration must use `BEGIN; … COMMIT;` to keep the window atomic,
and the migration must run in a single transaction (no
`StatementBegin`/`StatementEnd` split inside). On a reference-node-
size table (10k env rows) the lock is sub-second; on the worst-case
estimated table (1M rows) the lock is still under 5s — well within
the migration's accepted downtime window. The migration's header
comment documents this with `EXPLAIN ANALYZE` expectations.

---

## PR-B — API routes + DTOs + audit payload widening (user-facing surface)

**Branch:** `worktree-adr-090-named-envs/pr-b-api-scope`
**Depends on:** PR-A
**Size:** ~340 LoC across 7 files
**Review time:** ~10 min

**Files:**

| Action | Path | Content |
|---|---|---|
| Modified | `pkg/api/env.go` | `AppEnvResponse` keeps the existing flat shape; new `ScopedAppEnvResponse{Key, Scope, CreatedAt, UpdatedAt}`; `AppEnvListResponse` gains `EnvByScope map[string][]AppEnvResponse \`json:"env_by_scope,omitempty"\`` (D3 discriminated union). |
| New | `cmd/apid/handlers_env_scope.go` | Three new handlers — `listEnvAll` (renders `?scope=__all__` → nested map), `setEnvScoped`, `deleteEnvScoped` — or extend the existing three handlers in `handlers_env.go` with `?scope=` parsing. (TBD in PR; pick whichever keeps `handlers_env.go` under the 50-line handler limit per CLAUDE.md conventions.) |
| New | `cmd/apid/handlers_env_scope_test.go` | Default-scope path is byte-identical to ADR-045 (backwards-compat pin); `?scope=staging` filter; `?scope=__all__` returns nested map; PUT with bad scope → 400 `env_scope_invalid`; PUT with `?scope=__all__` → 400 `env_scope_invalid`; audit payload widens to include `scope`. |
| Modified | `cmd/apid/handlers_env.go` | Audit emit `env.set` / `env.deleted` payload widens from `{app_id, name}` to `{app_id, name, scope}` (D5 — additive key, no migration needed). |
| Modified | `cmd/apid/server.go` | Three route registrations widen to accept `?scope=` query parameter; no new routes (the wire-shape widening is a query-string change, additive). |
| Modified | `pkg/api/errors.go` | New problem constructors `ErrEnvScopeInvalid(detail)` + `ErrEnvScopeReserved` (codes `env_scope_invalid`, `env_scope_reserved`). |
| Modified | `api/openapi.yaml` | `?scope=` parameter on the three env routes (with `default: "default"`, enum for reserved-name rejection); `env_by_scope` field on `AppEnvListResponse`; `scope` field on `ScopedAppEnvResponse`; new error responses. |
| Modified | `pkg/apid/openapi.yaml` | The `//go:embed` copy (run `make spec-sync` after openapi.yaml change — `spec-sync-stale-embed-on-openapi-change.md`). |

**Why this is PR-B:** every user-facing wire surface lands here. PR-A
gave us the data model + scope validator; PR-B gives us the API on top.
The audit payload widening is additive (new `scope` key in the `data`
map) — existing consumers that don't read the field are unaffected.
The `?scope=__all__` flat-vs-nested discrimination is the only new
wire shape; without `?scope=__all__`, the response is byte-identical
to ADR-045.

**Acceptance:**

- `make test` — unit tests pass.
- `make spec-check` — openapi.yaml ↔ Go code parity.
- `cmd/e2e/named_envs_api_e2e_test.go` — full PG-backed acceptance:
  seed default row → PUT `?scope=staging` with same key + different
  value → GET `?scope=__all__` returns `{env_by_scope: {default:...,
  staging:...}}` → assert audit emit on the staging row carries
  `data.scope="staging"`.

**Risk:** the openapi.yaml regen touches `pkg/apid/openapi.yaml`. Run
`make spec-sync` after every openapi.yaml edit, or CI's spec-check
gate fails (memory: `spec-sync-stale-embed-on-openapi-change.md`). The
`?scope=` parameter is additive; no client SDK breaking change — old
SDKs read `env` and ignore `env_by_scope`; new SDKs read both. The
Go SDK's missing env-scope re-export (see ADR-090 §"Out of scope") is
explicitly deferred to a follow-up — PR-B does NOT touch
`sdk/go/scopes.go`.

---

## PR-C — Wake-time scope overlay + guest-init nested decode + docs

**Branch:** `worktree-adr-090-named-envs/pr-c-wake-guest`
**Depends on:** PR-A, PR-B
**Size:** ~220 LoC across 6 files
**Review time:** ~8 min

**Files:**

| Action | Path | Content |
|---|---|---|
| Modified | `pkg/fcvm/manager.go` | `Manager.Wake` widens the api-env staging block: filter entries by scope (D4 — `default` always layered, named scopes overlay in alphabetical order), choose flat-vs-nested JSON based on `len(scopeSet) <= 1`. The merge helper moves to a new file `pkg/fcvm/api_env_scope.go` for testability. |
| New | `pkg/fcvm/api_env_scope.go` | `mergeAPIEnvEntries(entries []APIEnvEntry, targetScope string) (scopes map[string]map[string]string, isFlat bool)` — pure function, no I/O. |
| New | `pkg/fcvm/api_env_scope_test.go` | Single-scope → flat; multi-scope → nested; alphabetical overlay (default first, then named); `targetScope=staging` + default-only rows → flat with staging as outer scope. |
| Modified | `guest/init/env_linux.go` | `loadAPIEnv` widens: probe top-level value shape, decode flat-or-nested, iterate-entries to merge across scopes (alphabetical order, default-first). |
| Modified | `guest/init/env_linux_test.go` | `TestLoadAPIEnv_FlatShape_StillWorks` (backwards-compat pin for the legacy single-scope case); `TestLoadAPIEnv_NestedShape_MergesScopes` (default + staging overlay); `TestLoadAPIEnv_AlphabeticalScopeOrder` (deterministic contract). |
| Modified | `docs/adr/README.md` | One-line index entry for ADR-090 + link to `090-named-envs.md` + link to `090-pr-cluster-outline.md`. |
| Modified | `docs/ops/named-envs.md` (new) | Operator runbook: how to migrate from per-app env to per-scope env; how `?scope=__all__` surfaces in the dashboard; what the nested `env.json` wire looks like; how to debug a wake that picks the wrong scope. |
| New | `cmd/e2e/named_envs_e2e_test.go` | Full lifecycle: seed default + staging rows → cold-boot a wake → assert guest-init's process env contains the merged map (`default` keys overridden by `staging` keys for the same key) → kill apid → restart → re-wake → assert env.json wire is deterministic (MD5 pin for the same input set). |

**Why this is PR-C:** everything in PR-C is the operational close —
the wake-time merge (vmmd side) + the guest-init decode (guest side)
+ the docs/runbook. The core feature ships without PR-C: a single-
scope customer never crosses the nested-shape boundary, and the
existing flat wire keeps working through every PR-A and PR-B commit.
PR-C adds the multi-scope overlay that closes the staging-vs-prod use
case from the roadmap.

**Acceptance:**

- `make test` — unit tests pass (merge helper, guest-init nested
  decode, alphabetical ordering).
- `make metal-lima` — wake path unchanged for single-scope apps
  (flat env.json wire + flat guest-init decode); MD5 of
  `/etc/faas/env.json` byte-identical for the same input set across
  restarts (the deterministic-ordering pin).
- `make leakcheck` — no leaked goroutines (the new helper is pure;
  no new daemon goroutines).
- `cmd/e2e/named_envs_e2e_test.go` — full lifecycle green.
- `make spec-check` — last sanity check.

**Risk:** guest-init is a static Go binary that ships in the base
image. The new decode branch MUST stay backwards-compatible — any
existing single-scope env.json on a parked snapshot must continue to
decode cleanly through the new code path. The
`TestLoadAPIEnv_FlatShape_StillWorks` test pins this; the metal
test verifies it end-to-end on a real microVM. If the metal test
flakes, the most likely cause is `encoding/json` map iteration order
— pin alphabetical sort in the merge helper to make the wire
deterministic.

---

## Commit order within each PR

Each PR is a single squash-merge commit on its branch. The commit
sequence across PRs is:

1. PR-A squash: "feat(state+proto+migration): app_envs.scope column + PK widening (ADR-090 PR-A)"
2. PR-B squash: "feat(apid+openapi): scoped env routes + audit payload widening (ADR-090 PR-B)"
3. PR-C squash: "feat(fcvm+guest-init+e2e): wake-time scope overlay + nested env.json decode (ADR-090 PR-C)"

Each PR's commit message names the ADR. The PR description names the
milestone (none — this is post-M8 hardening, no spec §14 milestone
tied to it). When the PR-cluster lands, the ADR slot fence file at
slot 193 is removed in PR-A's commit (the rename from 193 → 193 is
trivially a no-op because no fence sibling exists at this slot; the
fence pattern only applies if the cluster slots a fence to reserve
against sibling PRs that want the same slot).

## Cross-cutting notes

- **Per-scope sealed secrets:** rejected (roadmap memory
  `secrets-envs-roadmap-decisions-2026-08-10.md`). Sealed secrets stay
  per-app. ADR-090 does not introduce per-scope sealed secrets; that's
  a future ADR (likely 092).
- **Wake-time scope selection by deployment:** deferred to ADR-091
  (Phase 3). ADR-090 widens the wake-time data path (every entry
  carries a `Scope`), but schedd's wake-target selection still merges
  every scope and vmmd's overlay decides what reaches the guest. ADR-091
  will wire `deployments.scope` into schedd so a single deployment
  targets ONE named scope.
- **CLI surface (`gregale env list/set/unset --scope <name>`):**
  deferred to PR-D or a separate ADR. The inventory surfaced this as a
  pre-existing gap (`cmd/gregale env pull/push` reuses the secrets
  HTTP API rather than the env endpoints); ADR-090 does not close it.
- **Go SDK `ScopeEnvRead`/`ScopeEnvWrite` re-export:** deferred. The
  Python + Node SDKs have full env DTO coverage today; only the Go SDK
  is missing the re-export. Mechanical follow-up.
- **`/etc/faas/env.json` wire shape:** dual-shape (flat or nested).
  vmmd chooses the format; guest-init handles both. Pin
  `TestLoadAPIEnv_FlatShape_StillWorks` to lock the backwards-compat
  path through every PR-C refactor.
