-- filename: 00219_reserve_slot.sql
-- +goose Up
-- +goose StatementBegin
-- Reserve slot 219 for the ADR-095 (connection-aware execution)
-- PR-cluster. The real migration lands as 00219_data_upstreams.sql
-- at PR-A merge base; this fence exists solely to claim the slot
-- ahead of sibling PRs per the cross-pr-slot-fence pagination
-- pattern. Body is a no-op SELECT 1.
SELECT 1;
-- +goose StatementEnd
