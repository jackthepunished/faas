-- +goose Up
-- +goose StatementBegin

-- Issue #463 / ADR-069 / PR-B — Per-workload filesystem handle for sidecars.
--
-- Adds `deployment_sidecar_layers(deployment_id, sidecar_name,
-- storage_key, bytes, content_digest)`. Each row is one sidecar's
-- per-app ext4 storage key. The `deployments.sidecars` jsonb column
-- (migration 00118) remains the API contract surface; this table is
-- the per-workload filesystem handle that imaged writes and vmmd reads
-- at wake time.
--
-- Why a normalized table (not nested jsonb): vmmd reads exactly one
-- storage_key per sidecar at wake, and imaged needs to UPSERT the
-- row during rebuild. SQL side: a single-row SELECT by
-- (deployment_id, sidecar_name) is a direct index lookup; the
-- jsonb-alternative is `... -> 'name' = $1` against the array which
-- is exactly the pattern ADR-069 §Decision 2 explicitly rejected.
--
-- Why 2-row cap (mirrored from migration 00118): if someone bypasses
-- the API gate and INSERTs three rows here, the FK + the
-- `sidecars jsonb` CHECK constraint on `deployments` will be in
-- disagree (jsonb says <= 2, this table allows N). We mirror the
-- constraint as a deferred CHECK so a direct INSERT into this table
-- cannot exceed SidecarCapMax without violating the constraint when
-- the transaction commits. The unique index alone doesn't cap row
-- count; the CHECK does.
--
-- Why unique (deployment_id, sidecar_name) — not storage_key:
-- sidecar names are customer-chosen; storage_keys are derived and
-- must be unique per sidecar to avoid a name collision silently
-- shadowing a rebuild's output. A unique on storage_key would let
-- two sidecars share a key if a customer named them the same — the
-- name path catches it earlier and with a clearer error.
--
-- Why ON DELETE CASCADE: matches the `deployments` ownership
-- discipline (apid is the only writer; a deployment delete should
-- remove all per-sidecar artifacts so imaged's GC walk doesn't see
-- orphans). Per the project's "writer of last resort" pattern in
-- cleanupAppFiles (pkg/imaged/handler.go), imaged also removes the
-- storage keys explicitly — the FK cascade is the safety net.
--
-- Replay-safety: `IF NOT EXISTS` on CREATE TABLE and CREATE INDEX
-- (PR #377 / ADR-041 contract). Existing rows (if any from a partial
-- PR-B rebase) backfill nothing — the table is new.
--
-- Slot: 00127 (post-rebase renumber 119→123→124→127). See migrations/README.md §"Slot fence discipline"
-- for the renumber procedure. The original PR-B plan claimed 00119;
-- main's recent merges added real migrations + fences at 00119/00120/00121/00122
-- (00122_instances_framework_ready_at.sql) and 00123_compute_nodes_vcpu_budget.sql,
-- PR #547 (open) added 00124_reserve_slot + 00125_warm_hint + 00126_pg_ratelimit,
-- so the sidecar layers migration renumbered to 00127 — past PR #547's 00126
-- ceiling, before the index migration at 00128.

CREATE TABLE IF NOT EXISTS deployment_sidecar_layers (
    deployment_id  uuid NOT NULL REFERENCES deployments(id) ON DELETE CASCADE,
    sidecar_name   text NOT NULL,
    storage_key    text NOT NULL,
    bytes          bigint NOT NULL CHECK (bytes >= 0),
    content_digest text NOT NULL,
    created_at     timestamptz NOT NULL DEFAULT now(),
    updated_at     timestamptz NOT NULL DEFAULT now(),

    PRIMARY KEY (deployment_id, sidecar_name)
);

-- Per-deployment 2-row cap (issue #463 / ADR-069 / PR-B review
-- finding #1). The PR-B original used `CHECK (true) DEFERRABLE
-- INITIALLY DEFERRED` as a placeholder; that constraint is a no-op.
-- The real enforcement is a BEFORE INSERT OR UPDATE trigger that
-- counts existing rows for the same deployment_id and RAISE
-- EXCEPTIONs when the count would exceed 2. Why a trigger and not a
-- function-based CHECK: Postgres rejects function-based CHECKs that
-- subquery the same table (circular), so a per-row-count constraint
-- can only live in a trigger or a denormalised counter table. The
-- trigger is the standard Postgres pattern, well-understood by the
-- operator, and runs only on PK-projected rows (the
-- (deployment_id, sidecar_name) PK gives uniqueness so the trigger
-- can't double-fire on a re-imaged rebuild).
--
-- Why not rely solely on the `deployments.sidecars` jsonb CHECK:
-- the jsonb CHECK enforces <= 2 entries in the jsonb array; a hand-
-- INSERT into `deployment_sidecar_layers` could bypass the apid
-- API gate and store a third row that no jsonb entry references.
-- The trigger closes that escape hatch.
--
-- Why the count threshold is 2: matches SidecarCapMax in
-- pkg/api/limits.go (issue #463 / ADR-069 §Decision 1). If a
-- future PR lifts the cap, raise both constants together — the
-- trigger MUST be re-applied via a fresh migration so the operator
-- sees the limit change in git history.
CREATE OR REPLACE FUNCTION deployment_sidecar_layers_cap_check()
RETURNS trigger
LANGUAGE plpgsql
AS $$
DECLARE
    current_count integer;
BEGIN
    -- NEW-row predicate on UPDATE; existing-row count on INSERT.
    -- Same query works for both because NEW carries the row's
    -- deployment_id whether we're inserting a fresh row or
    -- rewriting an existing one.
    -- `current_count` is the number of rows for this deployment_id
    -- ALREADY in the table at the time of this trigger call. We
    -- reject the operation when the row count would exceed
    -- SidecarCapMax (=2). Because the trigger fires BEFORE the
    -- row is written, the post-insert count is current_count + 1
    -- (INSERT) or unchanged (UPDATE that doesn't move the row to
    -- a different deployment). We compare against the post-write
    -- ceiling: if the existing count is already at or above
    -- SidecarCapMax, refuse — that is, current_count >= 2 is a
    -- hard reject, since adding another row would push us to 3.
    SELECT count(*) INTO current_count
        FROM deployment_sidecar_layers
        WHERE deployment_id = NEW.deployment_id;

    IF current_count >= 2 THEN
        RAISE EXCEPTION 'deployment_sidecar_layers: deployment % exceeds the 2-row cap (existing=%, new would make 3)',
            NEW.deployment_id, current_count
            USING ERRCODE = 'check_violation';
    END IF;

    RETURN NEW;
END;
$$;

-- Replay-safety: the trigger has no `IF NOT EXISTS` clause (Postgres
-- 14+'s `CREATE OR REPLACE TRIGGER` was added in PG 14 but the project's
-- minimum supported version is the same and a few pg_dump tools still
-- emit the old form). Wrap in a DO block that checks pg_trigger
-- first so a re-run on a DB that already has the trigger is a no-op.
-- Matches the `00053_deployments_source_url.sql` DO-block-guarded
-- constraint pattern (replay-safety_test.go). PR #377 / ADR-041
-- contract.
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_trigger
        WHERE tgname = 'deployment_sidecar_layers_cap_trg'
    ) THEN
        CREATE TRIGGER deployment_sidecar_layers_cap_trg
            BEFORE INSERT OR UPDATE ON deployment_sidecar_layers
            FOR EACH ROW
            EXECUTE FUNCTION deployment_sidecar_layers_cap_check();
    END IF;
END $$;

CREATE INDEX IF NOT EXISTS deployment_sidecar_layers_storage_key_idx
    ON deployment_sidecar_layers (storage_key);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP INDEX IF EXISTS deployment_sidecar_layers_storage_key_idx;
DROP TRIGGER IF EXISTS deployment_sidecar_layers_cap_trg ON deployment_sidecar_layers;
DROP FUNCTION IF EXISTS deployment_sidecar_layers_cap_check();
DROP TABLE IF EXISTS deployment_sidecar_layers;

-- +goose StatementEnd
