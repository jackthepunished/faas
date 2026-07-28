-- +goose Up
-- +goose StatementBegin

-- issue #279 (PR-B, CPU-hour visibility, no billing): the
-- usage_minutes table now also accumulates the cumulative
-- CPU-µs each instance consumed in the minute. The values come
-- from the vmmd cpustats cache via the schedd
-- instancestats.Poller (pkg/sched/instancestats/poller.go),
-- are summed by the meterd sampler
-- (pkg/meter/sampler.go::SampleAndRoll), and written here.
--
-- cpu_usec is a measurement, NOT a billable unit: the financial
-- model still bills on plan RAM + 8 MB (pkg/api/limits.go,
-- pkg/meter/math.go). The data lands in usage_minutes because
-- that is the canonical per-(account, app, instance, minute)
-- table; the read path is wired so a follow-up PR can extend
-- pkg/billing.Provider.PushUsageRecord without re-plumbing
-- sampling.
--
-- ON CONFLICT semantics (issue #279, contrast with
-- 00075-ish PR #75 which set mb_seconds to DO NOTHING):
-- mb_seconds is "the plan RAM times the wall-clock minutes
-- the instance was up", so it is stable across the minute
-- and DO NOTHING is correct. cpu_usec is "the sum of
-- per-sample deltas across the minute" and the schedd
-- accumulator can call AppendUsage many times within the
-- same minute; DO UPDATE SET cpu_usec = cpu_usec + EXCLUDED.cpu_usec
-- is the only correct semantics here. The pusher (meter
-- → billing) deduplicates on a coarser window before
-- pushing, so the additive merge is safe end-to-end.

alter table usage_minutes
    add column if not exists cpu_usec bigint not null default 0;

comment on column usage_minutes.cpu_usec is
    'Cumulative host cgroup CPU-µs consumed by the instance during this minute. Source: vmmd cpustats.Cache (cpu.stat usage_usec delta) → schedd instancestats.Poller → meterd Sampler. Measurement only — billing is on plan RAM. issue #279 / PR-B.';

-- usage_monthly view (defined in 00002_app_manifest_and_domains.sql)
-- is recreated here to include the new cpu_usec column. We do not
-- edit 00002 (migrations are append-only, per CLAUDE.md and the
-- migrations-check Makefile target). The view is idempotent: the
-- exact same SQL is dropped and re-created on every migration run
-- via the OR REPLACE on the view name.

drop view if exists usage_monthly;
create or replace view usage_monthly as
  select
    account_id,
    app_id,
    date_trunc('month', minute) as month,
    sum(mb_seconds) as mb_seconds,
    sum(cpu_usec)   as cpu_usec,
    sum(requests)   as requests
  from usage_minutes
  group by account_id, app_id, date_trunc('month', minute);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

-- Restore the original view shape (no cpu_usec) so the Down
-- direction is a clean reversal of the Up direction. We do
-- not touch 00002 — the next migration that recreates the
-- view will overwrite this one again on its own Up.

drop view if exists usage_monthly;
create or replace view usage_monthly as
  select
    account_id,
    app_id,
    date_trunc('month', minute) as month,
    sum(mb_seconds) as mb_seconds,
    sum(requests)   as requests
  from usage_minutes
  group by account_id, app_id, date_trunc('month', minute);

alter table usage_minutes
    drop column if exists cpu_usec;

-- +goose StatementEnd
