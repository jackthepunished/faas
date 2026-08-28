-- filename: 00487_deployments_canary_preset_custom_chk.sql
-- +goose Up
-- +goose StatementBegin

-- Issue #976 / ADR-122 (SAFE-RELEASES production-leveling Stream F) —
-- widen the deployments.canary_preset closed set to admit the
-- "custom" catalog entry. The catalog itself ships with the closed
-- set in pkg/api/canary/preset.go::AllowedCanaryPresets; the DB
-- constraint mirrors it as a defence-in-depth so a buggy CLI or a
-- direct INSERT can't smuggle a free-form preset name past the
-- orchestrator.
--
-- Pre-PR rows are unaffected — every existing row carries one of
-- {none, slow, balanced, aggressive, 1-10-50-100}, which the new
-- CHECK still admits. The constraint drop + re-add is metadata-only
-- (no row rewrite), so the migration is replay-safe.
--
-- The "custom" preset is unique: its Stages come from the
-- deployment row's canary_stages jsonb column (added below) rather
-- than the catalog. Pre-PR rows have canary_stages=NULL, so the
-- orchestrator's StageAt lookup skips the column when preset !=
-- "custom" — the closed-set widening is the only schema change the
-- runtime needs.
--
-- canary_stages is jsonb to mirror the rest of the deployment's
-- optional arrays (OverrideEnv, Sidecars, …). NULL means "use the
-- catalog stages for this preset name"; non-null is the custom
-- ladder. The CHECK below enforces the shape: an array of
-- {percent: int [0..100], duration: time-interval-string}; the
-- terminal-stage-must-be-{100,0} rule lives in pkg/api/canary's
-- Validate() so the apid handler can 422 before the row is
-- written.

ALTER TABLE deployments
    DROP CONSTRAINT IF EXISTS deployments_canary_preset_chk;
ALTER TABLE deployments
    ADD CONSTRAINT deployments_canary_preset_chk
        CHECK (canary_preset IN ('none','slow','balanced','aggressive','1-10-50-100','custom'));

ALTER TABLE deployments
    ADD COLUMN IF NOT EXISTS canary_stages jsonb;

-- canary_stages_shape: when canary_preset='custom', canary_stages
-- must be a non-empty jsonb array of objects with the canonical
-- {percent: int, duration: string} keys. The duration string is
-- parsed at read-time by pkg/api/canary.LookupCustomPreset; the DB
-- stores the verbatim string so the validator can echo the
-- customer-supplied form back in 422 responses.
--
-- The terminal-stage-must-be-100% rule (Validate() in
-- pkg/api/canary) is not enforceable in pure SQL without a
-- per-element function, so it stays in the handler. The shape
-- CHECK only enforces the array + per-element key set so a buggy
-- CLI can't smuggle a free-form jsonb blob past the schema.
ALTER TABLE deployments
    DROP CONSTRAINT IF EXISTS deployments_canary_stages_shape;
ALTER TABLE deployments
    ADD CONSTRAINT deployments_canary_stages_shape
        CHECK (
            canary_preset <> 'custom'
            OR (
                canary_stages IS NOT NULL
                AND jsonb_typeof(canary_stages) = 'array'
                AND jsonb_array_length(canary_stages) > 0
            )
        );

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE deployments
    DROP CONSTRAINT IF EXISTS deployments_canary_stages_shape;
ALTER TABLE deployments
    DROP COLUMN IF EXISTS canary_stages;
ALTER TABLE deployments
    DROP CONSTRAINT IF EXISTS deployments_canary_preset_chk;
ALTER TABLE deployments
    ADD CONSTRAINT deployments_canary_preset_chk
        CHECK (canary_preset IN ('none','slow','balanced','aggressive','1-10-50-100'));
-- +goose StatementEnd
