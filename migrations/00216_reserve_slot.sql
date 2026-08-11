-- filename: 00216_reserve_slot.sql
-- +goose Up
-- +goose StatementBegin
-- Reserve slot 216 for PR #844 (ADR-093 per-route app metrics,
-- migrations/00216_apps_route_metrics_enabled.sql). Same fence
-- rationale as 00211; see cross-pr-slot-fence-pagination-gate
-- for the pattern. PR-A renumbered to 00217 because PR-844 also
-- claims 00216, and the slot gate check_migration_slots.sh
-- excludes *_reserve_slot.sql from collision detection.
SELECT 1;
-- +goose StatementEnd

-- +goose StatementBegin
SELECT 1;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
SELECT 1;
-- +goose StatementEnd
