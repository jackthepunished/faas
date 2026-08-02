-- +goose Up
-- +goose StatementBegin

-- filename: 00107_events_wake_id_idx.sql
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
-- Migration slot: 00107. The cross-PR gate reserves 00108 for
-- PR-D's jailer / Firecracker stderr capture table (issue #517
-- final PR, ADR-045 follow-up).
--
-- Renumber history (post-#533-merge reset): this PR's real migration
-- was at 92 → 97 → 99 → 101 → 103 → 105 → 107 across seven rebase
-- cycles before the post-#533 renumber reset strategy dropped the
-- intermediate renumber commits. After dropping the chain on this
-- rebase, the final slot landed at 107 (next free after main's
-- real migrations at 103, 104, 105). Companion reservation
-- `00108_reserve_slot.sql` (this branch) holds slot 108 for
-- PR-D's stderr capture table per ADR-045.

create index if not exists events_wake_id_idx
  on events ((data->>'wake_id'))
  where data->>'wake_id' is not null;

-- +goose StatementEnd
-- +goose Down
-- +goose StatementBegin

drop index if exists events_wake_id_idx;

-- +goose StatementEnd
