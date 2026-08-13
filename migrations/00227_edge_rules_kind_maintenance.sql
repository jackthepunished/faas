-- filename: 00227_edge_rules_kind_maintenance.sql
-- +goose Up
-- +goose StatementBegin

-- Edge rule kind=maintenance (closed-vocabulary widening, ADR-091
-- amendment, PR-B). The action blob carries optional
-- `retry_after_seconds` (per-rule Retry-After override, defaults to
-- the platform constant api.EdgeRuleMaintenanceRetryAfterSeconds =
-- 60 s when 0; hard cap api.MaxEdgeRuleMaintenanceRetryAfterSeconds
-- = 86400 / 24 h enforced at apid-create time) and an optional
-- `message` (≤ 512 B; surfaces as Problem.detail). The gateway
-- applier (pkg/gateway.(*Handler).applyEdgeRuleMaintenance,
-- §4.1.2.13) short-circuits a matched (host, path, http_method)
-- request with 503 + Retry-After BEFORE auth, BEFORE wake — the
-- cheapest possible deny path. Distinct from the coarse sibling
-- apps.maintenance_mode (§4.1.2.0) which fires on the whole app.
--
-- The new kind is open to Free + every other plan (no plan gate;
-- mirrors kind=validate / kind=limit posture). Cross-account rules
-- fall through silently (audit + outcome=blocked metric) — same
-- ADR-091 D5 same-account defence-in-depth.
--
-- DROP+ADD pair is canonical (migrations/00214, 00219, 00011,
-- 00075) because Postgres 15 (CI) does not accept `ADD CONSTRAINT
-- IF NOT EXISTS`. The constraint name `edge_rules_kind_check` is
-- the Postgres-assigned default for an inline CHECK on `kind`.
--
-- Ordering hazard vs PR #845 (kind=geo, ADR-091 D21-D23): when
-- #845 lands, its migration MUST widen its CHECK to include
-- 'maintenance' (11 values total). The reverse ordering (this
-- migration lands first, then #845 widens to 11 values) is the
-- preferred ordering per the first-match-wins-not-smallest-cap-
-- wins contract on rewrites of the same CHECK documented in
-- 00219.
--
-- Pre-reserved at PR-A by migrations/00227_reserve_slot.sql.

ALTER TABLE edge_rules DROP CONSTRAINT IF EXISTS edge_rules_kind_check;
ALTER TABLE edge_rules ADD CONSTRAINT edge_rules_kind_check
  CHECK (kind IN ('route', 'rewrite', 'redirect', 'headers',
                  'cors', 'jwt', 'ip', 'validate', 'limit', 'maintenance'));

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

-- Reverse the widening. Maintenance-kind rules created between
-- this migration's apply and a downgrade become the violators and
-- force the downgrade to fail with 23514 — same safety contract
-- as 00214 / 00219's reverse.
ALTER TABLE edge_rules DROP CONSTRAINT IF EXISTS edge_rules_kind_check;
ALTER TABLE edge_rules ADD CONSTRAINT edge_rules_kind_check
  CHECK (kind IN ('route', 'rewrite', 'redirect', 'headers',
                  'cors', 'jwt', 'ip', 'validate', 'limit'));

-- +goose StatementEnd