-- filename: 00110_snapshots_tier.sql
-- +goose Up
-- Issue #470 / ADR-055 — snapshots get a tier column. Today every
-- snapshot row is "init" (taken right after guest-init signals :8080
-- bound; restore pays framework warmup). With warm.snap enabled the
-- park path issues a SECOND PauseAndSnapshot once framework_ready is
-- observed; that row is tier='warm'.
--
-- One column + one partial unique index:
--
--   tier   text NOT NULL DEFAULT 'init' CHECK (tier IN ('init','warm'))
--
--   CREATE UNIQUE INDEX snapshots_deployment_tier_key
--     ON snapshots (deployment_id, tier) WHERE stale = false;
--
-- The unique index is on (deployment_id, tier) filtered to non-stale
-- rows so we can re-snapshot a stale-and-recovered tier without
-- colliding on the old row. Stale rows are excluded so imaged's
-- "ignore a duplicate emission" path (pgstore.CreateSnapshot returns
-- ErrConflict on UniqueViolation) still works — a stale duplicate
-- warm-row emission becomes a fresh row, not a collision.
--
-- Schema history note: 00001_init.sql declared snapshots WITHOUT a
-- unique constraint on deployment_id (verified — there is no
-- snapshots_deployment_id_key today). The pgstore comment at
-- pkg/state/pgstore.go:4524 about "Conflicts (same deployment_id)
-- collapse to ErrConflict" is stale; that branch was unreachable.
-- After this migration the new (deployment_id, tier) unique index
-- makes the branch reachable again on duplicate warm-tier emissions
-- from a buggy imaged.
--
-- Replay-safe (ADR-041): ADD COLUMN IF NOT EXISTS + CREATE UNIQUE
-- INDEX IF NOT EXISTS — second MigrateUp is a no-op.
-- +goose StatementBegin
ALTER TABLE snapshots
    ADD COLUMN IF NOT EXISTS tier text NOT NULL DEFAULT 'init';
ALTER TABLE snapshots
    DROP CONSTRAINT IF EXISTS snapshots_tier_check;
ALTER TABLE snapshots
    ADD CONSTRAINT snapshots_tier_check
        CHECK (tier IN ('init', 'warm'));
CREATE UNIQUE INDEX IF NOT EXISTS snapshots_deployment_tier_key
    ON snapshots (deployment_id, tier) WHERE stale = false;
-- +goose StatementEnd

-- +goose Down
-- Reverse: drop the unique index, then drop the tier column. Legacy
-- rows are all tier='init' (the DEFAULT); the column drop is a
-- metadata-only operation.
-- +goose StatementBegin
DROP INDEX IF EXISTS snapshots_deployment_tier_key;
ALTER TABLE snapshots
    DROP CONSTRAINT IF EXISTS snapshots_tier_check;
ALTER TABLE snapshots
    DROP COLUMN IF EXISTS tier;
-- +goose StatementEnd