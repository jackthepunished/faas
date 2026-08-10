-- +goose Up
-- +goose StatementBegin
--
-- 00168_reserve_slot.sql — slot reservation placeholder
-- (ADR-041 / PR #391 migration gate carve-out).
--
-- Sibling of migrations/00166_reserve_slot.sql and
-- migrations/00167_reserve_slot.sql from PR #795 (issue #791 PR A).
-- See 00167 for the full renumber context; 168 is held empty for the
-- same reason (gap between the four-way 00166 collision and the real
-- 00169_invocations_outcome.sql migration).
--
-- Body: `select 1;` — deliberate no-op for the apply path. The
-- replay-safety gate in ci.yml drops files matching the reservation
-- regex from its "added migration versions" computation. See
-- migrations/00056_reserve_slot.sql for the canonical template.
--
select 1;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
-- No-op: nothing to reverse (the Up body is a deliberate select 1;).
-- +goose StatementEnd
