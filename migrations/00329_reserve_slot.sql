-- filename: 00329_reserve_slot.sql
-- Reserved slot to keep the migrations directory contiguous for
-- the synthetic-merge embed (CI's TestMigrationsContiguous
-- pins slot N to position N). The api-contract-diff cluster
-- (ADR-121) leapfrogged slot 00330 to clear PR #997 / PR
-- #1000 / PR #1004; the slots between 00325 (PR #1000 / PR
-- #997 fence) and 00330 must be filled so the gate doesn't
-- trip when CI synthesizes a merge against the latest
-- branches. Drop these fences in a follow-up commit if the
-- slot is not consumed by PR-B / PR-C.
-- +goose Up
-- +goose StatementBegin
SELECT 1;
-- +goose StatementEnd
-- +goose Down
-- +goose StatementBegin
SELECT 1;
-- +goose StatementEnd
