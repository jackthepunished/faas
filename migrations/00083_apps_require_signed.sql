-- filename: 00083_apps_require_signed.sql
-- +goose Up
-- Per-app cosign signature-enforcement (issue #472, ADR-058).
-- Two shapes land in this single slot:
--
--   (1) ALTER TABLE apps ADD COLUMN require_signed — operator toggle.
--   (2) CREATE TABLE app_trusted_signers — per-app trust list mirroring
--       AWS Lambda's CodeSigningConfig.
--
-- Slot 83 chosen at PR-creation time per `migrations/README.md` after
-- origin/main already had 00081 (compute_nodes_vcpu_budget) and
-- 00082 (apps_scaling_policy). The two changes are tightly coupled
-- (the column is the operator toggle, the table is the data) — keeping
-- them in one slot matches the migration-order invariant in ADR-041.
-- Slot reservation per §17 G4: 00083_reserve_slot.sql is a placeholder
-- that drops to free the prefix for the slot-collision CI gate.

-- (1) Per-app require_signed flag.
--
-- Default false keeps every pre-existing app on the open-deploy path —
-- opt-in by the operator (admin-scoped) via PATCH /v1/apps/{slug}/security.
-- The flag is enforced in imaged (after pg_notify dispatch, before
-- PullDigest); apid writes the deployment row immediately
-- (status=pending) and stamps require_signed so imaged's buildImageLayer
-- knows to call cosign verify. On failure, imaged marks the deployment
-- FAILED with a structured failure_reason and emits an audit event
-- (app.signature_missing / app.signature_invalid).
--
-- Source-tarball deploys (Railpack path) are unaffected — they never
-- touch a customer OCI image, so the apid pre-flight gate skips the
-- trust-list check on the multipart branch.
--
-- Replay-safe (PR #377 / ADR-041): ADD COLUMN IF NOT EXISTS. The column
-- is a single NOT NULL boolean with a constant default — no rewrite,
-- no index bloat, and a second MigrateUp is a no-op.
--
-- No partial index. The operator "which apps require signing?" query
-- path is small (admins only) and always app-scoped; a full scan is
-- fine. Mirrors the apps table shape from migration 00080 which also
-- skipped an index for the same reason.
-- +goose StatementBegin
ALTER TABLE apps
    ADD COLUMN IF NOT EXISTS require_signed boolean NOT NULL DEFAULT false;
-- +goose StatementEnd

-- (2) Per-app trusted-publisher list.
--
-- Mirrors AWS Lambda's CodeSigningConfig: a per-app set of allowed
-- cosign public keys; deploys whose image is not signed by one of
-- these keys are rejected by imaged.
--
-- Schema:
--   account_id         uuid     NOT NULL  -- accounts.id (cascade on account delete)
--   app_id             uuid     NOT NULL  -- soft FK: cascade is handled in
--                                       -- pgstore.DeleteApp (same shape as
--                                       -- app_secrets 00008 / app_envs 00061)
--   signer_name        text     NOT NULL  -- operator-chosen label
--                                       -- (e.g. "ci-bot", "prod-publisher")
--   cosign_public_key  bytea    NOT NULL  -- DER-encoded SubjectPublicKeyInfo
--                                       -- bytes (loaded from the .pub PEM
--                                       -- upload at PUT time; stored as the
--                                       -- raw DER so the verify path doesn't
--                                       -- re-parse on every deploy)
--   added_at           timestamptz NOT NULL DEFAULT now()
--   added_by_account_id uuid    NOT NULL  -- accounts.id of the admin who
--                                       -- onboarded this signer (audit trail)
--
-- Primary key is (app_id, signer_name) so an operator can re-PUT a key
-- to rotate without an extra DELETE step. ON DELETE CASCADE on
-- account_id mirrors the app_secrets / app_envs shape so a GDPR-erased
-- account drops its trust list automatically. app_id has NO hard FK
-- for the same reason the secrets/env tables skip one — pgstore.DeleteApp
-- cascades trust rows manually so app deletion stays atomic.
--
-- The signer_name CHECK pins the operator label to a DNS-1123-label
-- style so it can be safely round-tripped through CLI args and audit
-- event payloads. The cosign_public_key CHECK pins the byte length
-- range so a regression that stores an empty blob or a megabyte blob
-- surfaces at insert time rather than at deploy time. 64-1024 bytes
-- covers ECDSA P-256 (the only key type pkg/cosign currently produces
-- per ADR-038) with ample slack; multi-key bundles are stored as one
-- row per publisher (signer_name) so each row stays small.
--
-- Index on app_id is redundant with the PK but kept for consistency
-- with the app_secrets / app_envs pattern (the planner uses it for
-- "list all signers across the system" operator queries).
--
-- No pg_notify trigger here — apid issues pg_notify on every
-- UpsertAppTrustedSigner / DeleteAppTrustedSigner directly so imaged
-- can re-read its in-memory cache without restart. (See
-- docs/adr/058-cosign-deploy-time-enforcement.md §R2.)
-- +goose StatementBegin
CREATE TABLE IF NOT EXISTS app_trusted_signers (
    account_id          uuid        NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    app_id              uuid        NOT NULL,
    signer_name         text        NOT NULL,
    cosign_public_key   bytea       NOT NULL,
    added_at            timestamptz NOT NULL DEFAULT now(),
    added_by_account_id uuid        NOT NULL,
    PRIMARY KEY (app_id, signer_name),
    CONSTRAINT app_trusted_signers_name_shape
        CHECK (signer_name ~ '^[a-z0-9][a-z0-9_-]{0,63}$'),
    CONSTRAINT app_trusted_signers_pem_shape
        CHECK (octet_length(cosign_public_key) BETWEEN 64 AND 1024)
);
CREATE INDEX IF NOT EXISTS app_trusted_signers_app_idx
    ON app_trusted_signers(app_id);
-- +goose StatementEnd

-- +goose Down
-- Reverse: drop the table (cascades through indexes), then drop the
-- column. A row that had require_signed=true loses the bit on
-- downgrade; the GET /v1/apps/{slug} response shape omits
-- require_signed because the column no longer exists, which is the
-- correct degraded behaviour (open-deploy path is restored).
-- +goose StatementBegin
DROP INDEX IF EXISTS app_trusted_signers_app_idx;
DROP TABLE IF EXISTS app_trusted_signers;
ALTER TABLE apps
    DROP COLUMN IF EXISTS require_signed;
-- +goose StatementEnd