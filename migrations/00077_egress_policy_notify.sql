-- +goose Up
-- +goose StatementBegin

-- filename: 00077_egress_policy_notify.sql
--
-- 00077_egress_policy_notify.sql — ADR-055, Tier 1 Phase 4.
--
-- The egress_policy table is the per-host policy audit row. The
-- canonical values still live in pkg/netns.DefaultHostPolicy (the
-- Go source of truth); the audit row exists so an operator can
-- update the policy from outside the ansible role and have
-- compute nodes pick it up via pg_notify, without re-running
-- `make bootstrap`. The watcher (cmd/vmmd/egress_watcher.go)
-- consumes the egress_policy_changed channel, re-renders with
-- the local host's compile-time defaults, validates via
-- `nft -c -f <staging>`, and atomic-replaces the live
-- /etc/nftables.conf.
--
-- Schema choices:
--
--   - Single-row table (PRIMARY KEY on a constant 'singleton' with
--     a CHECK constraint). The audit row holds ONE row named
--     'singleton'; a future per-tenant policy tier (ADR-057)
--     removes the singleton constraint and adds a tenant_id key.
--     The single-row shape is what the pg_notify trigger can
--     rely on without missing drops.
--
--   - public_iface and masquerade_cidr are TEXT with no shape
--     CHECK. The Go renderer validates them at reload time (the
--     package's panic-on-empty contract, policy.go:111-120) and
--     nft(8) itself rejects malformed CIDRs at `nft -c -f`. A DB
--     CHECK would only mirror what the renderer already validates
--     and would have to track every renderer field — a coupling
--     that risks drift.
--
--   - changed_at timestamptz DEFAULT now() lets ops see the last
--     update; the channel payload echoes this for log correlation.
--
--   - The pg_notify trigger fires AFTER INSERT/UPDATE on the
--     singleton row. No DELETE-trigger: ops do not delete the
--     row; an UPDATE with the same values re-emits the channel
--     harmlessly (the watcher's render is idempotent given the
--     same inputs).
--
-- Replay-safety (the contract migrations/replay_safety_test.go
-- asserts): every DDL is IF NOT EXISTS / DO-block-guarded. A
-- drifted box (schema present, goose row missing) re-applies
-- cleanly without tripping SQLSTATE 42P07 / 42710.

create table if not exists egress_policy (
    id                text        not null,
    public_iface      text        not null,
    masquerade_cidr   text        not null,
    changed_at        timestamptz not null default now(),
    primary key (id),
    -- Singleton row. The audit table holds exactly one row named
    -- 'singleton'. A future ADR relaxes this for per-tenant
    -- policy, but the watcher today only knows the local host's
    -- compile-time defaults, so a single row is the right shape.
    constraint egress_policy_singleton check (id = 'singleton')
);

-- Seed the singleton row so the watcher has a well-defined target
-- on first apply. The values mirror pkg/netns.DefaultHostPolicy
-- (the EX44 default-local node shape: eth0 + 10.100.0.0/16). A
-- box with a different public_iface overrides via UPSERT from the
-- ansible role post-bootstrap (out of scope here; the apply-time
-- default is the production default).
insert into egress_policy (id, public_iface, masquerade_cidr)
     values ('singleton', 'eth0', '10.100.0.0/16')
on conflict (id) do nothing;

-- pg_notify trigger. PL/pgSQL function emits the channel name with
-- a small JSON payload so the watcher can log which audit row
-- changed. The payload fields are informational; the watcher
-- re-renders from the local host's compile-time defaults, not
-- from the payload (mirrors cmd/vmmd/capacity_publisher.go's
-- "freshness, not authority" treaty).
create or replace function egress_policy_notify() returns trigger as $$
begin
    perform pg_notify(
        'egress_policy_changed',
        json_build_object(
            'policy_id', new.id,
            'public_iface', new.public_iface,
            'masquerade_cidr', new.masquerade_cidr,
            'changed_at', new.changed_at
        )::text
    );
    return null;
end;
$$ language plpgsql;

drop trigger if exists egress_policy_changed_trg on egress_policy;
create trigger egress_policy_changed_trg
    after insert or update on egress_policy
    for each statement execute function egress_policy_notify();

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

drop trigger if exists egress_policy_changed_trg on egress_policy;
drop function if exists egress_policy_notify();
drop table if exists egress_policy;

-- +goose StatementEnd
