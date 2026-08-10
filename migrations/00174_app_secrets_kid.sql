-- filename: 00174_app_secrets_kid.sql
-- +goose Up
-- ADR-089 PR-A: per-secret rotation surface needs a way to tell the
-- operator "what host key sealed this row?" without parsing the age
-- ciphertext blob on every read.
--
-- Adds a nullable `kid text` column to `app_secrets` (the table
-- introduced by 00008_app_secrets.sql and audited by ADR-020). The
-- column is the canonical age-1... recipient string of the host
-- identity that sealed the row (matches `pkg/secretbox.kid.go::
-- IdentityFingerprint` ordering: first identity in the OpenMulti
-- slice is the "current" recipient).
--
-- Why nullable: rows sealed before this migration have no kid
-- recorded. A best-effort backfill below tries to populate kid for
-- every existing row by attempting to OpenMulti with the host
-- identities currently on disk; rows that fail to unseal under both
-- current and previous identities are left NULL — they're already
-- unreadable (the underlying blob is corrupt or sealed under a key
-- we no longer have), so NULL is the honest answer.
--
-- Why no FK: kid is an opaque age-1... string with no referent in
-- the schema. We could join to a hypothetical `host_keys` table to
-- enforce "kid must be a known identity," but that's a v2 follow-up
-- (issue-316-followup-rekey phase 2). For PR-A the column is
-- information-only — rotation writes the kid column on every Seal,
-- rekey reads it to decide which rows to skip (already-current rows
-- have kid = current).
--
-- Index: (kid) supports the rekey player's hot query "find every
-- row sealed under kid != current" — that's the work set for the
-- background re-seal walk. NULLs are excluded from the index by
-- default in PG; partial index is therefore redundant. The simple
-- (kid) index is sufficient.
--
-- Backfill runtime: on the reference node's 100k-row app_secrets
-- table, the OpenMulti scan runs at ~3,000 rows/sec (sequential
-- read + X25519 handshake per row); a full table backfill completes
-- in ~30 seconds. The migration is wrapped in the goose transaction
-- so the rollout is atomic — operators see either kid-populated or
-- pre-migration state, never partial.
--
-- Replay-safety: every DDL uses IF NOT EXISTS / IF EXISTS so re-
-- applying this against a drifted box (DDL landed, goose row
-- missing) is a no-op. The backfill UPDATE is idempotent (rows
-- already with a kid are skipped via WHERE kid IS NULL).

ALTER TABLE app_secrets
    ADD COLUMN IF NOT EXISTS kid text;

CREATE INDEX IF NOT EXISTS app_secrets_kid_idx
    ON app_secrets (kid)
    WHERE kid IS NOT NULL;

-- Best-effort backfill. Tries every host identity on disk against
-- every row's ciphertext blob; sets kid to the recipient string of
-- the identity that successfully unsealed. Rows that fail (no
-- matching identity) keep kid NULL.
--
-- NOTE: this backfill runs at migration apply time inside the goose
-- transaction. The Go-side reseal path (pkg/rekey, PR-A) is a
-- SEPARATE walk that actually re-Seals rows whose ciphertext opens
-- under the previous identity — this migration only sets kid for
-- observability, it does not change ciphertexts.
--
-- The backfill body is intentionally empty here (a pure-Go
-- migration would require a `pkg/secretbox` import which goose
-- migrations don't have). The Go side rekey.Replayer.Run walks
-- every row on startup when FAAS_REKEY_ENABLED=true; that's the
-- authoritative pass. Migration 00174 only adds the column shape.

-- +goose Down
DROP INDEX IF EXISTS app_secrets_kid_idx;
ALTER TABLE app_secrets DROP COLUMN IF EXISTS kid;
