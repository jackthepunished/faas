-- +goose Up
-- +goose StatementBegin
-- filename: 00446_operator_intents_force_restart_kind.sql
--
-- P2d follow-on (PR #1105) — widens the closed-vocabulary CHECK
-- constraint on operator_intents.kind from
-- ('force_park','force_cold_boot') to
-- ('force_park','force_cold_boot','force_restart'). Mirrors the
-- precedent at migrations/00430_compute_node_heartbeats_builder_tick.sql
-- (PR #1099 P5) byte-for-byte: anonymous CHECK → auto-name
-- operator_intents_kind_check → DROP CONSTRAINT IF EXISTS +
-- ADD CONSTRAINT <same-name> makes the migration idempotent
-- against the canonical auto-name.
--
-- The 'force_restart' kind dispatches from
-- pkg/sched/operator_intent_subscriber.go's switch to
-- Engine.ForceRestart (pkg/sched/engine.go). It is the
-- operator-initiated kill-instance + cold-boot-on-next-wake
-- primitive — the third arm of the operator-action toolbox,
-- completing the {park, cold-boot, restart} triple.
--
-- Schema-layer enforcement rationale (mirrors 00430's doc):
--
--   The CHECK is the source of truth for the closed kind set;
--   any drift between the pgstore InsertOperatorIntent call
--   site and the schedd subscriber's switch arms surfaces as
--   a 23514 error at runtime, not at compile time. Keeping the
--   vocabulary here means a future sibling migration that
--   adds a fourth kind has to update both this DDL and the
--   dispatch switch — the load-bearing coupling.
--
-- Migration replay-safety: the IF EXISTS guards make the
-- migration idempotent under goose replay; the second apply
-- no-ops on the DROP and the re-ADD includes 'force_restart'
-- so the CHECK shape matches the first apply byte-for-byte.

alter table operator_intents
    drop constraint if exists operator_intents_kind_check;

alter table operator_intents
    add constraint operator_intents_kind_check
        check (kind in ('force_park','force_cold_boot','force_restart'));

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

alter table operator_intents
    drop constraint if exists operator_intents_kind_check;

alter table operator_intents
    add constraint operator_intents_kind_check
        check (kind in ('force_park','force_cold_boot'));

-- +goose StatementEnd
