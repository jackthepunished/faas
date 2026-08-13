-- filename: 00218_reserve_slot.sql
-- +goose Up
-- +goose StatementBegin
-- Reserve slot 218 for the preview-environments migration
-- (issue #272 / ADR-095). Same fence rationale as 00211; see
-- cross-pr-slot-fence-reservation-fence-pattern for the pattern.
--
-- DO NOT bump the slot. If you are claiming this slot for a
-- different feature, fork off the latest main and write your own
-- reserve_slot at the next free number.
SELECT 1;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
SELECT 1;
-- +goose StatementEnd