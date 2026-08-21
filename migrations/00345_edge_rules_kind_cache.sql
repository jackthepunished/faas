-- filename: 00345_edge_rules_kind_cache.sql
-- +goose Up
-- +goose StatementBegin

-- Edge rule kind=cache (ADR-122). The action blob carries
-- `max_age_seconds` (int, 1..86400), `stale_if_error_seconds`
-- (int, 0..300), `vary_on` (string array of non-credential header
-- names) and `methods` (string array, subset of {GET, HEAD}).
-- The runtime is `pkg/gateway/response_cache.go` — an in-process
-- bounded LRU per gatewayd-internal. No customer response body is
-- ever written to disk or to Postgres; the cache dies with the
-- process. `applyEdgeRuleCache` slots into ServeHTTP AFTER the
-- auth gates (a hit must never bypass require_authn /
-- public_auth) and BEFORE the wake gate (a hit must never wake a
-- microVM — that is the economic point of the feature).
--
-- Plan posture mirrors kind=validate / kind=limit / kind=geo /
-- kind=maintenance / kind=budget: no IsPaidOnly change. The
-- guardrail is a per-app count (Limits.EdgeRulesCachePerApp:
-- Free 0, Hobby 1, Pro 5, Scale 20) enforced inside the existing
-- CreateEdgeRuleIfUnderQuota FOR UPDATE lock. Free gets 0 because
-- cached bodies occupy shared node RAM, which is the scarce
-- resource here — not because caching is a security control.
--
-- DB-level widening only. The runtime extension (store + matcher
-- + applier + DTO + limits + cli flags + openapi schema + spec
-- §4.1.2 backfill + cmd/apid/handlers_edge_rules.go wiring) lands
-- in the same mega-PR.
--
-- DROP+ADD pair is canonical (00214 / 00219 / 00229 / 00236 /
-- 00254 / 00265) because Postgres 15 (CI) does not accept
-- `ADD CONSTRAINT IF NOT EXISTS`. IF EXISTS on the DROP keeps the
-- migration replay-safe on a local re-run and on a hot-fix path
-- that bypasses the standard goose Up sequence. The constraint
-- name `edge_rules_kind_check` is the Postgres-assigned default
-- for an inline CHECK on `kind` (column-level convention).
--
-- ─────────────────────────────────────────────────────────────
-- REGRESSION FIX: this migration restores 'budget'.
-- ─────────────────────────────────────────────────────────────
-- 00254 (kind=budget) widened the vocabulary to include 'budget'.
-- 00265 (kind=throttle) then rewrote the SAME constraint with a
-- 12-value list that OMITTED 'budget' — see 00265:69-75, whose
-- comment assumed kind=budget occupied slot 00231 and would
-- therefore land either before or after with an identical final
-- shape. It did not: budget shipped at 00254, i.e. BEFORE 00265,
-- so 00265's DROP+ADD silently narrowed the vocabulary and
-- dropped 'budget' back out.
--
-- Consequences on main today:
--   * schema.sql:1353 (the generated post-migration dump) lists
--     12 kinds; 'budget' is absent — the authoritative evidence.
--   * Any customer creating a kind=budget edge rule gets SQLSTATE
--     23514 on insert, even though cmd/apid/handlers_edge_rules.go
--     (:170 validate, :506 marshal), the CLI closed vocabulary
--     (edgeRuleKindVocab) and api/openapi.yaml all accept it. The
--     feature is wired end-to-end above the database and rejected
--     at it.
--   * TestMigrations_00254_EdgeRulesKindBudget runs the FULL
--     migration set (so 00265 applies after it) and then asserts
--     every value of budgetMigrationVocab — including 'budget' —
--     is present. That test has been failing whenever it actually
--     executes; it is green in most runs only because
--     pgtest.Open skips when DATABASE_URL is unset.
--
-- The IN list below is therefore the true union of every shipped
-- kind (13) plus 'cache' (14). This honours the rewrite contract
-- the earlier migrations state explicitly: "post-rewrite =
-- pre-rewrite ∪ {new values}". Restoring 'budget' here is not
-- scope creep — a widening migration that re-narrowed the
-- vocabulary would perpetuate the same class of bug it is meant
-- to avoid.
--
-- A future widening that adds another kind MUST carry all 14
-- values forward. The 00345 test asserts the full union, so a
-- re-narrowing is caught by 23514 on a known kind.
--
-- Slot choice: 00345 is the next free slot above every open
-- claim. Main now carries its own reserve_slot fences at
-- 00314-00317 + 00320 + 00328-00340 + the real migrations at
-- 00318 (deployments_actor), 00319 (actor_validate_fk) and
-- 00341 (repair_app_secrets_scope). Of the open PRs: #984 holds
-- 00342 (real deployments_annotation) + a 00343 reservation;
-- #1005 holds reservations at 00342 + a real at 00344
-- (deployment_openapi_snapshots). 00345 is therefore the lowest
-- slot that no open PR has claimed. Earlier fences accompanying
-- this branch (00314-00320, 00330) shadowed main's identical
-- reserve_slot fences + #1000's 00329 reservation and were
-- dropped per the cross-PR precheck fence carve-out
-- (pr-999-merged-fence-contiguity-backfire). Re-verify with
-- `git ls-tree origin/main migrations/` AND `git ls-tree
-- refs/pull/<N>/head migrations/` for every N in the open-PR
-- list (the per-PR precheck is the only one that sees open-PR
-- fences) after every rebase.
--
-- This branch carries vacated-slot fences at 00342, 00343 and
-- 00344 — temporary `-- filename: NNNNN_reserve_slot.sql`
-- no-op migrations that fill the gap between 00341
-- (repair_app_secrets_scope, the last contiguous main-side file
-- after rebase) and 00345. Without them the local
-- TestMigrationsContiguous check would see a 3-slot hole
-- (per `local-embed-vs-synthetic-merge-contiguity`). The cross-PR
-- precheck's `slots_from_paths` carve-out ignores reservations,
-- so 00342/00343/00344 fences do not count as slot claims here
-- and the gate stays green. They will be reaped automatically
-- on the first rebase that lands a real migration at any of the
-- three slots: if #984 merges first, its 00342
-- `deployments_annotation.sql` and 00343 fence shadow ours, and
-- the next rebase drops our equivalents. If #1005 merges
-- first, its 00344 `deployment_openapi_snapshots.sql` shadows
-- ours. Either ordering is clean; the local contiguity test is
-- what the fences exist to satisfy today.

ALTER TABLE edge_rules DROP CONSTRAINT IF EXISTS edge_rules_kind_check;
ALTER TABLE edge_rules ADD CONSTRAINT edge_rules_kind_check
  CHECK (kind IN ('route', 'rewrite', 'redirect', 'headers',
                  'cors', 'jwt', 'ip', 'validate', 'limit', 'geo',
                  'maintenance', 'throttle', 'budget', 'cache'));

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

-- Reverse the widening. Cache-kind rules created between this
-- migration's apply and a downgrade become the violators and
-- force the downgrade to fail with 23514 — same safety contract
-- as 00214 / 00219 / 00229 / 00236 / 00254 / 00265's reverses: a
-- downgrade must never silently delete rule rows, so the
-- drop-and-re-ADD rejects the narrower re-add before any row is
-- touched and the operator sees the problem at downgrade time
-- rather than silently later.
--
-- The reverse deliberately KEEPS 'budget'. Restoring the literal
-- pre-00345 state would mean re-dropping 'budget', i.e.
-- reintroducing the 00265 regression documented above. A
-- downgrade should undo this migration's feature (cache), not
-- resurrect a known bug in a neighbouring one. Every other kind
-- ('geo', 'maintenance', 'throttle', 'budget') is carried
-- forward for the same reason the UP path carries them.
ALTER TABLE edge_rules DROP CONSTRAINT IF EXISTS edge_rules_kind_check;
ALTER TABLE edge_rules ADD CONSTRAINT edge_rules_kind_check
  CHECK (kind IN ('route', 'rewrite', 'redirect', 'headers',
                  'cors', 'jwt', 'ip', 'validate', 'limit', 'geo',
                  'maintenance', 'throttle', 'budget'));

-- +goose StatementEnd
