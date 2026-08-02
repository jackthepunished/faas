-- +goose Up
-- +goose StatementBegin

-- Issue #463 / ADR-066 — Sidecar containers (init + sidecar, hard cap 2).
--
-- Adds a single JSONB column `deployments.sidecars` (`NOT NULL DEFAULT
-- '[]'::jsonb`) with a CHECK constraint pinning the 2-sidecar cap at
-- the schema layer. JSONB is adequate for the 2-cap because
-- (a) per-instance O(2) reads; (b) no per-sidecar queries; (c) atomic
-- CREATE-DEPLOYMENT write; (d) shape validation happens at the API
-- layer in pkg/api/dto.go::Sidecars.Validate.
--
-- Why NOT NULL DEFAULT '[]'::jsonb: legacy rows (pre-#463) backfill
-- to a valid empty array on the ALTER TABLE without a full-table
-- sweep in the hot path; the SELECT side coalesces NULL to '[]' too
-- (defence in depth). The pkg/state/pgstore.go SELECT projection
-- `coalesce(sidecars, '[]'::jsonb)` handles hand-INSERTed NULLs.
--
-- Why a CHECK constraint on the cap: the API gate may be bypassed
-- (manual SQL, future grpc handler, debug shell). The schema is the
-- authoritative second-line defence. The cap is intentionally 2, not
-- configurable per plan — see ADR-066 §Decision 1 (the hard cap is
-- a GLOBAL const, not a per-plan matrix).
--
-- Why no GIN index: per-sidecar queries are not a documented access
-- pattern. PR-B reads the array once at wake; the SELECT side
-- always returns the full array. The 2-row scan cost is negligible.
-- If a future PR needs `WHERE sidecars @> '[{"type":"init"}]'`, add
-- a `CREATE INDEX deployments_sidecars_init ON deployments USING GIN
-- (sidecars jsonb_path_ops)` at that time.
--
-- Replay-safety: `IF NOT EXISTS` on the ADD COLUMN makes a second
-- MigrateUp a no-op (PR #377 / ADR-041 contract). A second ADD
-- CONSTRAINT IF NOT EXISTS is NOT supported by Postgres (constraint
-- names share a namespace; use the pg_constraint existence check
-- below).
--
-- Slot: 095 (HEAD on origin/main is 094 = app_registry_credentials,
-- issue #461 / ADR-064). If a sibling PR claims 095 first, renumber
-- per migrations/README.md (ADR-041 fence) and update the test
-- filename + test function name + cmd/e2e/harness.go::e2eMigrationTarget
-- together.

ALTER TABLE deployments
    ADD COLUMN IF NOT EXISTS sidecars jsonb NOT NULL DEFAULT '[]'::jsonb;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1
        FROM pg_constraint
        WHERE conname = 'deployments_sidecars_cap_chk'
          AND conrelid = 'deployments'::regclass
    ) THEN
        ALTER TABLE deployments
            ADD CONSTRAINT deployments_sidecars_cap_chk
                CHECK (jsonb_array_length(sidecars) <= 2);
    END IF;
END $$;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

ALTER TABLE deployments DROP CONSTRAINT IF EXISTS deployments_sidecars_cap_chk;
ALTER TABLE deployments DROP COLUMN IF EXISTS sidecars;

-- +goose StatementEnd
