-- filename: 00193_cron_fire_now_requests.sql
-- +goose Up
-- ADR-090 PR-C: durable request queue for `POST /v1/crons/{id}/run`.
--
-- apid (the only writer to this table) INSERTs a row when a customer
-- fires a cron, then emits `db.NotifyCronRunNow` so schedd picks it
-- up on the next LISTEN delivery. schedd (the only reader/claimer)
-- runs `SELECT … FOR UPDATE SKIP LOCKED LIMIT 1` to claim a row,
-- calls `sched.RunCronNow` in-process, then updates status +
-- invocation_id.
--
-- Why a new table, not a column on `crons`: a `fire_now_requested_at`
-- column pollutes the lifecycle row with an execution-intent signal
-- that doesn't belong there. A separate table isolates the fire-now
-- lifecycle, supports future `pause|cancel|backfill` semantics, and
-- keeps the audit row + invocation id + error in one place that
-- operator queries can join against.
--
-- Status enum: pending (just inserted, awaiting schedd) → running
-- (schedd claimed, mid-dispatch) → succeeded | failed | cancelled
-- (terminal). The 5-value CHECK matches the audit-event shape
-- (`cron.fired.manually` carries `status: "succeeded|failed"`); the
-- cancelled value is reserved for a future PR — schema is forward-
-- compatible.
--
-- Idempotency: the apid handler wraps the INSERT in `s.idempotent`
-- (server.go:1745-1766), so duplicate (account, Idempotency-Key)
-- pairs return the stored row without re-inserting. The table itself
-- has no UNIQUE constraint — the dedupe happens at the API boundary.
--
-- Index: (cron_id, requested_at DESC) supports both the operator
-- query "show me the last 20 fire-nows for this cron" (PR-A's
-- `GET /v1/crons/{id}/runs` could JOIN this table) and the schedd
-- hot path "next pending row to claim" (which is a status-indexed
-- sequential scan, not an index hit; the safety tick at 60s is the
-- recovery path, the notify is the fast path).
--
-- Invariant: `crons.id` is the FK target. ON DELETE CASCADE so a
-- deleted cron drops its pending fire-nows — a manual fire against a
-- deleted cron is meaningless and the customer's 404 is the API
-- surface that prevents it anyway, but the cascade is defence in
-- depth.
--
-- Replay-safety: every DDL uses IF NOT EXISTS. The `crons` table
-- exists since `00003_cron_last_fired.sql` so the FK reference is
-- stable across reapply.

CREATE TABLE IF NOT EXISTS cron_fire_now_requests (
    id              uuid PRIMARY KEY,
    cron_id         uuid NOT NULL REFERENCES crons(id) ON DELETE CASCADE,
    account_id      uuid NOT NULL,
    requested_at    timestamptz NOT NULL DEFAULT now(),
    status          text NOT NULL CHECK (status IN
                       ('pending','running','succeeded','failed','cancelled')),
    invocation_id   uuid NULL,
    error           text NULL,
    finished_at     timestamptz NULL
);

-- Hot path: schedd's claim query. The (status, requested_at) order
-- matches the `WHERE status = 'pending' ORDER BY requested_at ASC`
-- shape — sequential scan is fine for low request volume, this
-- index becomes useful if fire-now volume grows.
CREATE INDEX IF NOT EXISTS cron_fire_now_requests_pending_idx
    ON cron_fire_now_requests (status, requested_at)
    WHERE status = 'pending';

-- Lookup path: PR-A's `GET /v1/crons/{id}/runs` can JOIN this table
-- by cron_id. (cron_id, requested_at DESC) matches the JSONB
-- ordering contract.
CREATE INDEX IF NOT EXISTS cron_fire_now_requests_cron_idx
    ON cron_fire_now_requests (cron_id, requested_at DESC);

-- +goose Down
DROP INDEX IF EXISTS cron_fire_now_requests_cron_idx;
DROP INDEX IF EXISTS cron_fire_now_requests_pending_idx;
DROP TABLE IF EXISTS cron_fire_now_requests;
