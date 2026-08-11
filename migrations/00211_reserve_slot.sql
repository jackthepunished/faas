-- filename: 00211_reserve_slot.sql
-- +goose Up
-- +goose StatementBegin
-- Reserve slot 211 for PR #836 (ADR-091 deployments.scope
-- real migration). Same fence rationale as 00210; see
-- cross-pr-slot-fence-pagination-gate for the pattern.
SELECT 1;
-- +goose StatementEnd
