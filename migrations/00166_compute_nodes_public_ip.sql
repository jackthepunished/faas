-- filename: 00166_compute_nodes_public_ip.sql
-- +goose Up
-- Per-compute-node public egress address (Tier A9 / ADR-084).
--
-- The standby write-redirect (pkg/gateway/writegate +
-- cmd/gatewayd-internal/write_gate.go) needs the leader's public
-- egress IP so a standby box can dial `https://<leaderPublicIP>/`
-- over mTLS when it receives a bearer-authenticated mutation
-- while the leader is on a different physical host.
--
-- Today every compute_nodes row tracks (name, node_id, region,
-- zone, admission_ceiling_mb, vcpu_budget) but NOT the public
-- egress address. The leader URL is operator-managed DNS in
-- production (CNAME-ing the cluster hostname at the public edge),
-- but on a multi-host Lima fleet (deploy/lima/faas-metal-2b) each
-- box has a distinct host IP — the gate needs that IP to relay.
--
-- Both columns are NULLABLE for backwards compatibility with the
-- synthetic default-local row seeded by migration 00024: a
-- single-box install never sets public_ip, and the gate's
-- LeaderResolver handles the empty-public-ip case as
-- `OutcomeLeaderUnreachable` (fail-closed, see ADR-084 §Decision
-- 7). The CLI upserts via `gregale compute-nodes set-public-ip`
-- once PR-B's wire lands.
--
-- Replay-safe (PR #377 / ADR-041): ADD COLUMN IF NOT EXISTS.
-- Re-running this migration is a no-op.
--
-- Note: we do NOT add a UNIQUE constraint on public_ip — two
-- nodes on the same host (e.g. a Lima 2-node box) share an
-- egress IP. The (compute_nodes.name, public_ip_set_at) pair is
-- the audit trail.
-- +goose StatementBegin
ALTER TABLE compute_nodes
    ADD COLUMN IF NOT EXISTS public_ip inet;
ALTER TABLE compute_nodes
    ADD COLUMN IF NOT EXISTS public_ip_set_at timestamptz;
-- +goose StatementEnd

-- +goose Down
-- Reverse: drop the columns. The gate falls back to
-- `OutcomeLeaderUnreachable` on downgrade (the leader URL is
-- empty), and operators must re-issue the
-- `gregale compute-nodes set-public-ip` upsert after re-upgrade.
-- +goose StatementBegin
ALTER TABLE compute_nodes
    DROP COLUMN IF EXISTS public_ip;
ALTER TABLE compute_nodes
    DROP COLUMN IF EXISTS public_ip_set_at;
-- +goose StatementEnd
