-- filename: 00224_apps_cors_defaults.sql
-- +goose Up
-- +goose StatementBegin

-- Per-app default CORS (issue #561 follow-up / new CORS improvements
-- section of ADR-091). The kind=cors edge rule remains the
-- authoritative shape; this migration adds a soft fallback so the
-- most common case (one origin, no edge rules) does not require the
-- customer to learn the edge-rule vocabulary.
--
--   cors_default_enabled boolean NOT NULL DEFAULT false
--       Master switch. When false (the default), the gateway
--       applies no default CORS and the "no rule → no CORS"
--       contract from spec §4.1.2.6 is preserved unchanged.
--       When true, the gateway consults cors_default_origins for
--       every request that misses a kind=cors edge rule.
--
--   cors_default_origins text[]
--       Origin allowlist for the default fallback. Same
--       string shape as edge_rules_cors.allow_origins; the
--       gateway reuses the matchOrigin matcher verbatim
--       (handler.go::matchOrigin widened in the same PR to
--       accept subdomain/port wildcards). Nullable to
--       distinguish "I forgot to fill it in" from "empty
--       list means deny all"; the gateway treats NULL and
--       empty list the same (deny all).
--
-- text[] is preferred over jsonb: (a) the gateway matcher
-- already accepts []string, (b) Postgres array equality is
-- deterministic and indexable if we ever need it, (c) the
-- existing edge_rules_cors.allow_origins column is also
-- text[] — same shape, same validation path.
--
-- The fallback runs AFTER the kind=cors match miss and BEFORE
-- the JWT/IP gates (handler.go::applyEdgeRuleCORS). The OPTIONS
-- short-circuit is intentionally SKIPPED on the default path:
-- the customer's backend is the authority on the preflight
-- answer, the gateway only stamps response headers. A
-- preflight still reaches the customer code; the response
-- gets Allow-Origin + Allow-Methods + Allow-Headers stamped
-- on the way out. This matches the user's intent ("just
-- allow my origin, no application code") without breaking
-- apps whose preflight handlers depend on running.

-- Replay-safe (PR #377 / ADR-041): ADD COLUMN IF NOT EXISTS. The two
-- columns are NOT NULL DEFAULT false (boolean, no rewrite) and
-- nullable text[] (no rewrite) — both metadata-only — so a second
-- MigrateUp is a no-op and the replay_safety_test harness stays
-- green. The original DROP COLUMN IF EXISTS pair (below) keeps the
-- down-migration idempotent for the same reason.
ALTER TABLE apps
  ADD COLUMN IF NOT EXISTS cors_default_enabled boolean NOT NULL DEFAULT false,
  ADD COLUMN IF NOT EXISTS cors_default_origins  text[];

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

-- Reverse the ADD. The columns are populated by user PATCH on
-- apps; the DROP loses those values, which is acceptable
-- because the fallback is opt-in (no data loss for customers
-- who never set it). A future upgrade is replay-safe because
-- the DEFAULT false makes the upgrade-side ADD backfill no
-- rows.

ALTER TABLE apps
  DROP COLUMN IF EXISTS cors_default_origins,
  DROP COLUMN IF EXISTS cors_default_enabled;

-- +goose StatementEnd
