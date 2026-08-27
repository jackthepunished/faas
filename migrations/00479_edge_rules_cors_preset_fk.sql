-- filename: 00479_edge_rules_cors_preset_fk.sql
-- +goose Up
-- +goose StatementBegin

-- Edge rules → cors presets FK (issue #975 item #4 / PR-B / ADR-129).
-- PR-A (00304) stood up the cors_presets data model and the read
-- path; PR-B wires the per-rule FK so a kind=cors rule can reference
-- a reusable preset instead of repeating allow_origins /
-- allow_methods / allow_headers / expose_headers /
-- allow_credentials / max_age_seconds inline. The compile-side
-- merge lives in cmd/gatewayd-internal/edge_rules.go::compileCORSRules
-- and calls MergeCorsPresetIntoRule
-- (pkg/state/cors_preset.go:100).
--
-- FK shape decisions (ADR-129 D1):
--
--   * Nullable column. A rule that does not reference a preset
--     (inline-only) has cors_preset_id = NULL. The column is NOT
--     set with a DEFAULT — NULL is the default, mirroring the
--     standard "FK may or may not be set" pattern. A DEFAULT would
--     be misleading because gen_random_uuid()'s UUIDs are never
--     valid cors_presets.id by construction.
--
--   * ON DELETE SET NULL. A customer deleting a preset must not
--     cascade-delete their edge rules. SET NULL keeps the rule in
--     place; the compile path (D3) then fails-closed with
--     ErrNotFound from MergeCorsPresetIntoRule, so the rule stops
--     matching CORS preflight until the customer wires a new
--     preset or inlines fallback values. The customer can read the
--     compile error from /v1/edge-rules/{id}.
--
--   * Partial index. cors_preset_id IS NOT NULL — most rules do
--     not reference a preset (the typical pattern is inline-only),
--     so a partial index keeps the index size bounded to the rules
--     that actually use it. The compile path needs to look up the
--     preset for a rule that sets cors_preset_id; this index covers
--     that lookup.
--
--   * FK target is cors_presets(id). The cors_presets table is the
--     canonical storage; no indirection through a join table (which
--     would require a separate create/delete surface and add no
--     value over a single-column FK).
--
-- Replay-safe: ADD COLUMN IF NOT EXISTS + DROP CONSTRAINT IF EXISTS
-- + ADD CONSTRAINT — same idempotent pattern 00293 uses (lines
-- 21-36). A second MigrateUp finds the column + constraint already
-- there, drops and recreates the constraint, leaves the column
-- alone. The replay_safety_test harness stays green.

ALTER TABLE edge_rules
  ADD COLUMN IF NOT EXISTS cors_preset_id UUID
    REFERENCES cors_presets(id) ON DELETE SET NULL;

ALTER TABLE edge_rules
  DROP CONSTRAINT IF EXISTS edge_rules_cors_preset_fk;

ALTER TABLE edge_rules
  ADD CONSTRAINT edge_rules_cors_preset_fk
    FOREIGN KEY (cors_preset_id) REFERENCES cors_presets(id)
      ON DELETE SET NULL;

-- Partial index covering the per-rule lookup. The compile path
-- (cmd/gatewayd-internal/edge_rules.go::compileCORSRules) loads the
-- preset for a rule that sets cors_preset_id; this index keeps the
-- lookup index-covered. Without the WHERE clause, the index would
-- be the same size as edge_rules itself (every rule has a UUID
-- column) — the partial predicate drops the index to only the
-- rows that use it. Same pattern as
-- 00345_edge_rules_kind_cache.sql's partial index pattern at lines
-- 121-122 (request_telemetry_trace_idx).
CREATE INDEX IF NOT EXISTS edge_rules_cors_preset_id_idx
  ON edge_rules (cors_preset_id)
  WHERE cors_preset_id IS NOT NULL;

-- pg_notify trigger for cache invalidation (ADR-129 D4). The
-- gatewayd-internal compile cache (cmd/gatewayd-internal
-- /edge_rules.go) holds a per-account preset overlay; on every
-- cors_presets INSERT/UPDATE/DELETE the gatewayd listener must
-- reload the affected account's overlay. The trigger fires
-- pg_notify('cors_preset_changed', NEW.account_id::text) on every
-- write so the listener reloads unconditionally — re-emit on
-- UPDATE is simpler than tracking which fields changed (the
-- overlay is small, the reload is index-covered). Mirrors the
-- apps_maintenance_mode_notify pattern at 00237_apps_maintenance
-- _mode.sql:65-78.
--
-- Replay-safe: DROP TRIGGER IF EXISTS before CREATE so a second
-- goose-up pass (TestNewMigrationsAreReplaySafe) doesn't trip
-- SQLSTATE 42710 "trigger ... already exists". Same pattern as
-- 00237 and 00212.

CREATE OR REPLACE FUNCTION cors_presets_changed_notify() RETURNS trigger AS $$
BEGIN
    IF (TG_OP = 'DELETE') THEN
        PERFORM pg_notify('cors_preset_changed', OLD.account_id::text);
    ELSE
        PERFORM pg_notify('cors_preset_changed', NEW.account_id::text);
    END IF;
    RETURN COALESCE(NEW, OLD);
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS cors_presets_changed_notify_trg ON cors_presets;
CREATE TRIGGER cors_presets_changed_notify_trg
AFTER INSERT OR UPDATE OR DELETE ON cors_presets
FOR EACH ROW
EXECUTE FUNCTION cors_presets_changed_notify();

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

-- Reverse order: trigger first, then function, then index, then
-- FK constraint, then column. The trigger+function drop goes
-- first so a downgrade doesn't fire pg_notify during the
-- column/constraint drop.

DROP TRIGGER IF EXISTS cors_presets_changed_notify_trg ON cors_presets;
DROP FUNCTION IF EXISTS cors_presets_changed_notify();
DROP INDEX IF EXISTS edge_rules_cors_preset_id_idx;
ALTER TABLE edge_rules DROP CONSTRAINT IF EXISTS edge_rules_cors_preset_fk;
ALTER TABLE edge_rules DROP COLUMN IF EXISTS cors_preset_id;

-- +goose StatementEnd
