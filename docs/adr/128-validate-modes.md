# ADR-128 · Edge-rule validate_mode: top-level authority + admission gate

- **Status:** proposed
- **Date:** 2026-08-24
- **Issue / PR:** [#975 item #3](https://github.com/poyrazK/faas/issues/975) (mega-API-native; also closes §17 G19.4 follow-on)
- **Decision:** Promote `edge_rules.validate_mode` from JSONB action field
  (`action.validate_mode`) to top-level column. Add a per-app
  `ruleLabelSet` admission gate so the new failure metric
  `gateway_validate_failures_total{app_id, rule_id, mode, reason}` cannot
  blow up Prometheus cardinality. Keep `action.validate_mode` as a
  read-side fallback for the deprecation window (1 release).

## Context

The validate path ships three enforcement modes for `kind='validate'`
edge rules today:

- **`observe`** — counter only, allow the request through.
- **`warn`** — counter + `X-Validation-Warning: <rule_id>` response header
  via the `statusRecorder` (`pkg/gateway/handler.go:5924-6024`), allow.
- **`block`** (default) — 422 with the existing RFC 7807 problem +
  field-error array.

The runtime path is wired end-to-end
(`pkg/gateway/handler.go:2519-2806::applyEdgeRuleValidate`):
mode dispatch happens at lines 2692-2806, reading
`rule.ValidateMode` from the resolved rule
(`pkg/gateway/edge_rules.go:280-301::EdgeRuleValidateResolved`) and
defaulting an empty string to `block`. Audit events
(`edge_rule.validate_failed` with `mode` in the data payload) fire on
all three arms; the `apply` and `match` counter triplets fire alongside
the new per-`{mode,reason}` failure counter.

What's **not** done:

1. **Schema authority.** Migration `00293_validate_mode.sql` (already
   merged) added the top-level column with the right NOT NULL DEFAULT
   'block' and CHECK constraint. But `pkg/state.EdgeRule` doesn't have a
   `ValidateMode` field, `pgstore.edgeRuleSelectCols`
   (`pkg/state/pgstore.go:7370-7372`) doesn't include it,
   `scanEdgeRuleCols` (`pkg/state/pgstore.go:7410-7430`) doesn't bind it,
   and the Create/Update SQL at `pkg/state/pgstore.go:7445-7455` +
   `:7812-7823` omits it. The runtime reads mode exclusively from the
   JSONB action field — `cmd/gatewayd-internal/edge_rules.go:1551`
   sets `ValidateMode: action.ValidateMode` and that's the only source
   today. The top-level column is dead schema.

2. **Failure metric labels.** The metric shipped as
   `gateway_edge_rule_validate_failures_total{mode, reason}` at
   `pkg/gateway/metrics.go:570-573`. The issue spec is
   `gateway_validate_failures_total{app_id, rule_id, mode, reason}` —
   operators need per-rule localization, not just the cardinality of
   the rule's mode. The current metric cannot answer "which rule on app
   X is failing most often".

3. **Migration companion test.** Migration 00293 was merged without a
   `migrations/00293_*_test.go` companion — `00345_edge_rules_kind_cache_test.go`
   shows the convention.

This ADR formalises the source-of-truth resolution (top-level wins),
the deprecation window for the JSONB field, and the admission-set
strategy for the new high-cardinality label pair.

## Decision

### 1. `edge_rules.validate_mode` is the source of truth (D1)

Three reasons:

- **Spec compliance.** Issue #975 #3 says "ALTER TABLE edge_rules ADD
  COLUMN validate_mode". The migration author followed the spec — the
  Store layer just never caught up.
- **Hot-path cost.** JSONB extraction on every rule lookup vs a direct
  column read is a measurable difference at the gatewayd-internal
  scale. Per-rule column read is cheaper than jsonb_extract_path_text.
- **Observability.** Operators want to query `WHERE validate_mode='observe'`
  in pg directly without `jsonb_path_query_array`. The column makes
  that trivial.

Concretely:

- `pkg/state.EdgeRule.ValidateMode string` (top-level).
- `pkg/state.pgstore.edgeRuleSelectCols` appends `, validate_mode`.
- `pkg/state.pgstore.scanEdgeRuleCols` adds the `&mode` scan target.
- pgstore Create + Update SQL list the column.
- Memstore mirrors the same struct + paths.
- `state.CreateEdgeRuleParams` + `state.UpdateEdgeRuleParams` expose
  `ValidateMode` so the apid handler passes it through.

### 2. `action.validate_mode` is a read-side fallback for one release (D2)

Rows created before this PR shipped with `validate_mode` only in the
JSONB action (the top-level column's default 'block' doesn't reflect
the customer's actual mode). For the deprecation window (1 release):

- Loader reads `state.EdgeRule.ValidateMode` first.
- If empty AND `action.ValidateMode` is non-empty, action wins.
- The action JSON field stays in the response wire format as
  `deprecated: true` so existing customer automation doesn't break.

Post-deprecation (next PR cycle), the loader drops the fallback and the
action field is removed from the response surface.

### 3. `ruleLabelSet` admission gate for `(app_id, rule_id)` (D3)

The new label pair is unbounded — a customer can have N apps each with
M rules. Without an admission set, a single noisy tenant could exhaust
the Prometheus label budget. Mirror PR #1070's `boxLabelSet` pattern
(`pkg/wire/metrics.go:5868-5950`):

- `pkg/wire/ruleLabelSet` — per-app LRU of rule IDs, cap 256, mutex +
  fixed-size map. Overflow → `__other__` collapsed value.
- Closed reason + mode vocabs pre-instantiated at metric registration
  (no dynamic label allocation on the hot path).
- Property test: 10k fuzzed `(appID, ruleID)` pairs per app → ≤ 257
  distinct labels per app.

**Cardinality budget:** per app, ≤ (256 rules + 1 `__other__`) × 4 modes
× 6 reasons = 6168 series worst case (modes: observe, warn, block,
other — `other` is the coerce-on-unknown bucket per the closed
vocab). Well under Prometheus per-instance
label budget.

### 4. Wire-format change

`pkg/api/dto.go` — promote `ValidateMode` from
`EdgeRuleValidateAction.ValidateMode` to top-level
`EdgeRuleResponse.ValidateMode`, `EdgeRuleCreateRequest.ValidateMode`,
`EdgeRuleUpdateRequest.ValidateMode`. The action-level field stays as
a deprecated pointer (`omitempty`) on the response for one release.

`api/openapi.yaml` + `pkg/apid/openapi.yaml` get the same move:
top-level `validate_mode` on `EdgeRuleResponse` / `EdgeRuleCreate` /
`EdgeRuleUpdate`; `EdgeRuleValidateAction.validate_mode` marked
`deprecated: true`. SDK regen (Node + Python + Go) follows per
ADR-085 (`make spec-sync` → `make sdk-gen-*-check`).

### 5. Metric rename with shadow period

Rename `gateway_edge_rule_validate_failures_total` →
`gateway_validate_failures_total` (matches the issue spec verbatim).
For the deprecation window, **also emit the old name** so existing
dashboards keep working. After one release, drop the old name.

This is the same pattern ADR-097 / ADR-074 used when renaming the
wake-tier metric. The dual emission is the only reason the dashboard
team has to coordinate — release notes call it out.

## Consequences

- **+ Single source of truth.** No more dual-field drift between
  `action.validate_mode` and the top-level column.
- **+ Observability.** `WHERE validate_mode='observe'` in pg; per-rule
  metric labels in Prometheus.
- **+ Hot-path cost.** Direct column read vs JSONB extraction.
- **− Migration companion test required.** Migration 00293 ships a
  back-fill companion test in this PR.
- **− Wire format change.** Existing customer automation reading
  `action.validate_mode` gets the deprecated value for one release, then
  the field drops. Release notes must call this out.
- **− Metric rename.** Dashboards referencing the old name get a shadow
  emission for one release.
- **− One release of dual-write complexity.** Loader does top-level +
  JSONB fallback read; apid does top-level write.

## Alternatives considered

### Drop the top-level column (Path B)

Make `action.validate_mode` authoritative, drop the column in a new
migration (slot 00417). Smaller PR but diverges from the issue spec +
the migration author's apparent intent. Operators can't query pg
directly. Rejected — the column is the right shape.

### Hybrid (Path C)

Top-level column authoritative on read AND write. Action JSON kept as
read-only fallback. Same as Path A; the only difference is Path A
already aligns with this. Considered synonymous.

### Wider gate (per-account `ruleLabelSet`)

Bound the label set across the whole account rather than per app. A
single noisy account with many apps could starve less-active apps.
Per-app is fairer; per-account rejected.

### No admission gate

Trust the customer to keep rule counts bounded. Rejected — the issue
spec asks for `{app_id, rule_id, mode, reason}` and the only way to
ship that safely is with an admission gate. The 256-per-app cap is
generous (typical app has <50 rules).

## Implementation notes

- Slot pick: no new migration needed — `00293_validate_mode_test.go` is
  a new companion test (test files don't consume slots). If we
  add a follow-up cleanup migration in D2's window, slot 00417+
  (above current max 00416 from PR #1049).
- Cross-PR slot precheck at PR-open time:
  `scripts/ci/check_migration_slots.sh`.
- The pattern for the admission gate mirrors PR #1070's `boxLabelSet`
  exactly. Same property test (10k fuzzed keys → ≤ cap + 1 distinct).
- Test files reference: `pkg/state/pgstore_edge_rules_test.go`
  (TestPgStore_EdgeRule_RoundTrip lines 95-171 — add ValidateMode
  round-trip); `pkg/wire/metrics_box_label_set_test.go` (pattern mirror
  for ruleLabelSet); `pkg/gateway/edge_rules_validate_mode_e2e_test.go`
  lines 93-196 (extend mode coverage with label assertions).
