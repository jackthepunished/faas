-- filename: 00253_reserve_slot.sql
-- +goose Up
-- +goose StatementBegin
-- Co-fence for PR #895 (ADR-099 jobs cluster). PR #895 owns
-- 00253_reserve_slot.sql on its open branch; this is a paired
-- fence at the same slot (carve-out excludes *_reserve_slot.sql
-- from collision checks per ADR-041). Dropped in a follow-up
-- commit when PR #895 merges.
SELECT 1;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
SELECT 1;
-- +goose StatementEnd