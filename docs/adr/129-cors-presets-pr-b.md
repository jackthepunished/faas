# ADR-129 · CORS presets: write API + per-rule wiring (issue #975 #4 PR-B)

- **Status:** proposed
- **Date:** 2026-08-24
- **Issue / PR:** [#975 item #4](https://github.com/poyrazK/faas/issues/975)
  (mega-API-native; PR-B closes the surface PR-A opened in PR #988)
- **Decision:** Promote `cors_presets` from a read-only data model to a
  full write API (`POST /v1/cors-presets`, `GET`, `PATCH`, `DELETE`),
  add a nullable `edge_rules.cors_preset_id` FK with `ON DELETE SET
  NULL`, and wire `gatewayd-internal`'s CORS compile path to resolve
  the preset through `MergeCorsPresetIntoRule`. Invalidate the
  per-account preset overlay via `pg_notify('cors_preset_changed',
  <account_id>)`.

## Context

PR-A (PR #988) shipped three pieces for issue #975 #4:

1. **Data model** — `cors_presets` table (migration `00304`) with
   `account_id` + optional `app_id` (NULL = account-wide) + per-row
   `allow_origins`, `allow_methods`, `allow_headers`, `expose_headers`,
   `allow_credentials`, `max_age_seconds`, `name`, `description`.
   Unique index `(account_id, COALESCE(app_id,
   '00000000-0000-0000-0000-000000000000'), name)`.
2. **Read path** — `pkg/state.Store` exposes
   `ListCorsPresetsForAccount`, `ListCorsPresetsForApp`,
   `GetCorsPresetByID` (pgstore + memstore mirrors).
3. **Compile helper** — `pkg/state/cors_preset.go::MergeCorsPresetIntoRule`
   validates the `*+credentials` footgun (ADR-091 D12) and returns
   `MergedCorsRuleAction` or typed error.

What's missing for PR-B:

- **No write API.** Customers cannot `POST /v1/cors-presets`, list
  their presets from the public surface, edit, or delete. The data
  model is inert.
- **No `cors.preset_id` field.** `state.EdgeRuleCORSAction` and
  `api.EdgeRuleCORSAction` have no way to reference a preset; the
  gateway can never call `MergeCorsPresetIntoRule` at compile time.
- **No `gatewayd-internal` wiring.** `compileCORSRules` at
  `cmd/gatewayd-internal/edge_rules.go:808` resolves inline action
  fields only.
- **No `pg_notify` cache invalidation.** Writes never fire the
  per-account reload signal that gatewayd-internal needs to see a
  preset edit reflected in `applyEdgeRuleCORS`.
- **No DTOs / OpenAPI / SDK.** `pkg/api/dto.go` has no
  `CorsPresetResponse`, `CreateCorsPresetRequest`, or
  `UpdateCorsPresetRequest`. No `/v1/cors-presets` paths in
  `api/openapi.yaml` or `pkg/apid/openapi.yaml`. SDKs untyped.

PR-B closes these gaps: write methods, DTOs, handlers, OpenAPI
surface, gatewayd-internal compile wiring, FK on `edge_rules`, and
`pg_notify` cache invalidation.

### What PR-B is NOT

- **No seeded built-ins.** `cors_presets` are **customer-owned and
  writable**. The `alert_presets` precedent (`migrations/00418_*`)
  seeds GLOBAL catalog rows with `account_id` deliberately NULL — the
  wrong shape for per-tenant writable presets. We skip seeding
  entirely; the customer is the only writer.
- **No plan-limit table additions.** Limits are pre-declared at
  `pkg/api/limits.go:601-645` (Pinned by
  `TestPlanCorsPresetLimits`). PR-B only adds the `errors.go`
  builders that reference the existing `CodePlanCorsPresetQuotaReached`
  constant and the new `CodePlanCorsPresetsNotAllowed` constant.
- **No back-compat window.** PR-A shipped to an empty table; no
  legacy rows to migrate.

### Intended outcome

Customer journey after PR-B:

1. Create a preset: `POST /v1/cors-presets` with `{name,
   allow_origins, ...}` → 201 with the canonical resource.
2. List presets: `GET /v1/cors-presets?app_id=...` → 200 with
   `[]CorsPreset`.
3. Compose a rule: `POST /v1/edge-rules` with `kind=cors`, `action:
   {cors_preset_id}` → 201; the preset's origins/methods/headers/
   credentials/max_age override rule-level values via
   `MergeCorsPresetIntoRule`.
4. Edit / delete: `PATCH /v1/cors-presets/{id}` /
   `DELETE /v1/cors-presets/{id}`.
5. Plan gates: Free tier → 402 `plan_cors_preset_not_allowed`
   (`pkg/api/limits.go:1382-1386` declares 0 for all CORS preset
   dimensions on Free); Hobby/Pro/Scale → enforced per the
   `pkg/api/limits.go:601-645` table.

## Decision

### 1. `edge_rules.cors_preset_id` is a nullable FK to `cors_presets(id)` with `ON DELETE SET NULL` (D1)

Three reasons:

- **No cascade delete of customer rules.** A customer deleting a
  preset they no longer want must not take down edge rules they still
  want. `ON DELETE SET NULL` keeps the rule in place; the runtime
  then fails-closed (see D3) so the rule stops matching CORS
  preflight until the customer wires a new preset or inlines fallback
  values.
- **Single resolved shape.** A FK nullable column + a single
  compile-time merge keeps `EdgeRuleCORSResolved` as one struct. No
  "preset + inline fallback" flag at the runtime boundary.
- **Migration economy.** One column + one FK + one partial index =
  one migration (slot 00428). No new tables, no new enums.

Concretely:

- `pkg/state.EdgeRuleCORSAction.CorsPresetID *string` (nullable).
- `pkg/state.pgstore.edgeRuleSelectCols` appends `, cors_preset_id`.
- `pkg/state.pgstore.scanEdgeRuleCols` adds the `&presetID` scan
  target.
- pgstore Create + Update SQL list the column (nullable).
- Memstore mirrors the same struct + paths.
- `state.CreateEdgeRuleParams` + `state.UpdateEdgeRuleParams` expose
  `CorsPresetID` so the apid handler passes it through.

### 2. `PresetID` and inline action fields are mutually exclusive (D2)

When `cors_preset_id` is set on a `kind=cors` rule's action, no inline
`allow_origins`, `allow_methods`, `allow_headers`, `expose_headers`,
`allow_credentials`, or `max_age_seconds` may be present. The preset
is the entire policy.

Two reasons:

- **No silent override.** Without mutual exclusivity, a customer who
  sets `allow_credentials: false` inline and `*` origins on the
  preset silently breaks their `*+credentials` guarantee when they
  later change the preset to set `allow_credentials: true`. The
  inline field shadows the preset but no audit/UI surfaces the
  override.
- **Same shape as ADR-127 sealed.env scope.** ADR-127 made the
  `sealed.env` PATH/CONTENT env vars mutually exclusive to prevent
  the "two env vars pointing at the same secret" surprise. PR-B
  applies the same pattern to the CORS preset vs inline surface.

Concretely:

- `api.EdgeRuleCORSAction.Validate` enforces: if `PresetID != nil`
  AND any inline field is non-empty / non-zero, return RFC 7807 with
  `code=cors_preset_inline_override_forbidden` and a doc URL.
- The DTO test matrix asserts the rejection on the wire.

### 3. Compile-time merge via `MergeCorsPresetIntoRule` (D3)

`compileCORSRules` (`cmd/gatewayd-internal/edge_rules.go:808-830`)
loads the preset via `ListCorsPresetsForAccount` overlay
`ListCorsPresetsForApp`, calls
`MergeCorsPresetIntoRule(accountID, appID, presetID,
rule.Action, preset)`, and emits the resolved action.

Three reasons:

- **Hot-path cost.** The runtime `applyEdgeRuleCORS`
  (`pkg/gateway/handler.go:1668-1773`) reads the resolved
  `EdgeRuleCORSResolved` (`pkg/gateway/edge_rules.go:203-216`). The
  merge happens once at rule compile and the result is cached.
  `pg_notify` invalidation triggers a recompile, not a per-request
  lookup.
- **Defense-in-depth footgun re-validation.** The apid handler
  validates `*+credentials` on create. The compile path re-validates
  in case the preset is edited independently — the rule author may
  have created the rule against a clean preset, then the preset was
  edited to add `allow_credentials: true` with `*` origins. The
  compile path catches this before the runtime.
- **Fail-closed on `ErrNotFound`.** FK `ON DELETE SET NULL` makes
  preset deletion a normal failure path. `MergeCorsPresetIntoRule`
  returns `ErrNotFound` if the preset was deleted between
  rule-create and rule-compile; the compile layer records a
  non-fatal compile error and the rule stops matching CORS preflight
  until wired again.

### 4. `pg_notify('cors_preset_changed', <account_id>)` for cache invalidation (D4)

Each pgstore write method (`CreateCorsPreset`, `UpdateCorsPreset`,
`DeleteCorsPreset`) emits a `pg_notify` on the
`cors_preset_changed` channel with the affected `account_id` as the
payload. gatewayd-internal subscribes via the existing pg-listen
machinery and reloads the account's preset overlay on receipt.

Three reasons:

- **Cross-PR proven pattern.** The existing `app_changed` and
  `edge_rule_changed` channels use the same machinery
  (`pkg/gateway/edge_rules_cache.go` is the closest precedent). No
  new infrastructure needed.
- **Re-emit on UPDATE and DELETE.** A preset's `app_id` transition
  (account-wide → app-scoped, or vice versa) invalidates both
  account and app overlays; re-emitting on UPDATE means the gatewayd
  reloads unconditionally, which is simpler than tracking which
  fields changed.
- **No write-side validation of `app_id` change.** A preset that
  transitions from account-wide to app-scoped is fine; the FK
  constraint to `apps(id)` is the only validation. UPDATE always
  invalidates.

Concretely:

- pgstore write methods call `pg_notify` after the SQL transaction
  commits (in the same transaction scope so a rolled-back write
  does not emit).
- gatewayd-internal's existing pg-listen loop picks up the new channel
  (additive change; the dispatcher routes the channel name to the
  `corsPresetsCache.ReloadAccount` handler).
- Per-write re-emit is a `INSERT/UPDATE/DELETE` on
  `cors_presets WHERE account_id = $1` — index-covered.

### 5. Wire-format change

`pkg/api/dto.go` — add `CorsPresetResponse`, `CreateCorsPresetRequest`,
`UpdateCorsPresetRequest`. Add `PresetID *string` to
`EdgeRuleCORSAction`. Extend `EdgeRuleCORSAction.Validate` for the
mutual-exclusivity check (D2).

`api/openapi.yaml` + `pkg/apid/openapi.yaml` get the same additions:
`CorsPreset` + request/response schemas + `/v1/cors-presets` paths +
`cors_preset_id` field on `EdgeRuleCORSAction` + plan-limit additions.
SDK regen (Node + Python + Go) follows per ADR-085 (`make spec-sync`
→ `make sdk-gen-*-check`).

### 6. Plan-gate error surface

`pkg/api/errors.go` — add two builders:

- `ErrPlanCorsPresetsNotAllowed(plan)` referencing new
  `CodePlanCorsPresetsNotAllowed` constant in `pkg/api/limits.go`
  (Free tier with `CorsPresetsPerAccount()==0`).
- `ErrPlanCorsPresetQuotaReached(plan, observed, limit)` referencing
  the existing `CodePlanCorsPresetQuotaReached` constant.

Both follow the RFC 7807 stable-`code` convention (§11).

## Consequences

- **+ Write API parity with data model.** Customers can finally author
  and manage presets.
- **+ Reusable CORS policies.** One preset drives N rules + N apps,
  reducing customer-side duplication and the
  `*+credentials`-footgun surface.
- **+ Hot-path cost.** Compile-time merge — runtime is unchanged.
- **+ Cache consistency.** `pg_notify` invalidation means edits are
  live within one cache reload cycle.
- **− FK ON DELETE SET NULL loses rule intent silently.** Mitigated
  by the D3 fail-closed semantics (rule stops matching; operator
  sees the compile error).
- **− Mutual exclusivity (D2) surprises customers used to inline
  override.** Documented in the ADR; a follow-up PR could add an
  `extends_preset_id` field if customer demand emerges.
- **− Wire-format change.** Additive only — existing customer rules
  with inline fields continue to work. New rules with both fields
  set are rejected (D2).
- **− New `pg_notify` channel.** Existing gatewayd-internal pg-listen
  dispatcher must route the new channel. The reload handler is
  additive (no existing behavior changes).

## Alternatives considered

### Inline-only references (Path B)

Keep `EdgeRuleCORSAction` inline-only; presets are a
documentation/organization feature only. Rejected — the data model
already exists, the compile helper already exists, and the runtime
hot-path is unchanged. Skipping the wiring leaves the feature inert.

### Cascade delete (Path C)

`ON DELETE CASCADE` on `edge_rules.cors_preset_id`. Rejected — a
customer deleting a preset would lose their edge rules. The data
loss class is unacceptable for a customer-owned table.

### Soft-delete preset (Path D)

Add `deleted_at` to `cors_presets`, treat soft-deleted rows as
absent. Rejected — adds a column for no operator benefit; the FK ON
DELETE SET NULL + fail-closed semantics already prevent customer
surprise.

### Per-app `ruleLabelSet` for preset cardinality

The new `cors_preset_id` label on edge-rule metrics is bounded by
the existing per-app `ruleLabelSet` (PR #1079). No new admission set
needed.

## Implementation notes

- Migration slot: **00428** (next free; highest in tree is
  `00427_request_telemetry.sql`). `migrations/README.md` documents
  the reservation fences at 00422-00426 — unrelated to this work.
- ADR-128 is the template for this ADR's structure.
- The compile helper signature `MergeCorsPresetIntoRule` is
  unchanged; only the `*string` nil-check at the call site in
  `compileCORSRules` is new.
- Per-write re-emit of `pg_notify` is the same pattern as the
  existing `app_changed` channel; no new infrastructure.
- Test files reference: `pkg/state/pgstore_cors_presets_test.go`
  (TestPgStore_CorsPreset_RoundTrip + TestPgStore_CorsPreset_FKOnDeleteSetNull +
  TestPgStore_CorsPreset_PgNotify); `cmd/apid/handlers_cors_presets_test.go`
  (validation matrix + plan quota + IDOR + audit);
  `cmd/gatewayd-internal/edge_rules_compile_cors_preset_test.go`
  (end-to-end + mutual exclusivity + footgun re-validation).
- Companion test for migration 00428:
  `migrations/00428_edge_rules_cors_preset_fk_test.go` pinning
  column existence, nullable, FK ON DELETE SET NULL behavior, index
  selectivity.