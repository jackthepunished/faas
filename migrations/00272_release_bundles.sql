-- filename: 00272_release_bundles.sql
-- +goose Up
-- +goose StatementBegin
--
-- 00272_release_bundles.sql — issue #911 / ADR-110 release-bundle
-- storage carrier (PR-3a). New table for the content-addressed
-- release tuples that PR-3 (release bundle content + install) writes
-- and PR-4 (gregale doctor) reads. PR-3a is the carrier; PR-3 fills
-- the table, PR-4 reads from it.
--
-- Columns:
--   id              uuid PRIMARY KEY DEFAULT gen_random_uuid() —
--                   surrogate key so PR-3 can FK into this from
--                   the future compute_nodes.release_bundle_id
--                   (deferred to a later cluster PR — release_id
--                   text pointer suffices today).
--   git_sha         text NOT NULL CHECK git_sha ~ '^[a-f0-9]{40}$'
--                   — the immutable 40-hex commit SHA the bundle
--                   was materialised from. Both PR-3's `gregale
--                   release bundle <git-sha>` and the doctor's
--                   "rebuild from same commit produces same hashes"
--                   assertion use this as the join key.
--   manifest_hash   text NOT NULL CHECK manifest_hash ~
--                   '^sha256:[a-f0-9]{64}$' — sha256:<64hex> hash
--                   of the manifest the renderer materialised for
--                   this release. Doctor compares bundle.manifest_hash
--                   against compute_nodes.manifest_hash on every node.
--   daemon_hashes   jsonb NOT NULL DEFAULT '{}'::jsonb —
--                   per-daemon sha256:<64hex> hashes produced by
--                   `make build-sha256` for this release. Empty
--                   default so PR-3 can stamp the JSON incrementally;
--                   PR-4 doctor compares this against the running
--                   binary hashes on each box.
--   created_at      timestamptz NOT NULL DEFAULT now() — bundle
--                   creation time (per release_bundles.created_at
--                   in the PR-3a plan).
--   applied_at      timestamptz NULL — bundle activation time
--                   (set the first time a node installs this
--                   release). Nullable because bundles can exist
--                   before any node adopts them.
--
-- Indexes:
--   release_bundles_git_sha_idx — point-lookup by commit SHA.
--     Mirror the compute_nodes_active_idx WHERE active pattern.
--   release_bundles_applied_at_idx partial WHERE applied_at IS
--     NOT NULL — operator-side "which bundles are live" dashboard
--     query. The partial form keeps the index small when most
--     bundles are unapplied.
--
-- Replay-safety (per the contract migrations/replay_safety_test.go
-- asserts): CREATE TABLE is IF NOT EXISTS, every CREATE INDEX is
-- IF NOT EXISTS. A drifted box (table present, goose row missing)
-- re-applies cleanly without tripping SQLSTATE 42P07 / 42710.
--
-- CHECK constraints are SQLSTATE 23514 violations, not 42710, so
-- they don't conflict on re-apply. Down reverses in tear-down
-- order so the partial index drops first (the partial predicate
-- references the column), then the table.
--
-- Out of scope (consumed by later cluster PRs):
--   - PR-3: writes `daemon_hashes` JSON; sets `applied_at` on first
--     install per node.
--   - PR-4 doctor: read-side check.
--   - Trigger for pg_notify on insert (deliberate — release bundles
--     change at most once per operator action; the doctor's polling
--     loop is fine).

CREATE TABLE IF NOT EXISTS release_bundles (
    id              uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    git_sha         text        NOT NULL,
    manifest_hash   text        NOT NULL,
    daemon_hashes   jsonb       NOT NULL DEFAULT '{}'::jsonb,
    created_at      timestamptz NOT NULL DEFAULT now(),
    applied_at      timestamptz NULL,
    CONSTRAINT release_bundles_git_sha_shape
        CHECK (git_sha ~ '^[a-f0-9]{40}$'),
    CONSTRAINT release_bundles_manifest_shape
        CHECK (manifest_hash ~ '^sha256:[a-f0-9]{64}$')
);

CREATE INDEX IF NOT EXISTS release_bundles_git_sha_idx
    ON release_bundles(git_sha);

CREATE INDEX IF NOT EXISTS release_bundles_applied_at_idx
    ON release_bundles(applied_at) WHERE applied_at IS NOT NULL;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
--
-- Clean reverse: drop the partial index first (it references the
-- applied_at column), then the btree index, then the table itself.
-- The CHECK constraints drop implicitly with the table. Matches
-- the 00076 posture on compute_node_keys.
DROP INDEX IF EXISTS release_bundles_applied_at_idx;
DROP INDEX IF EXISTS release_bundles_git_sha_idx;
DROP TABLE IF EXISTS release_bundles;
-- +goose StatementEnd
