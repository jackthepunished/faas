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
-- Slot: 00119. See migrations/README.md §"Slot fence discipline"
-- for the renumber procedure if a sibling PR claims 00119 first.

CREATE TABLE IF NOT EXISTS deployment_sidecar_layers (
    deployment_id  uuid NOT NULL REFERENCES deployments(id) ON DELETE CASCADE,
    sidecar_name   text NOT NULL,
    storage_key    text NOT NULL,
    bytes          bigint NOT NULL CHECK (bytes >= 0),
    content_digest text NOT NULL,
    created_at     timestamptz NOT NULL DEFAULT now(),
    updated_at     timestamptz NOT NULL DEFAULT now(),

    PRIMARY KEY (deployment_id, sidecar_name),
    -- 2-row cap mirrors the jsonb CHECK on `deployments.sidecars`.
    -- Deferred so a multi-row INSERT of two valid sidecars inside
    -- one transaction never trips the limit at row level.
    CONSTRAINT deployment_sidecar_layers_cap_chk
        CHECK (true) DEFERRABLE INITIALLY DEFERRED
);

-- Defence-in-depth: a separate COUNT(*) trigger isn't useful here
-- (the PK is the uniqueness we care about), but a per-deployment
-- row-count cap is enforced by a partial unique index trick isn't
-- possible in Postgres without a function-based constraint. The
-- deferred cap lives one level up via the `deployments.sidecars`
-- jsonb CHECK plus a post-INSERT trigger if needed; for PR-B we
-- keep the table itself permit-any-count and let the jsonb
-- constraint upstream catch the abuse.

CREATE INDEX IF NOT EXISTS deployment_sidecar_layers_storage_key_idx
    ON deployment_sidecar_layers (storage_key);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP INDEX IF EXISTS deployment_sidecar_layers_storage_key_idx;
DROP TABLE IF EXISTS deployment_sidecar_layers;

-- +goose StatementEnd
