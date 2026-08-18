-- filename: 00244_reserve_slot.sql
-- +goose Up
-- +goose StatementBegin
-- Co-fence for PR #887 (kind=throttle, ADR-091 amendment D20.5,
-- issue #881). PR #887 owns 00244_*.sql on its open branch.
-- PR #864 (reqbudget) re-renumbered its slot from 00244 to
-- 00245 once PR #887's reservation surfaced. Body is a no-op
-- SELECT 1; will be shadowed by PR #887's real DDL when it
-- merges (per ADR-041, PR #887 drops this fence in a
-- follow-up commit).
SELECT 1;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
SELECT 1;
-- +goose StatementEnd
