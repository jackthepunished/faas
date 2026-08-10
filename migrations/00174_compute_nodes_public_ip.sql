-- +goose Up
-- Tier A9 / ADR-089 §Decision #2 (lex-min leader identity) needs
-- a stable public address per node — the standby's 307-redirect
-- Location: header is built from this column. Pre-Tier-A9 the
-- leader URL was host-local; the cross-box mTLS hop from
-- PR-B (MTLSLeaderClient) cannot construct a URL from a
-- host-local-only address.
--
-- Additive: nullable columns so existing rows (pre-Tier-A9
-- deployments) keep validating. Backfill is operator-driven
-- via `gregale compute-nodes set-public-ip <name> <ip>`; no
-- automatic inference from private IPs. The §14 M9 SLA pinned
-- by the try/c-redirect runbook does NOT depend on the column
-- being non-null on every node — only the standby that
-- receives a cookie write needs it on the leader's row.
ALTER TABLE compute_nodes
  ADD COLUMN public_ip INET,
  ADD COLUMN public_ip_set_at TIMESTAMPTZ;

-- CHECK: an INET column accepts NULL naturally. We pin
-- non-NULL rows to a non-empty family — a '0.0.0.0' or
-- '::' would silently produce a malformed Location header.
ALTER TABLE compute_nodes
  ADD CONSTRAINT compute_nodes_public_ip_format_chk
  CHECK (public_ip IS NULL OR family(public_ip) IN (4, 6));

-- +goose Down
ALTER TABLE compute_nodes
  DROP CONSTRAINT IF EXISTS compute_nodes_public_ip_format_chk;
ALTER TABLE compute_nodes
  DROP COLUMN IF EXISTS public_ip_set_at,
  DROP COLUMN IF EXISTS public_ip;
