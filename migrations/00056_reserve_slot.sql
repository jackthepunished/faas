-- +goose Up
-- +goose StatementBegin

-- Slot 00056 reservation.
--
-- Three PRs opened against origin/main @ e935b93 in parallel all landed
-- on migration slot 00056 in the same week (2026-07-28):
--
--   PR #335 (this branch)  -- IAM-3 server-side session revocation
--                            (issue #187 + #244 merged, ADR-039)
--   PR #369                -- billing: credit consumption reducer
--                            (issue #279 PR-C)
--   PR #352                -- feat(githubd): real user-to-server OAuth
--                            exchange + durable install state (PR-C)
--
-- The cross-PR slot gate (scripts/ci/check_migration_slots.sh, PR #377)
-- refuses to let any two of them share a slot: whichever merged second
-- would panic goose with "duplicate version 56 detected" and break main
-- (the failure mode PR #377 was written to prevent -- see issue #366).
-- PR #377 itself shipped on main after 00055, so the highest current
-- slot on main is 55 and 00056 is the next free one -- but three
-- concurrent PRs cannot share it.
--
-- This migration claims slot 00056 with a no-op Up block so:
--
--   1. The embedded migration set stays 1..N contiguous per
--      embed_test.go::TestMigrationsContiguous.
--   2. The cross-PR slot gate fails PRs #369 + #352 with a clear
--      "this PR must renumber" message pointing at the slot
--      reservation that now lives on main.
--   3. The actual IAM-3 schema (the sessions table + partial index)
--      ships in 00057_sessions.sql next to this file; the +1
--      displacement is intentional and required by the slot dance.
--
-- Why a SELECT 1 no-op rather than an empty Up block: goose requires
-- at least one statement between StatementBegin and StatementEnd (its
-- parser rejects a comment-only body at apply time via the
-- missingSemicolonError check in sqlparser/parser.go). A read-only
-- statement is the canonical goose no-op -- it is visible to the
-- transaction, requires no schema change, and is the smallest possible
-- reservation footprint. postgres's MVCC means the statement commits
-- on version 56 of goose_db_version and is immediately superseded by
-- the 00057_sessions.sql row in the same apply pass.
--
-- Why PR #335 takes the slot: this branch originally wrote
-- migrations/00050_sessions.sql at commit 12a5672 (the IAM-3 feature
-- commit) and has been renumbered four times since (00050 -> 00054 ->
-- 00055 -> 00056 -> 00057) to dodge other merges that landed slots out
-- from under it. Owning 00056 here keeps the migration set contiguous
-- on main and lets the other two PRs renumber to the next free slots
-- (00058 and 00059 respectively) without anyone else having to move.
-- PRs #369 and #352 are expected to renumber their files in their own
-- follow-up commits; the cross-PR gate will surface that promptly.

select 1;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

-- Slot 00056 reservation is forward-only by design; rolling it back
-- would re-expose the slot to the cross-PR gate and is not part of
-- any supported rollback path. See the file header for context.

select 1;

-- +goose StatementEnd
