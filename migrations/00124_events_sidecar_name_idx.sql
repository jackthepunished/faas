-- +goose Up
-- +goose StatementBegin

-- Issue #463 / ADR-069 / PR-B review finding #5 — index
-- ListEventsBySidecar's jsonb filter so the audit timeline
-- doesn't fall off a sequential scan once events accumulate.
--
-- The reader (pkg/state/pgstore.go::ListEventsBySidecar) runs:
--
--   select ... from events
--   where kind in ('wake.sidecar_init_exit', 'wake.sidecar_restart')
--     and data->>'sidecar_name' = $1
--   order by at asc
--   limit $2
--
-- Without an index, this is a sequential scan that reads every
-- event row in the table just to filter on two rarely-matched
-- kinds. The PR-B events audit surface (PR-C's wider wake-
-- timeline rollout) will issue this query from a hot path
-- inside the per-sidecar observability dashboard; a Seq Scan
-- here is the difference between a sub-millisecond timeline
-- and a multi-second pause once the events table has a few
-- million rows.
--
-- The index is a PARTIAL expression index restricted to the
-- two closed sidecar-event kinds. This keeps it small (the
-- wake.boot_started / wake.cold_boot_finished / ... events
-- that dominate the table are excluded) and lets the planner
-- use it directly for ListEventsBySidecar's predicate. The
-- planner's btree-on-expression index match is exact — the
-- index key is `(data->>'sidecar_name')::text`, the predicate
-- is `data->>'sidecar_name' = $1`, so an index-only scan
-- suffices. A future PR that adds another closed sidecar-kind
-- must add the kind to the WHERE list (the planner can still
-- use the index for the jsonb predicate but the kind filter
-- falls back to a heap filter — fine, the index size stays
-- the same).
--
-- Storage cost is bounded: at the closed-kinds rate (a few
-- events per sidecar restart cycle), the index never grows
-- past O(sidecars × events-per-sidecar) rows. With the 2-row
-- cap (ADR-069 §Decision 1), the realistic ceiling is ~6
-- rows per deployment per wake cycle.
--
-- Replay-safety: `CREATE INDEX IF NOT EXISTS` (PR #377 /
-- ADR-041 contract).

CREATE INDEX IF NOT EXISTS events_sidecar_name_idx
    ON public.events ((data->>'sidecar_name')::text)
    WHERE kind IN ('wake.sidecar_init_exit', 'wake.sidecar_restart');

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP INDEX IF EXISTS public.events_sidecar_name_idx;

-- +goose StatementEnd