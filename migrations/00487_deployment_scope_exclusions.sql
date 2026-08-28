-- filename: 00487_deployment_scope_exclusions.sql
-- +goose Up
-- +goose StatementBegin
--
-- ADR-124 follow-up #3 — persistent --exclude history. The
-- per-deploy --exclude flag (PR #1065, head 0b4cf07f4) is
-- ephemeral: it survives only for the one scan/apply cycle that
-- invoked it. Operators who want to "exclude this workload for
-- the long haul" need a persistent record so a subsequent
-- `gregale deploy --tarball=...` without --exclude still honors
-- the previous intent. This table is that record.
--
-- Schema posture (modeled on 00384_mirror_rules.sql):
--   * account_id + project_id FKs both ON DELETE CASCADE —
--     accounts/projects use real DELETE on user-pays-GDPR /
--     full-reset paths (unlike apps which are soft-deleted; see
--     the soft-delete CASCADE blind spot section below).
--   * app_id is a snapshot reference to apps(id) with NO FK —
--     see CRITICAL pitfall in the header below.
--   * slug is closed-set invariant (CHECK slug = lower(slug))
--     so consumers can lowercase-key without ambiguity.
--   * UNIQUE (account_id, project_id, slug) prevents the same
--     exclusion being recorded twice — the active state of an
--     exclusion is "the row exists".
--   * reason defaults '' and is operator-supplied free text —
--     surfaced in the audit log + the --show-affected table so
--     the customer remembers WHY they excluded the workload.
--   * created_by is the actor identity (cookie subject, API key
--     id, or 'system' for janitor-reaped rows) and the audit
--     log stitches it back to the kind=project.scope.excluded
--     emission.
--   * created_at + updated_at + set_updated_at trigger mirror the
--     mirror_rules pattern.
--
-- Indexing posture:
--   * deployment_scope_exclusions_project_idx on (project_id) —
--     the apply path's hot lookup (does this project have
--     persisted exclusions to fold into the next apply?).
--     Partial WHERE created_at > now() - interval '90 days'
--     caps the index to the active window; the janitor purges
--     older rows after they hit the retention boundary
--     (PurgeOrphanedScopeExclusions in pkg/state/pgstore.go).
--   * deployment_scope_exclusions_account_idx on (account_id) —
--     secondary; admin tooling queries "every persisted
--     exclusion in this account" and the per-project lookup
--     alone would scan N rows per project.
--
-- ============================================================
-- CRITICAL: SOFT-DELETE CASCADE BLIND SPOT (do not regress)
-- ============================================================
-- pkg/state/pgstore.go:3426 documents: "Per Phase 5 user
-- decision the cascade is status-only — child rows survive
-- for slug-reuse". The platform uses `UPDATE apps SET
-- status='deleted'` (pgstore.go:3442-3444), NOT a row DELETE.
-- An FK with ON DELETE CASCADE to apps(id) would NOT fire when
-- an app is soft-deleted — orphans would accumulate forever.
-- The audit trail here is keyed by (account_id, project_id,
-- slug) for active exclusions; app_id is a snapshot reference
-- that may go stale (acceptable — exclusions lifecycle is
-- project-scoped, not app-scoped). The janitor
-- PurgeOrphanedScopeExclusions(ctx) (lands in PR-B commit 3
-- alongside the PgStore CRUD) reaps rows whose app_id points
-- at a soft-deleted app after the 90-day retention window.
--
-- ============================================================
-- Slot choice
-- ============================================================
-- Branch note: this branch (worktree-feat-affected-workload-
-- preview) was cut from origin/main at commit `0b4cf07f4`
-- where the migration tail was 00386. origin/main has since
-- advanced 140 commits to 00487_edge_rules_cors_preset_fk.sql
-- (PR #1090). Picking 00487 — directly claiming the tail slot
-- is correct against the current origin/main head. The
-- original branch picked 00417 + 00418, then renumbered to
-- 00487 + 00488 (with a 00487 reserve_slot fence) after the
-- first rebase; the second rebase onto origin/main (which now
-- has a real 00487 edge_rules_cors_preset_fk) required the
-- fence to be deleted and the exclusions migration to be
-- renumbered to 00487.

-- Replay-safe posture: every CREATE in this Up block uses
-- IF NOT EXISTS (or DROP TRIGGER IF EXISTS before CREATE
-- TRIGGER) so the migration is idempotent on re-apply.
-- TestNewMigrationsAreReplaySafe walks each new migration in
-- isolation on a fresh DB, replays it, and asserts no 42P07 /
-- 42710 (relation-already-exists) errors — the same pattern
-- 00304_cors_presets + 00329_consumer_keys + 00384_mirror_rules
-- use.

CREATE TABLE IF NOT EXISTS deployment_scope_exclusions (
    id         uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    account_id uuid        NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    project_id uuid        NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    -- app_id is a snapshot reference; NO FK to apps(id) — see
    -- the soft-delete CASCADE blind spot section above.
    app_id     uuid        NOT NULL,
    slug       text        NOT NULL,
    reason     text        NOT NULL DEFAULT '',
    created_by text        NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    CHECK (slug = lower(slug)),
    CHECK (length(slug) > 0),
    UNIQUE (account_id, project_id, slug)
);

-- Indexes on (project_id) + (account_id) — full coverage; the
-- per-row volume per scope is bounded by the janitor
-- (PurgeOrphanedScopeExclusions reaps rows > 90 days). We do NOT
-- use a partial-index `WHERE created_at > now() - interval '90 days'`
-- because `now()` is STABLE not IMMUTABLE, so PostgreSQL rejects
-- the partial-index predicate with SQLSTATE 42P17 (functions in
-- index predicate must be marked IMMUTABLE). The janitor keeps the
-- table small enough that a full index is fine.
CREATE INDEX IF NOT EXISTS deployment_scope_exclusions_project_idx
    ON deployment_scope_exclusions (project_id);

CREATE INDEX IF NOT EXISTS deployment_scope_exclusions_account_idx
    ON deployment_scope_exclusions (account_id);

CREATE OR REPLACE FUNCTION deployment_scope_exclusions_set_updated_at()
RETURNS TRIGGER AS $$
BEGIN
    NEW.updated_at = now();
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS deployment_scope_exclusions_set_updated_at_trg
    ON deployment_scope_exclusions;
CREATE TRIGGER deployment_scope_exclusions_set_updated_at_trg
    BEFORE UPDATE ON deployment_scope_exclusions
    FOR EACH ROW EXECUTE FUNCTION deployment_scope_exclusions_set_updated_at();

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

-- Forward-only widening: dropping this migration's objects is
-- safe (no other table references it by FK — see the soft-
-- delete CASCADE blind spot section above; the absence of an
-- apps FK is the load-bearing design choice here).
DROP TRIGGER IF EXISTS deployment_scope_exclusions_set_updated_at_trg
    ON deployment_scope_exclusions;
DROP FUNCTION IF EXISTS deployment_scope_exclusions_set_updated_at();
DROP INDEX  IF EXISTS deployment_scope_exclusions_account_idx;
DROP INDEX  IF EXISTS deployment_scope_exclusions_project_idx;
DROP TABLE IF EXISTS deployment_scope_exclusions;

-- +goose StatementEnd