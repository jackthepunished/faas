-- filename: 00271_compute_nodes_release.sql
-- +goose Up
-- +goose StatementBegin
--
-- 00271_compute_nodes_release.sql — issue #911 / ADR-110 release-bundle
-- storage carrier (PR-3a). Six nullable columns on compute_nodes that
-- PR-3 (release bundle content + install) and PR-4 (gregale doctor)
-- consume:
--
--   release_id        — pointer back to release_bundles.git_sha
--                       (see migration 00272). NULL = pre-bundle row,
--                       or a node that hasn't been registered against
--                       a release yet (single-box installs without a
--                       manifest).
--   manifest_hash     — sha256:<64hex> hash of the manifest the PR-2
--                       renderer materialized on this node (parallels
--                       release_bundles.manifest_hash). NULL = pre-
--                       manifest row. Doctor compares this against
--                       release_bundles.manifest_hash for the
--                       release_id to detect manifest drift.
--   host_certificate  — PEM-encoded leaf cert for this node, written
--                       by either cmd/hostage-gen (legacy) or
--                       gregale secrets init (PR-X). Doctor reads
--                       this to verify the cert on disk matches
--                       the row's cert_fingerprint.
--   cert_fingerprint  — sha256:<64hex> fingerprint of host_certificate.
--                       NULL until PR-X or cmd/hostage-gen stamps it.
--                       Wire-level consumers (PR-3 bundle install,
--                       PR-4 doctor) compare this against
--                       pkg/pki.LoadCertificateFingerprint at mTLS
--                       handshake time.
--   role              — per-node role label (control-plane |
--                       compute-node). Populated from
--                       manifest.fleet.hosts[].role by the PR-2
--                       renderer; NULL = pre-manifest row.
--   generation        — monotonic counter bumped by the PR-4 doctor
--                       when a per-node inconsistency is detected.
--                       Default 0; the counter never decreases.
--
-- All six columns are nullable so pre-PR-3a compute_nodes rows (the
-- seeded 'default-local' row from 00024 plus any operator-added rows
-- since then) accept the schema without a backfill transaction. The
-- partial index compute_nodes_active_idx (00024) is unchanged; the
-- chooser reads columns it already understands (region/zone/
-- schedd_target_url) and stays oblivious to these six.
--
-- No new admission or billing semantics — the authoritative caps
-- stay in pkg/api/limits.go (CLAUDE.md). Replay-safety (per the
-- contract migrations/replay_safety_test.go asserts): every
-- ADD COLUMN is IF NOT EXISTS, every DROP COLUMN is IF EXISTS.
-- A drifted box (schema present, goose row missing) re-applies
-- cleanly without tripping SQLSTATE 42P07 / 42710.
--
-- Out of scope for this migration (consumed by later cluster PRs):
--   - PR-3: bundle install populates release_id / cert_fingerprint
--     on UPSERT.
--   - PR-2 renderer: populates manifest_hash + role.
--   - PR-X secrets init: writes host_certificate + cert_fingerprint.
--   - PR-4 doctor: bumps generation on drift detection.

ALTER TABLE compute_nodes
    ADD COLUMN IF NOT EXISTS release_id       text,
    ADD COLUMN IF NOT EXISTS manifest_hash    text,
    ADD COLUMN IF NOT EXISTS host_certificate text,
    ADD COLUMN IF NOT EXISTS cert_fingerprint text,
    ADD COLUMN IF NOT EXISTS role             text,
    ADD COLUMN IF NOT EXISTS generation       int;

COMMENT ON COLUMN compute_nodes.release_id       IS
    'release_bundles.git_sha this node claims membership in (PR-3a). NULL = pre-bundle row. Populated by PR-3 release install + PR-2 renderer.';
COMMENT ON COLUMN compute_nodes.manifest_hash    IS
    'sha256:<64hex> hash of the manifest the PR-2 renderer materialized on this node (PR-3a). NULL = pre-manifest row. Compared against release_bundles.manifest_hash by PR-4 doctor.';
COMMENT ON COLUMN compute_nodes.host_certificate IS
    'PEM-encoded leaf certificate for this node (PR-3a). NULL = pre-PR-X or pre-cmd/hostage-gen row. Doctor reads to verify cert on disk matches cert_fingerprint.';
COMMENT ON COLUMN compute_nodes.cert_fingerprint IS
    'sha256:<64hex> fingerprint of host_certificate (PR-3a). NULL until secrets init (PR-X) or cmd/hostage-gen stamps it. PR-3 bundle install + PR-4 doctor compare against pkg/pki.LoadCertificateFingerprint at mTLS handshake time.';
COMMENT ON COLUMN compute_nodes.role             IS
    'per-node role label: control-plane | compute-node (PR-3a). Populated from manifest.fleet.hosts[].role by PR-2 renderer. NULL = pre-manifest row.';
COMMENT ON COLUMN compute_nodes.generation       IS
    'monotonic counter bumped by PR-4 doctor on per-node inconsistency detection (PR-3a). Default 0; never decreases.';

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
--
-- Reverse: drop the six columns. A row that wrote release-bundle
-- metadata under the new columns will lose it on downgrade; the
-- GET /v1/compute-nodes/{id} response shape omits the fields because
-- the columns no longer exist, which is the correct degraded
-- behaviour (mirrors 00079's posture on deployment_overrides).
ALTER TABLE compute_nodes
    DROP COLUMN IF EXISTS generation,
    DROP COLUMN IF EXISTS role,
    DROP COLUMN IF EXISTS cert_fingerprint,
    DROP COLUMN IF EXISTS host_certificate,
    DROP COLUMN IF EXISTS manifest_hash,
    DROP COLUMN IF EXISTS release_id;
-- +goose StatementEnd
