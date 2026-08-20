-- filename: 00300_triggers_filter_criteria.sql
-- +goose Up
-- +goose StatementBegin

-- ADR-118 / issue #757 closure — add `filter_criteria` to the
-- unified Trigger primitive (issue #757 §criterion 4: "supports
-- FilterCriteria JSON with $or / $and / payload operators — direct
-- parity with Lambda's filter criteria").
--
-- Scope: nullable JSONB column on `triggers` storing a closed-vocab
-- filter tree evaluated per polled record in pkg/sched/dispatch_triggers.go.
-- Default NULL = "no filter, every record passes through" (preserves
-- the byte-for-byte behaviour of every existing trigger row from
-- PR #910 / migration 00297 onwards).
--
-- Schema shape (mirrors the FilterCriteria type in pkg/sched/filter.go,
-- see commit 5 of the ADR-118 mega-PR):
--
--  {
--    "$or":      [ FilterClause, ... ],
--    "$and":     [ FilterClause, ... ],
--    "payload":  [ FilterClause, ... ]   // jsonpath predicates over rec.Payload
--  }
--  FilterClause := {
--    "op":    "eq" | "neq" | "exists" | "jsonpath",
--    "field": "<header key>"             // eq/neq/exists against rec.Headers
--    "path":  "$.foo.bar"                // jsonpath predicate
--    "value": <json.RawMessage>
--    "clauses": [ FilterClause, ... ]    // nested $or / $and
--  }
--
-- Why nullable JSONB rather than typed columns:
--   - The closed vocab lives in the application-layer validator
--     (pkg/gregalemanifest.FilterCriteria). Postgres CHECK on a JSONB
--     shape would be brittle (jsonpath expressions in CHECK are
--     unindexable; a closed JSONB shape needs jsonb_typeof + manual
--     field walk).
--   - The application is the source of truth (the validator rejects
--     malformed trees with a typed error code). Postgres stores
--     opaque.
--   - Same precedent as `triggers.config` (PR #910 / migration 00297
--     line 42: `config JSONB NOT NULL DEFAULT '{}'::jsonb`).
--
-- Why no partial index on (filter_criteria IS NOT NULL):
--   - The per-record evaluation lives in pkg/sched (in-process);
--     Postgres never sees the predicate. A GIN index would pay off
--     only if a future API exposes "list triggers with filter X"
--     server-side — defer until that surface lands.
--
-- Replay-safety (per migrations/replay_safety_test.go contract):
--   - ADD COLUMN IF NOT EXISTS is the modern idempotent form (PG9.6+).
--   - No DOWN mirror: the column is additive; dropping it on downgrade
--     would orphan any `filter_criteria` rows the migration applied to
--     AFTER a default was added. Forward-only by design (matches
--     00298 + 00299 pattern — those widen CHECKs, this adds a column).

ALTER TABLE triggers
    ADD COLUMN IF NOT EXISTS filter_criteria JSONB NULL;

-- +goose StatementEnd

-- +goose Down
-- Forward-only by design — see commit-msg rationale above. Preserve
-- the column unconditionally on downgrade (matches 00298 + 00299).
-- +goose StatementBegin
SELECT 1;
-- +goose StatementEnd
