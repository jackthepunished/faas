-- filename: 00207_edge_rules_kind_validate.sql
-- +goose Up
-- +goose StatementBegin

-- Edge rule kind=validate (closed-vocabulary widening, ADR-091 D20 /
-- new ADR-0NN-edge-rule-validate). The action blob carries a JSON
-- Schema body that the gateway validates against the inbound
-- request before the wake gate; rejections return 422
-- `request_validation_failed` and never pay a cold-boot cost.
-- Schema storage is inline in `action` jsonb (single-table flow),
-- capped at api.MaxEdgeRuleValidateSchemaBytes (64 KiB) on the
-- pkg/api/dto.go::EdgeRuleValidateAction.Validate side. The new
-- kind is open to Free + every other plan (no plan gate; mirrors
-- ADR-091 D1's structure but does not extend IsPaidOnly).
--
-- DB-level widening only. The runtime extension (gateway matcher +
-- applier + pkg/edgevalidate + cli flag + openapi schema + spec
-- §4.1.2 backfill) lands in PR-B and PR-C of the same cluster;
-- this migration is the unblocker that lands first so PRs B/C
-- can compile + the e2e bitmask can exercise the kind.
--
-- DROP+ADD pair is canonical (migrations/00011_app_min_instances.sql
-- + 00075_app_runtime_node24_python313.sql) because Postgres 15
-- (CI) does not accept `ADD CONSTRAINT IF NOT EXISTS`. Using
-- IF EXISTS on the DROP keeps the migration replay-safe during a
-- local development re-run and during a hot-fix path that bypasses
-- the standard goose Up sequence. The constraint name
-- `edge_rules_kind_check` is the Postgres-assigned default for an
-- inline CHECK on `kind` (column-level convention).

ALTER TABLE edge_rules DROP CONSTRAINT IF EXISTS edge_rules_kind_check;
ALTER TABLE edge_rules ADD CONSTRAINT edge_rules_kind_check
  CHECK (kind IN ('route', 'rewrite', 'redirect', 'headers',
                  'cors', 'jwt', 'ip', 'validate'));

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

-- Reverse the widening. Validate-kind rules created between this
-- migration's apply and a downgrade become the violators and force
-- the downgrade to fail with 23514 — same safety contract as
-- 00075's reverse: a downgrade should never silently delete rule
-- rows, but the CHECK drop-and-re-ADD will reject the narrower
-- re-add before any row is touched, so an operator sees the
-- problem at downgrade time, not silently later.
ALTER TABLE edge_rules DROP CONSTRAINT IF EXISTS edge_rules_kind_check;
ALTER TABLE edge_rules ADD CONSTRAINT edge_rules_kind_check
  CHECK (kind IN ('route', 'rewrite', 'redirect', 'headers',
                  'cors', 'jwt', 'ip'));

-- +goose StatementEnd
