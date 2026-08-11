-- filename: 00208_github_webhook_secrets.sql
-- +goose Up
-- +goose StatementBegin

-- PR-D / ADR-012 §7 amendment — per-tenant GitHub App webhook secret.
--
-- Replaces the platform-wide FAAS_GITHUB_WEBHOOK_SECRET with a row-
-- per-installation_id lookup so a leaked tenant secret can rotate
-- without forcing every GitHub App install to re-coordinate. The
-- secret is stored as raw bytes (bytea) because the daemon-side
-- verifier at pkg/githubd/webhook.go:42-60 VerifyPushSignature reads
-- the wire body raw. A hex-decode on every webhook would be wasted
-- CPU at the gatewayd-internal proxy hot path.
--
-- Schema notes:
--   * installation_id is the GitHub Apps installation_id (bigint).
--     Unique-indexed as the PRIMARY KEY so the daemon-side resolver's
--     hot path is a single O(1) index lookup. `github_installations`
--     carries the same id but is keyed by account_id; we don't FK
--     because the secrets table has a tighter lifecycle (the install
--     row soft-deletes via account_id CASCADE; the secret row follows
--     via dedicated `delete_github_webhook_secret` on the apid side
--     in a future PR).
--   * secret_value is bytea (NOT hex-encoded text). The verifier
--     reads raw bytes via VerifyPushSignature.
--   * upgraded_at + upgraded_by form a §11 audit trail: every
--     `gregale github-webhook-secret set` invocation stamps the
--     operator who triggered the rotation. The Prometheus metric
--     `githubd_webhook_secret_total{status="set"}` is emitted
--     server-side at the same boundary.
--   * Replay-safety: CREATE TABLE IF NOT EXISTS is the load-bearing
--     guard. The replay pass through apply_walk_test.go is a no-op.
--
-- Cross-PR slot gate: PR-D landed at slot 208 (slot 207 was claimed
-- by PR #826's 00207_compute_node_heartbeats_stats). The fence
-- 00208_reserve_slot.sql was committed first per
-- cross-pr-slot-fence-pagination-gate (delete after this real
-- migration lands).

CREATE TABLE IF NOT EXISTS github_webhook_secrets (
    installation_id bigint PRIMARY KEY,
    secret_value    bytea NOT NULL,
    upgraded_at     timestamp with time zone NOT NULL DEFAULT now(),
    upgraded_by     text NOT NULL DEFAULT 'platform'
);

-- Index strategy: the PRIMARY KEY already gives us the unique
-- installation_id → secret_value lookup. No secondary indexes
-- are needed at this volume (one row per GitHub install).

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS github_webhook_secrets;
-- +goose StatementEnd
