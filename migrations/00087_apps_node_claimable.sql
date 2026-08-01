-- +goose Up
-- +goose StatementBegin

-- filename: 00087_apps_node_claimable.sql
--
-- Phase 2 / Gate A — schedd-side async placement claim.
--
-- Pre-00087 apps.node_id was NOT NULL: the apid createApp handler
-- resolved placement in-process (PlacementScheduler.Choose) and
-- stamped the chosen node_id inside the same transaction that
-- called CreateAppIfUnderQuota. The lint gate tripped on that
-- design because the depguard rule apid-control-plane-only
-- (.golangci.yml:36-58) forbids apid from importing pkg/sched —
-- scheduling is the schedd's job, not control-plane's.
--
-- This migration relaxes apps.node_id to nullable so apid can
-- insert a fresh app with the owner undecided; schedd's
-- PlacementClaimSubscriber (pkg/sched/placement_claim.go)
-- atomically stamps the column on the first NotifyAppChanged
-- "created" event via Store.SetAppNodeID, which performs an
-- UPDATE … WHERE node_id IS NULL. The conditional UPDATE
-- serialises N schedds into exactly one winner; losers re-read
-- and find the row already bound to a peer's owner
-- (early-out, no engine mutation). The empty-uuid CHECK
-- (apps_node_id_nonempty_chk) stays in force: a stray INSERT
-- or UPDATE that tries to set node_id to the zero uuid still
-- trips 23514 — the only semantic shift is "NULL is now legal;
-- a real FK target is set asynchronously by schedd".
--
-- Replay-safety: ALTER TABLE … ALTER COLUMN … DROP NOT NULL is
-- idempotent. The down path re-tightens NOT NULL; Postgres
-- rejects the down if any row has node_id IS NULL (because the
-- 00086 backfill guarantees all pre-00087 rows have a real
-- node_id, and post-00087 every new row is stamped by schedd
-- before the down could be exercised). The
-- migrations/00087_apps_node_claimable_test.go suite pins this
-- behaviour.

alter table apps
  alter column node_id drop not null;

-- +goose StatementEnd
-- +goose Down
-- +goose StatementBegin

-- Re-tighten NOT NULL on the down path. Postgres errors loud
-- if any row has node_id IS NULL — operator must investigate
-- (a schedd is down or has not claimed) before retrying. We
-- do NOT silently coerce NULL to the empty uuid (that would
-- defeat the purpose of the relaxation).

alter table apps
  alter column node_id set not null;

-- +goose StatementEnd
