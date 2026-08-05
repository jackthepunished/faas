-- filename: 00135_apps_eviction_priority.sql
-- +goose Up
-- Issue #475 — per-app eviction_priority ('best_effort'|'reserved').
-- Default 'best_effort' preserves the pre-#475 LRU-by-last_request_at
-- reaper behaviour bit-for-bit (spec §4.3). The plan gate is enforced
-- upstream in apid via api.Plan.EvictionPriorityReservedAllowed(); the
-- CHECK here is the data-integrity backstop (mirrors
-- apps_workload_class_chk from migration 00086 / apps_runtime_check).
--
-- One column + one CHECK:
--
--   eviction_priority  text NOT NULL DEFAULT 'best_effort'
--                       CHECK (eviction_priority IN ('best_effort', 'reserved'))
--
-- Behaviour:
--   - best_effort: the historical posture. Eligible for cross-account
--     RAM-pressure eviction under schedd's existing LRU-by-last_request_at
--     sort (pkg/sched/reaper.go::SelectEvictions, spec §4.3).
--   - reserved: still subject to account-level caps (per-account RAM
--     ceiling, plan concurrency cap, idle reaper) but NOT eligible for
--     cross-account RAM-pressure eviction. Every best_effort candidate
--     is exhausted before a reserved instance is evicted (the reaper
--     tier-order sort added in commit 4 of the PR cluster).
--   - Idle reaping and the per-app floor (MinInstances, ux_spec §6.5)
--     are tier-agnostic — reserved does not grant immortality. An idle
--     reserved instance is still parked after IdleTimeoutS.
--
-- Per-account cap on the reserved tier (Free 0, Hobby 1, Pro 2, Scale 4)
-- lives in pkg/api/limits.go::ReservedConcurrencyPerAccount and is
-- enforced in apid's updateApp path under an apps-row FOR UPDATE lock
-- (mirrors CreateCronIfUnderQuota).
--
-- No partial index on eviction_priority='reserved'. The per-account
-- cap counter is bounded to ~4 rows per account in the worst case and
-- runs under an apps-row lock; the apps_account_idx that backs the
-- owner filter already covers the read. A partial index would not
-- shrink any hot read and would only bloat the parked footprint on
-- a 100k-app fleet.
--
-- Replay-safe (PR #377 / ADR-041): ADD COLUMN IF NOT EXISTS + DO-block
-- pg_catalog.pg_constraint guard for the CHECK. Postgres has IF NOT
-- EXISTS for ADD COLUMN but not for ADD CONSTRAINT; the DO-block
-- pattern matches migrations/00082_apps_scaling_policy.sql:50-77,
-- 00109_apps_warm_snapshot.sql:44-64, and 00074_projects_and_workloads
-- .sql:86-104.
-- +goose StatementBegin

ALTER TABLE apps
    ADD COLUMN IF NOT EXISTS eviction_priority text NOT NULL DEFAULT 'best_effort';

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_catalog.pg_constraint
        WHERE conname = 'apps_eviction_priority_chk'
          AND conrelid = 'apps'::regclass
    ) THEN
        ALTER TABLE apps
            ADD CONSTRAINT apps_eviction_priority_chk
            CHECK (eviction_priority IN ('best_effort', 'reserved'));
    END IF;
END$$;

-- +goose StatementEnd

-- +goose Down
-- Forward-only: dropping the column would silently flip reserved apps
-- back to best_effort under RAM pressure, breaking the operator's
-- "this app is reserved" intent. Down is a no-op so an operator-
-- driven downgrade preserves data (mirrors migration 00131's
-- down-section rationale — pre-#557 divergence was a bug, not a
-- feature; rolling back would silently revoke the floor).
-- +goose StatementBegin
SELECT 1;
-- +goose StatementEnd