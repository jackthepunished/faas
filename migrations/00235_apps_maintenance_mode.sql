-- filename: 00235_apps_maintenance_mode.sql
-- +goose Up
-- +goose StatementBegin

-- apps.maintenance_mode (ADR-091 amendment, PR-B). The
-- coarse-grained customer-facing primitive for "this whole app is
-- in maintenance mode" — a single per-app boolean that 503s every
-- request to the app before auth, before wake, before any
-- kind=maintenance rule (coarse gate beats fine-grained per D4).
-- Free-tier allowed (no IsPaidOnly change). The customer flips it
-- via PATCH /v1/apps/{slug} (`maintenance_mode: true`); the
-- gatewayd-internal hot-path applier
-- (pkg/gateway.(*Handler).applyAppsMaintenanceMode, §4.1.2.0)
-- short-circuits every request to this app with 503 + Retry-After
-- (default 60 s, env-overridable via
-- FAAS_EDGE_RULE_MAINTENANCE_RETRY_AFTER_SECONDS).
--
-- One column:
--   maintenance_mode  boolean NOT NULL DEFAULT false
--
-- Mirror of the 00216_apps_route_metrics_enabled.sql shape (same
-- replay-safe ADD COLUMN IF NOT EXISTS, same partial index
-- `WHERE true`) — the column is a single NOT NULL boolean with a
-- constant default, no rewrite, no index bloat. The partial
-- index is small (the count of apps currently in maintenance is
-- expected to be tens, not thousands).
--
-- Default false keeps every pre-existing app on the normal
-- request path — opt-in by the customer via PATCH
-- /v1/apps/{slug}. No plan gate (Free and above may opt in).
--
-- The companion apps_maintenance_mode_notify trigger fires
-- pg_notify('app_changed', NEW.id::text) ONLY when
-- maintenance_mode IS DISTINCT FROM OLD.maintenance_mode, so the
-- channel stays low-volume (per-app flips, not every app
-- UPDATE). The gatewayd-internal listener
-- (cmd/gatewayd-internal/run.go) calls Backend.ResetApp(appID)
-- which `delete`s the entry from the per-host apps LRU (no TTL)
-- so the next Backend.Lookup repopulates the row from PG and
-- picks up the new MaintenanceMode value. Without the trigger the
-- apps LRU keeps the stale MaintenanceMode for the lifetime of
-- the cache entry — the first node to see the app returns 503
-- forever, every subsequent node returns 200. The apps LRU
-- invalidation is the load-bearing reason this trigger exists.
--
-- Pre-reserved at PR-A by migrations/00227_reserve_slot.sql
-- (since renumbered to migration 00235 by PR-B's 7-cycle renumber;
-- 00227 is now a fence on main owned by the kind=geo cluster
-- (PR #845) — the 00226/00227/00228 stampede is settled by
-- stepping this migration to 00235; 00232 is reserved by PR #864
-- ADR-093 request budgets; 00233 by PR #873 secretscan v2).

ALTER TABLE apps
    ADD COLUMN IF NOT EXISTS maintenance_mode boolean NOT NULL DEFAULT false;
CREATE INDEX IF NOT EXISTS apps_maintenance_mode_idx
    ON apps(maintenance_mode)
    WHERE maintenance_mode = true;

-- Notify trigger: only on a maintenance_mode flip.
-- Replay-safe: DROP IF EXISTS before CREATE so a second
-- goose-up pass (TestNewMigrationsAreReplaySafe) doesn't trip
-- SQLSTATE 42710 "trigger ... already exists". Mirrors the
-- pattern at 00212_github_webhook_secrets.sql.
CREATE OR REPLACE FUNCTION apps_maintenance_mode_notify() RETURNS trigger AS $$
BEGIN
    IF (TG_OP = 'UPDATE' AND NEW.maintenance_mode IS DISTINCT FROM OLD.maintenance_mode) THEN
        PERFORM pg_notify('app_changed', NEW.id::text);
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS apps_maintenance_mode_notify ON apps;
CREATE TRIGGER apps_maintenance_mode_notify
AFTER UPDATE ON apps
FOR EACH ROW
EXECUTE FUNCTION apps_maintenance_mode_notify();

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

-- Reverse: drop the trigger, function, partial index, then
-- column. A row that had maintenance_mode=true loses the bit on
-- downgrade; the GET /v1/apps/{slug} response shape omits
-- maintenance_mode because the column no longer exists, which
-- is the correct degraded behaviour (every app falls back to
-- normal request handling — the per-app applier sees
-- MaintenanceMode=false on every request). The trigger and
-- function drop is BEFORE the column drop so a downgrade on a
-- row that has maintenance_mode=true doesn't fire pg_notify
-- during the column drop.
DROP TRIGGER IF EXISTS apps_maintenance_mode_notify ON apps;
DROP FUNCTION IF EXISTS apps_maintenance_mode_notify();
DROP INDEX IF EXISTS apps_maintenance_mode_idx;
ALTER TABLE apps
    DROP COLUMN IF EXISTS maintenance_mode;

-- +goose StatementEnd