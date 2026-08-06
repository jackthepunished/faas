-- filename: 00155_apps_auth_default_flip.sql
-- +goose Up
-- Issue #695 / ADR-080 — flip the global default for apps auth from
-- public-by-default to authenticated-by-default. Closes spec §17 G15
-- (the open gap carried over from issue #560).
--
-- Two pieces in one migration because they MUST ship atomically:
--
--  1. New companion column apps.auth_default_flipped_at timestamptz
--     NULL. Stamped on every pre-flip app by the backfill below so
--     the dashboard banner query + `faas apps list` annotation can
--     render the "since YYYY-MM-DD" annotation. The column does
--     NOT change any existing app's behaviour — pre-flip rows keep
--     require_authn=false and public_auth_mode='open' exactly as
--     they were. This is the "grandfather" mechanism: every
--     existing app is marked as already-flip-acknowledged.
--
--  2. One batch audit row `apps.auth_default_global_flipped` emitted
--     by the migration itself (actor='migration', subject=NULL — the
--     transition is platform-wide, not per-app). Payload carries the
--     migrated_count + the from/to defaults per the ADR-080 §3
--     shape. This is the canonical audit record for the cut-over
--     moment; per-app PATCH flips continue to emit the existing
--     `app.authn_required` / `app.authn_disabled` /
--     `app.public_auth_changed` audit kinds without change.
--
-- Replay-safe (ADR-041):
--   - ADD COLUMN IF NOT EXISTS makes a second MigrateUp a no-op for
--     the schema piece.
--   - The UPDATE statement uses WHERE auth_default_flipped_at IS NULL
--     so a re-apply does NOT re-stamp existing rows (re-stamping
--     would shift the `since YYYY-MM-DD` suffix on the CLI
--     annotation and the dashboard banner display).
--   - The audit-row INSERT is guarded by `WHERE NOT EXISTS (SELECT 1
--     FROM events WHERE kind = 'apps.auth_default_global_flipped')`
--     so a re-apply is a clean no-op — the row is emitted exactly
--     once across the migration's lifetime.
--
-- No down migration: the schema column is dropped, but the audit row
-- stays (events is append-only per spec §11). A future rollback PR
-- reverts the Go-side defaults in pkg/api/limits.go and apid's
-- buildApp path; this migration is the storage piece only.

-- +goose StatementBegin
ALTER TABLE apps
    ADD COLUMN IF NOT EXISTS auth_default_flipped_at timestamptz NULL;
-- +goose StatementEnd

-- +goose StatementBegin
UPDATE apps
   SET auth_default_flipped_at = COALESCE(auth_default_flipped_at, now())
 WHERE auth_default_flipped_at IS NULL;
-- +goose StatementEnd

-- +goose StatementBegin
INSERT INTO events (actor, actor_account_id, kind, subject, data)
SELECT 'migration',
       NULL,
       'apps.auth_default_global_flipped',
       NULL,
       jsonb_build_object(
         'migrated_count',      (SELECT count(*) FROM apps WHERE auth_default_flipped_at IS NOT NULL),
         'from_require_authn_default',  false,
         'to_require_authn_default',    true,
         'from_public_auth_mode_default', 'open',
         'to_public_auth_mode_default',   'bearer',
         'plan_overrides', jsonb_build_object(
           'free',  jsonb_build_object('require_authn', to_jsonb(false::bool),  'public_auth_mode', to_jsonb('open'::text)),
           'hobby', jsonb_build_object('require_authn', to_jsonb(true::bool),   'public_auth_mode', to_jsonb('open'::text)),
           'pro',   jsonb_build_object('require_authn', to_jsonb(true::bool),   'public_auth_mode', to_jsonb('bearer'::text)),
           'scale', jsonb_build_object('require_authn', to_jsonb(true::bool),   'public_auth_mode', to_jsonb('bearer'::text))
         )
       )
 WHERE NOT EXISTS (
   SELECT 1 FROM events WHERE kind = 'apps.auth_default_global_flipped'
 );
-- +goose StatementEnd

-- +goose Down
-- Reverse: drop the companion column. The audit row stays in events
-- (append-only). The grand-father marker is gone — future reader of a
-- pre-Down DB sees no `auth_default_flipped_at` column; future
-- reader of an Up-Down-Up replay sees the column restored with NULL
-- values and the audit row once.
-- +goose StatementBegin
ALTER TABLE apps
    DROP COLUMN IF EXISTS auth_default_flipped_at;
-- +goose StatementEnd
