-- filename: 00227_reserve_slot.sql
-- +goose Up
-- +goose StatementBegin
-- Reserve slot 227 for the next PR-cluster PR after PR #845
-- (ADR-091 D21-D23 kind=geo at 00226). Same fence rationale as
-- 00211; see cross-pr-slot-fence-reservation-fence-pattern for
-- the pattern.
--
-- DO NOT bump the slot. If you are claiming this slot for a
-- different feature, fork off the latest main and write your own
-- reserve_slot at the next free number. The slot is currently
-- fenced because main has 00225_reserve_slot.sql (PR #866) and
-- PR #845 has 00226_edge_rules_kind_geo.sql + 00227 (this fence)
-- — three contiguous reservation markers from sibling PRs.
SELECT 1;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
SELECT 1;
-- +goose StatementEnd
