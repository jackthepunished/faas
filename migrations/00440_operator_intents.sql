-- filename: 00440_operator_intents.sql
-- +goose Up
-- ADR-127 / PR #1099 P2 redesign: durable intent queue for
-- `POST /v1/admin/instances/{id}/force-park` and
-- `POST /v1/admin/apps/{slug}/force-cold-boot`. Routes the
-- admin recovery primitives through the same
-- table + pg_notify + schedd-claim pattern as
-- `cron_fire_now_requests` (migrations/00194) — keeps apid out
-- of `pkg/scheddgrpc` per the `apid-control-plane-only` depguard
-- rule (.golangci.yml:41-58). Schedd is still the only writer to
-- `instances`; the trigger is now a Postgres row INSERT, not a
-- direct gRPC call.
--
-- Two producers, one consumer:
--
--   * apid (cmd/apid/handlers_admin_force_park.go::forcePark
--     and the cold-boot twin) is the only caller of
--     InsertOperatorIntent. Status starts at `pending`. After the
--     INSERT, apid emits `db.NotifyOperatorIntent` so schedd
--     picks up the row on the next LISTEN delivery.
--   * schedd (pkg/sched/operator_intent_subscriber.go) is the
--     only caller of ClaimPendingOperatorIntent /
--     MarkOperatorIntentSucceeded / MarkOperatorIntentFailed.
--     It dispatches by kind (force_park → Engine.Park,
--     force_cold_boot → Engine.ForceColdBootNextWake) and
--     stamps terminal state.
--
-- Status enum: pending (just inserted, awaiting schedd) →
-- running (schedd claimed, mid-dispatch) → succeeded | failed
-- (terminal). The 4-value active set matches the audit-event
-- `operator.action.<verb>.outcome` shape (result: "succeeded"
-- | "failed"). `cancelled` is reserved for a future operator
-- cancel surface — schema is forward-compatible.
--
-- Why a new table, not a column on `instances` / `deployments`:
-- an `operator_intent` column pollutes the lifecycle row with an
-- admin-action signal that doesn't belong there. A separate table
-- isolates the intent lifecycle, supports a future
-- pause|cancel|backfill surface, and keeps the audit row + error
-- in one place that operator queries can join against.
--
-- Cross-account safety: every helper takes an explicit
-- target_id + account_id parameter; schedd never queries by
-- account_id alone (the claim query selects by status='pending'
-- ORDER BY requested_at — the row's account_id is for audit-log
-- correlation, not access control). The API surface is gated by
-- the IDOR check in forcePark / forceColdBoot via the existing
-- apid allowlist (admin scope + s.adminAllows).
--
-- Idempotency: the apid handler is NOT wrapped in s.idempotent
-- today (admin actions are deliberate re-clicks, not retries).
-- The table has no UNIQUE constraint for that reason — two
-- clicks produce two intents, each dispatched independently.
--
-- Index: (status, requested_at) WHERE status='pending' matches
-- the `SELECT ... WHERE status = 'pending' ORDER BY
-- requested_at ASC FOR UPDATE SKIP LOCKED LIMIT 1` claim query
-- (mirrors cron_fire_now_requests_pending_idx at
-- migrations/00194:64-66 byte-for-byte). A (kind, requested_at)
-- index would NOT index-seek on the unfiltered claim query.
--
-- (target_id, requested_at DESC) supports the operator query
-- "show me the last 20 intents for this instance/deployment"
-- (the future `GET /v1/admin/instances/{id}/intents` surface;
-- today the per-intent GET is by id, not by target).
--
-- Replay-safety: every DDL uses IF NOT EXISTS. No FK target —
-- target_id is a free-text string that may be an instance uuid
-- OR a deployment uuid depending on the kind; the kind column
-- disambiguates.
--
-- Slot fence: 00440 was claimed for this PR (the 00430-00440
-- range is fenced per the cross-PR slot gate). 00431-00439
-- are reservation placeholders that shadow slots currently
-- held by PR #1036 (reals at 431-434), PR #1085 (reals at
-- 435-437), and PR #1024 (reals at 437-439). Leapfrog past
-- the highest sibling claim (00439) with a 1-slot margin —
-- same precedent as PR #1064 / 6898e4666 ("leapfrog E.2 past
-- PR #1036 + PR #1024 → 00440/00441").

CREATE TABLE IF NOT EXISTS operator_intents (
    id              uuid PRIMARY KEY,
    kind            text NOT NULL CHECK (kind IN
                       ('force_park','force_cold_boot')),
    target_id       text NOT NULL,
    account_id      uuid NULL,
    actor_id        uuid NOT NULL,
    reason          text NULL,
    metadata        jsonb NOT NULL DEFAULT '{}'::jsonb,
    status          text NOT NULL DEFAULT 'pending'
                       CHECK (status IN
                           ('pending','running','succeeded',
                            'failed','cancelled')),
    requested_at    timestamptz NOT NULL DEFAULT now(),
    started_at      timestamptz NULL,
    finished_at     timestamptz NULL,
    error           text NULL,
    snap_ids_marked_stale text[] NULL
);

-- Hot path: schedd's claim query. The (status, requested_at)
-- order matches the WHERE clause + ORDER BY exactly; the partial
-- WHERE keeps the index narrow (only pending rows are indexed,
-- so the 30s safety tick + the notify-driven drain both seek).
CREATE INDEX IF NOT EXISTS operator_intents_pending_idx
    ON operator_intents (status, requested_at)
    WHERE status = 'pending';

-- Lookup path: per-target history (most-recent-first). Future
-- per-target GET endpoint (GET /v1/admin/instances/{id}/intents)
-- will sort by requested_at DESC; this index is the read path.
CREATE INDEX IF NOT EXISTS operator_intents_target_idx
    ON operator_intents (target_id, requested_at DESC);

-- +goose Down
DROP INDEX IF EXISTS operator_intents_target_idx;
DROP INDEX IF EXISTS operator_intents_pending_idx;
DROP TABLE IF EXISTS operator_intents;