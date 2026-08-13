-- filename: 00244_edge_rules_kind_budget.sql
-- +goose Up
-- +goose StatementBegin

-- Edge rule kind=budget (ADR-093 §Decision). The action blob
-- carries a single `budget_ms` integer (milliseconds, ≥ 1, ≤
-- api.RequestBudgetMaxMs) and an optional `allow_override_header`
-- string naming an HTTP header whose numeric value (if present on
-- the inbound request) overrides `budget_ms` for that single
-- request. The runtime is `pkg/reqbudget` — the gateway matcher
-- resolves the matched rule and stamps the budget onto the inbound
-- ctx via `reqbudget.WithRemaining`, and every downstream hop
-- (JWT verify, forward, gRPC, DB) propagates the remaining time
-- via `reqbudget.WithOverhead` / `WithCeiling`. Deadline fire is
-- surfaced as 504 + RFC 7807 `code: request_budget_exceeded`.
--
-- Plan posture mirrors kind=validate / kind=limit / kind=geo /
-- kind=maintenance: open to Free and every other plan (no
-- IsPaidOnly change). The default budget when no rule matches is
-- the per-plan budget on the plan row (`plan.request_budget_ms`,
-- default 0 → falls back to api.RequestBudgetDefault = 3 s).
-- Per-app overrides via kind=budget take precedence over the plan
-- default at request time; the allow_override_header is the
-- per-customer-tunable knob (default `x-faas-budget-ms`).
--
-- DB-level widening only. The runtime extension (gateway matcher +
-- applier + DTO + cli flag + openapi schema + spec §4.1.2
-- backfill + cmd/apid/handlers_edge_rules.go wiring + the entire
-- `pkg/reqbudget` library) lands in the same PR (mega-PR cluster).
--
-- DROP+ADD pair is canonical (migrations/00236
-- + 00229 + 00219_edge_rules_kind_limit.sql
-- + 00214_edge_rules_kind_validate.sql + 00011_app_min_instances.sql +
-- 00075_app_runtime_node24_python313.sql) because Postgres 15 (CI)
-- does not accept `ADD CONSTRAINT IF NOT EXISTS`. Using IF EXISTS
-- on the DROP keeps the migration replay-safe during a local
-- development re-run and during a hot-fix path that bypasses the
-- standard goose Up sequence. The constraint name
-- `edge_rules_kind_check` is the Postgres-assigned default for an
-- inline CHECK on `kind` (column-level convention).
--
-- Ordering hazard vs prior PRs: every CHECK widening is a full-
-- vocabulary literal rewrite of the same `edge_rules_kind_check`
-- constraint. As long as the final widening in any merge window
-- includes the union of all concurrent additions, intermediate
-- migrations can land in any order — the rewrite contract is
-- "post-rewrite = pre-rewrite ∪ {new values}". 00214 (validate),
-- 00219 (limit), 00229 (geo, PR #845), 00236 (maintenance, PR-B
-- post-272-preview) all do this; this migration extends the same
-- convention to include 'budget'. The IN list is the union of all
-- post-00219 vocab values to date. A future widening that adds
-- another kind MUST update the IN list together with the
-- pre-existing-vocab walk loop in 00244's test, or the test will
-- catch the regression via 23514 on a known kind (CHECK-rewrite
-- race, see PR #864 CI run 31705973056 + memory
-- migration-gates-collision-and-replay.md).
--
-- Slot choice: 00244 is the lowest unclaimed slot after main's
-- 00237 (apps_maintenance_mode) and PR #884's fences 00238-00243
-- (tenant_surfaces PR-0, ADR-099 cluster, issue #879). Future
-- renumbering must re-verify `git ls-tree origin/main migrations/`
-- AND enumerate open-PR fence claims
-- (cross-pr-slot-gate-reservation-fence-pattern) after every
-- rebase, per migration-test-uuid-sed-residual and
-- pr-845-edge-rules-geo-slot-chase-2026-08-11.

ALTER TABLE edge_rules DROP CONSTRAINT IF EXISTS edge_rules_kind_check;
ALTER TABLE edge_rules ADD CONSTRAINT edge_rules_kind_check
  CHECK (kind IN ('route', 'rewrite', 'redirect', 'headers',
                  'cors', 'jwt', 'ip', 'validate', 'limit',
                  'geo', 'maintenance', 'budget'));

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

-- Reverse the widening. Budget-kind rules created between this
-- migration's apply and a downgrade become the violators and force
-- the downgrade to fail with 23514 — same safety contract as 00219's
-- reverse, 00214's reverse, 00229's reverse, and 00236's reverse: a
-- downgrade should never silently delete rule rows, but the CHECK
-- drop-and-re-ADD will reject the narrower re-add before any row
-- is touched, so an operator sees the problem at downgrade time,
-- not silently later. The narrower reverse keeps 'geo' and
-- 'maintenance' so downgrades don't silently drop them either —
-- parallel contract to the UP path.
ALTER TABLE edge_rules DROP CONSTRAINT IF EXISTS edge_rules_kind_check;
ALTER TABLE edge_rules ADD CONSTRAINT edge_rules_kind_check
  CHECK (kind IN ('route', 'rewrite', 'redirect', 'headers',
                  'cors', 'jwt', 'ip', 'validate', 'limit',
                  'geo', 'maintenance'));

-- +goose StatementEnd
