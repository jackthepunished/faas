-- +goose Up
-- +goose StatementBegin
--
-- 00190_reserve_slot.sql — slot reservation placeholder
-- (ADR-041 / PR #391 migration gate carve-out).
--
-- Bridging fence between PR #805's real migration (00190_admin_obs_index.sql)
-- and PR #800's (this PR, ADR-089) real migration 00191_app_secrets_kid.sql.
-- One-slot fence so the embedded FS stays contiguous while the two-way slot
-- race resolves.
--
-- Body: `select 1;` — deliberate no-op. The replay-safety gate in ci.yml
-- drops files matching the reservation regex from its "added migration
-- versions" computation.
--
select 1;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
-- No-op: nothing to reverse (the Up body is a deliberate select 1;).
-- +goose StatementEnd
