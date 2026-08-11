-- filename: 00209_github_webhook_secrets.sql
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
--   * The github_webhook_secret_changed pg_notify trigger fires
--     AFTER INSERT OR UPDATE so the daemon-side resolver at
--     pkg/githubd/webhook_secret.go can drop its cached entry on
--     rotation (closing the 60s TTL fail-closed window). The
--     payload is the installation_id as text; the consumer re-reads
--     the row defensively per the pg_notify contract.
--   * Replay-safety: CREATE TABLE IF NOT EXISTS + CREATE OR REPLACE
--     FUNCTION are the load-bearing guards. The replay pass through
--     apply_walk_test.go is a no-op.
--
-- Cross-PR slot gate: PR-D landed at slot 209 (slot 207 was claimed
-- by PR #826's 00207_compute_node_heartbeats_stats; slot 008 holds
-- the 00208_reserve_slot.sql fence per the cross-pr-slot-fence-
-- pagination-gate pattern). The 00208_reserve_slot.sql fence
-- commits first so the renumber chain doesn't collide if both
-- PRs land in the same merge window.

CREATE TABLE IF NOT EXISTS github_webhook_secrets (
    installation_id bigint PRIMARY KEY,
    secret_value    bytea NOT NULL,
    upgraded_at     timestamp with time zone NOT NULL DEFAULT now(),
    upgraded_by     text NOT NULL DEFAULT 'platform'
);

-- Index strategy: the PRIMARY KEY already gives us the unique
-- installation_id → secret_value lookup. No secondary indexes
-- are needed at this volume (one row per GitHub install).

-- pg_notify trigger — api/githubd invalidation bridge (PR-D).
-- Mirrors the convention in 00026_compute_node_notify.sql +
-- 00031_invocations_notify.sql. The function is CREATE OR REPLACE
-- so the migration is idempotent across apply_walk_test.go replays.
CREATE OR REPLACE FUNCTION github_webhook_secrets_notify() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    PERFORM pg_notify(
        'github_webhook_secret_changed',
        jsonb_build_object('installation_id', NEW.installation_id)::text
    );
    RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS github_webhook_secrets_notify_trg ON github_webhook_secrets;
CREATE TRIGGER github_webhook_secrets_notify_trg
    AFTER INSERT OR UPDATE ON github_webhook_secrets
    FOR EACH ROW EXECUTE FUNCTION github_webhook_secrets_notify();

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TRIGGER IF EXISTS github_webhook_secrets_notify_trg ON github_webhook_secrets;
DROP FUNCTION IF EXISTS github_webhook_secrets_notify();
DROP TABLE IF EXISTS github_webhook_secrets;
-- +goose StatementEnd

