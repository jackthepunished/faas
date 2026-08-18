-- filename: 00285_pg_ratelimit_add_rule_scope.sql
-- +goose Up
-- +goose StatementBegin

-- ADR-104 amendment 5 (issue #881 Phase 4 follow-up, 2026-08-18) —
-- widen the closed set of `pg_ratelimit_counters.scope` to admit
-- `rule` so per-rule buckets (kind=throttle rules, keyed on rule ID)
-- share the same central counter row shape as per-app + per-account.
--
-- Phase 3 (PR #909 / commit 82d62881, merged 2026-08-18) added the
-- in-process rule bucket, but the *central* counter (opt-in via
-- `[ratelimit] mode = "central"`) was the rejected alternative in
-- ADR-104 amendment 4. Amendment 5 flips that decision: central mode
-- is now the recommended posture for production multi-replica clusters
-- and must accept `scope='rule'` so per-rule buckets participate in
-- the same cross-replica serialisation as per-app + per-account.
--
-- The 00126 schema CHECK is inline (line 64 of 00126_pg_ratelimit.sql,
-- the same pattern 00229 used on edge_rules_kind_check). Postgres
-- auto-assigned the constraint name `pg_ratelimit_counters_scope_check`
-- — the DROP+ADD pair anchors to that exact name.
--
-- Replay-safe shape (matches 00229 + 00206 precedents):
--   DROP CONSTRAINT IF EXISTS  -- idempotent on re-apply
--   ADD CONSTRAINT ... CHECK (...)  -- new superset
-- The new set is a strict superset of the old (`rule` is unused in the
-- current table by construction — the column CHECK rejects it up to
-- this migration), so a plain ADD CONSTRAINT is correct. NOT VALID +
-- VALIDATE is not needed (cf. 00229:18-25).
--
-- PK shape is unchanged: `(scope, subject_id, plan)`. For `scope='rule'`
-- the subject_id is the rule UUID (the migration stores it as
-- uuid to satisfy the existing column type — no shape change). Per-
-- consumer central mode is Phase 4; the PK does NOT include
-- consumer_id and is out of scope here.

ALTER TABLE pg_ratelimit_counters
    DROP CONSTRAINT IF EXISTS pg_ratelimit_counters_scope_check;

ALTER TABLE pg_ratelimit_counters
    ADD CONSTRAINT pg_ratelimit_counters_scope_check
    CHECK (scope IN ('app', 'account', 'rule'));

-- +goose StatementEnd

-- +goose Down
-- Forward-only by design (mirrors 00229 + 00206: reverting the
-- widening would orphan any existing `scope='rule'` rows the gateway
-- may have written under central mode. Preserve the wider constraint
-- unconditionally on downgrade.)
-- +goose StatementBegin
SELECT 1;
-- +goose StatementEnd