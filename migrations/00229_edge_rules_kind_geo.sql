-- filename: 00229_edge_rules_kind_geo.sql
-- +goose Up
-- +goose StatementBegin

-- ADR-091 D21 (kind=geo) — widen the closed set of `edge_rules.kind`
-- to admit `geo`. The 00192 schema CHECK is inline (line 48-50 of
-- migrations/00192_edge_rules.sql), so Postgres auto-assigned the
-- constraint name `edge_rules_kind_check` — the DROP+ADD pair anchors
-- to that exact name. The DROP+ADD pair is the canonical replay-safe
-- shape because Postgres 15 (CI) rejects `ADD CONSTRAINT IF NOT EXISTS`
-- (see migrations/00011_app_min_instances.sql for the precedent).
--
-- Why NOT the NOT VALID + VALIDATE pattern from 00206:
-- 00206 needed NOT VALID because the pre-existing rows had to satisfy
-- an evolving vocab (the audit event name `cron.fired.manually` was
-- being added). Here, we're ADDING a brand-new enum value that no
-- existing row uses (`geo` is unused in the current table by
-- construction — the column CHECK rejects it up to this migration).
-- A plain ADD CONSTRAINT is correct: validity check is free because
-- the new set is a strict superset of the old set.
--
-- This migration lands after 00228 (PR #867 / ADR-091 amendment
-- apps.maintenance_mode). PR #845 (this PR) renumbered kind=geo
-- through 00207 → 215 → 216 → 217 → 218 → 220 → 221 → 222 → 223
-- → 226 → 229 as main picked up sibling migrations that claimed
-- the earlier slots AND as open-PR sibling migrations collided on
-- 226/227/228 during the final hop. The 00223 → 00226 hop was
-- caused by PR #866 landing at 00224 with a 00223_reserve_slot.sql
-- coexistence fence aimed at PR #845 — rather than overwrite that
-- coordination, we stepped kind=geo to 00226. The 00226 → 00229
-- hop was caused by open-PR stampede: PR #864 (reqbudget PR1)
-- also claimed 00226 with a real schema, PR #867 (maintenance
-- PR-A) claimed 00227 (kind=maintenance) + 00228
-- (apps.maintenance_mode), and PR #873 (cli-secret-scan) fenced
-- 223-227 with reservation markers. Rather than litigate the
-- stampede, we stepped kind=geo to the next truly free slot
-- 00229. The fenced slots at 00217 + 00218 + 00221 + 00223 (PR
-- #866's) carry cross-PR coordination fences; the 00226/00227
-- collisions are settled by stepping.
--
-- Sub-decision hop (D21) deviates from the team's ADR-091 D14 default
-- of "Hobby+ only" for non-trivial edge-rule kinds: geo is allowed on
-- ALL plans including Free, but with a tighter per-app quota
-- (EdgeRulesGeoPerApp=1 for Free, see pkg/api/limits.go). The shift is
-- driven by the abuse-desk customer persona — a Free-tier customer who
-- needs to "block everything except DE" gets one rule. The upgrade path
-- to Hobby+ raises the cap to 5.

ALTER TABLE edge_rules
    DROP CONSTRAINT IF EXISTS edge_rules_kind_check;

ALTER TABLE edge_rules
    ADD CONSTRAINT edge_rules_kind_check
    CHECK (kind IN ('route','rewrite','redirect','headers','cors','jwt','ip','validate','limit','geo'));

-- +goose StatementEnd

-- +goose Down
-- Forward-only by design (mirrors migrations/00206_*.sql:58-67 +
-- migrations/00141_app_webhook_deliveries.sql:110-117). Reverting
-- the widening would orphan any existing `kind='geo'` rows the
-- operator may have created, so we preserve the wider constraint
-- unconditionally on downgrade.
-- +goose StatementBegin
SELECT 1;
-- +goose StatementEnd
