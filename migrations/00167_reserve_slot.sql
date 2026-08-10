-- +goose Up
-- +goose StatementBegin
--
-- 00167_reserve_slot.sql — slot reservation placeholder
-- (ADR-041 / PR #391 migration gate carve-out).
--
-- Sibling of migrations/00166_reserve_slot.sql from PR #795 (issue
-- #791 PR A). Created on the same renumber: the original migration
-- was 00166 but PRs #797 (compute_nodes_public_ip), #799 (edge_rules),
-- and #800 (app_secrets_kid) all claimed 00166 in parallel, so this
-- PR renumbered to 00169. Slots 167 and 168 are intentionally left
-- empty for whichever sibling PR wants them — fences 166/167/168
-- hold the embedded FS contiguous while the four-way slot race
-- resolves on its own.
--
-- Body: `select 1;` — deliberate no-op for the apply path. The
-- replay-safety gate in ci.yml drops files matching the reservation
-- regex from its "added migration versions" computation, so this
-- file is invisible to replay-safety but still satisfies
-- TestMigrationsContiguous. See migrations/00056_reserve_slot.sql
-- for the canonical comment template (PR #369 / PR #391).
--
select 1;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
-- No-op: nothing to reverse (the Up body is a deliberate select 1;).
-- +goose StatementEnd
