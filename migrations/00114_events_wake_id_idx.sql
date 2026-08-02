-- +goose Up
-- +goose StatementBegin

-- filename: 00114_events_wake_id_idx.sql
--
-- issue #517 / PR-C / ADR-064 — wake-timeline expression index.
--
-- PR-A and PR-B write wake_id through state.Store.AppendEvent as
-- a jsonb data key. The customer-facing timeline endpoint
-- (GET /v1/apps/{slug}/wakes/{wake_id}/timeline) filters on
-- data->>'wake_id', but a plain index on (data) would index the
-- whole jsonb and explode storage. The expression index
-- `events((data->>'wake_id'))` is a partial index that only
-- materialises rows whose data carries a wake_id, and the WHERE
-- clause `data->>'wake_id' IS NOT NULL` keeps the index size
-- proportional to the wake-envelope rows (1 per wake phase, ~13
-- per cold wake) rather than the full audit-log volume.
--
-- A naive filter against unindexed jsonb would miss frames on
-- high-RPS apps (audit-events table can carry 10⁶+ rows / day /
-- account). The endpoint must read in at most one or two
-- disk-resident index pages per wake_id lookup, so the
-- expression + partial + WHERE-NOT-NULL combination is the
-- shape.
--
-- Backwards-compat: existing rows that pre-date the wake.* kind
-- vocabulary do not have data.wake_id set. They naturally fall
-- outside the partial WHERE clause and are not indexed — that
-- is the intended outcome (legacy rows are out of scope of
-- PR-C, see ADR-064 §"Compatibility").
--
-- Migration slot: 00114. PR-D's jailer / Firecracker stderr capture
-- table (issue #517 final PR, ADR-045 follow-up) is held by the
-- 00113 reservation on this branch (the cross-PR gate hides
-- simultaneous reservations via the slots_from_paths regex carve-
-- out per ADR-041).
--
-- Renumber history (post-#533-merge reset + post-#525-merge bump +
-- post-#540-collision bump): this PR's real migration has been at
-- 92 → 97 → 99 → 101 → 103 → 105 → 107 → 111 → 113 → 114 across ten
-- rebase cycles. PR #540 (state coverage 86%) opened with a real
-- migration at 00111 plus a partner reservation at 00112, so PR-C
-- bumped from 111 to 113 then 114 (next free slot after main's 110
-- + the 111/112 pair held by PR #540).

create index if not exists events_wake_id_idx
  on events ((data->>'wake_id'))
  where data->>'wake_id' is not null;

-- +goose StatementEnd
-- +goose Down
-- +goose StatementBegin

drop index if exists events_wake_id_idx;

-- +goose StatementEnd
