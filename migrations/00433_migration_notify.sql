-- filename: 00433_migration_notify.sql
-- +goose Up
-- +goose StatementBegin

-- Multi-host safety cluster PR-2 (audit F2-B) / ADR-124 amendment.
--
-- The pg_advisory_lock from PR-1 wraps MigrateUp so concurrent daemons
-- in a fleet serialise before any DDL runs. The lock prevents the
-- "two daemons race on goose_db_version INSERT (SQLSTATE 23505)"
-- crash. It does NOT, on its own, give a deterministic boot order.
--
-- cmd/migrate -leader / cmd/migrate -wait-for-migrations (PR-2)
-- adds the deterministic boot order: the leader runs migrations
-- first, every other daemon blocks on a pg_notify('migrations_applied')
-- channel until the leader's last migration lands. This trigger
-- fires on the leader's final INSERT into goose_db_version (i.e.
-- when NEW.version_id == MAX(version_id)) and notifies. The
-- waiter in cmd/migrate/wait.go is the subscriber.
--
-- Why an INSERT-trigger on goose_db_version and not a sidecar
-- "migrations_complete" row: goose's own ledger is the source of
-- truth for "what was applied". A sidecar row would race the
-- ledger and require its own cross-process mutex to be reliable.
-- The ledger's INSERT is already serialised by the advisory lock
-- from PR-1, so the trigger fires exactly once per migration,
-- from the same connection that committed the INSERT.
--
-- Replay-safe: CREATE OR REPLACE FUNCTION + DROP TRIGGER IF EXISTS
-- before CREATE, mirroring 00237_apps_maintenance_mode.sql. Goose
-- replays the migration on a fresh DB the same way it always has;
-- the IF EXISTS / OR REPLACE pattern makes the second pass a
-- no-op instead of a 42710.
--
-- Payload: NEW.version_id::text (decimal int64). The waiter treats
-- the payload as informational — it re-reads goose_db_version
-- directly to confirm v == MaxEmbedded before returning success.
-- This guards against notify-loss during a Postgres restart
-- between the INSERT and the delivery.
--
-- Companion surface: cmd/migrate/wait.go WaitForMigrationsApplied
-- and the NotifyMigrationsApplied constant in pkg/db/notify.go.
--
-- Slot note: PR-2 originally claimed 00347 (branched off post-00346).
-- A flood of #1006 safe-releases mega fences consumed 00350..00375
-- on main; PR #1023 (ADR-124 app_protocol) claimed 00376-00382;
-- PR #1019 (mirror) claimed 00383-00386; PR #1046 (openapi-import)
-- claimed 00383; PR #1049 (openapi-import rebump) and #1070 (wake-
-- failure) absorbed 00387-00416. PR-2 was renumbered to 00433 (chain
-- through 00419 → 00423 → 00427 → 00431 → 00433 to dodge PR #1017,
-- PR #1024, PR #1064, and PR #1067 collision waves) to land past
-- PR-4's 00431_compute_nodes_active_unique.sql and PR-5's
-- 00432_instances_wake_attempt_active_unique.sql. See
-- docs/runbooks/migration-slot-dance.md.

CREATE OR REPLACE FUNCTION migration_notify() RETURNS trigger AS $$
BEGIN
    -- Only fire when the new row is at the leading edge of the
    -- applied ledger. Earlier rows (e.g. a downgrade script run
    -- out-of-band, or a partial-replay rollback) do not signal
    -- "we're caught up". The waiter is gated on MAX(version_id)
    -- after every notification, so a spurious early fire is
    -- harmless; a missed late fire is impossible because goose
    -- inserts in strict ascending order inside the advisory lock.
    IF NEW.is_applied IS DISTINCT FROM true THEN
        RETURN NEW;
    END IF;
    IF NEW.version_id = (SELECT MAX(version_id) FROM goose_db_version) THEN
        PERFORM pg_notify('migrations_applied', NEW.version_id::text);
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS migration_notify_trg ON goose_db_version;
CREATE TRIGGER migration_notify_trg
AFTER INSERT ON goose_db_version
FOR EACH ROW
EXECUTE FUNCTION migration_notify();

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP TRIGGER IF EXISTS migration_notify_trg ON goose_db_version;
DROP FUNCTION IF EXISTS migration_notify();

-- +goose StatementEnd
