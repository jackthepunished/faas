-- filename: 00219_edge_rules_kind_limit.sql
-- +goose Up
-- +goose StatementBegin

-- Edge rule kind=limit (closed-vocabulary widening, ADR-091 D24 /
-- new ADR-0NN-edge-rule-limit). The action blob carries a single
-- `max_body_bytes` integer (and an optional streaming counterpart
-- `max_body_bytes_streaming`) that the gateway enforces on the
-- inbound request body before the wake gate. A request whose
-- declared Content-Length exceeds the cap is rejected with 413
-- `request_too_large` before any compute is woken; chunked
-- requests are bound via `http.MaxBytesReader` on `r.Body`.
-- Rejections short-circuit before the auth chain, the rate
-- limiter, and Backend.Pick — the cheapest possible deny path.
--
-- The new kind is open to Free + every other plan (no plan gate;
-- mirrors kind=validate's posture). The hard ceiling on
-- `max_body_bytes` is `api.MaxRequestBodyBytes` (25 MiB on the
-- buffered path); `max_body_bytes_streaming` is capped at
-- `api.MaxBodyBytesStreaming` (100 MiB, on the streaming opt-in).
--
-- DB-level widening only. The runtime extension (gateway matcher +
-- applier + DTO + cli flag + openapi schema + spec §4.1.2
-- backfill) lands in the same PR.
--
-- DROP+ADD pair is canonical (migrations/00214_edge_rules_kind_validate.sql
-- + 00011_app_min_instances.sql + 00075_app_runtime_node24_python313.sql)
-- because Postgres 15 (CI) does not accept `ADD CONSTRAINT IF
-- NOT EXISTS`. Using IF EXISTS on the DROP keeps the migration
-- replay-safe during a local development re-run and during a
-- hot-fix path that bypasses the standard goose Up sequence. The
-- constraint name `edge_rules_kind_check` is the Postgres-assigned
-- default for an inline CHECK on `kind` (column-level convention).
--
-- Ordering hazard vs PR #845 (kind=geo, ADR-091 D21-D23): the geo
-- migration's CHECK widening is also a full-vocabulary literal
-- rewrite of the same `edge_rules_kind_check` constraint, and
-- both migrations were authored under the same constraint-name
-- pattern. When #845 lands, its migration MUST widen its CHECK
-- to include `'limit'` (10 values total) — otherwise the rule rows
-- this migration accepts become 23514 violators under the
-- narrower CHECK. The ADR documents this as the
-- first-match-wins-not-smallest-cap-wins contract on rewrites of
-- the same CHECK. The reverse direction (this migration lands
-- first, then #845 widens to 10 values) is also safe and the
-- preferred ordering.

ALTER TABLE edge_rules DROP CONSTRAINT IF EXISTS edge_rules_kind_check;
ALTER TABLE edge_rules ADD CONSTRAINT edge_rules_kind_check
  CHECK (kind IN ('route', 'rewrite', 'redirect', 'headers',
                  'cors', 'jwt', 'ip', 'validate', 'limit'));

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

-- Reverse the widening. Limit-kind rules created between this
-- migration's apply and a downgrade become the violators and force
-- the downgrade to fail with 23514 — same safety contract as
-- 00214's reverse: a downgrade should never silently delete rule
-- rows, but the CHECK drop-and-re-ADD will reject the narrower
-- re-add before any row is touched, so an operator sees the
-- problem at downgrade time, not silently later.
ALTER TABLE edge_rules DROP CONSTRAINT IF EXISTS edge_rules_kind_check;
ALTER TABLE edge_rules ADD CONSTRAINT edge_rules_kind_check
  CHECK (kind IN ('route', 'rewrite', 'redirect', 'headers',
                  'cors', 'jwt', 'ip', 'validate'));

-- +goose StatementEnd