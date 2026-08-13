-- +goose Up
-- +goose StatementBegin

-- 00233_deployments_secret_findings.sql — secret-scan v2 audit row.
--
-- Closes v1 gap B: when the server-side scan (cmd/apid/secretscan.go)
-- finds secret-shaped bytes in a deployment, the findings list and
-- the scan timestamp land in the row so the dashboard, the
-- /v1/deployments/{id}/secret-scan endpoint, and post-mortem
-- forensics can all read them back without re-walking the tarball
-- (the tarball is gone by the time the audit row is consulted).
--
-- Columns:
--   secret_findings  jsonb NOT NULL DEFAULT '[]'::jsonb
--                   — the full []secretscan.Finding list, serialized
--                     exactly as it was on the wire when the 422 fired.
--                     Per-finding Snippet is the pre-truncated safe
--                     representation (first 6 + "…" + last 4) — never
--                     the raw value. The closed-list provider keys
--                     are stable across versions.
--   secret_scanned_at timestamptz NULL
--                   — set by pkg/state.UpsertDeploymentSecretFindings
--                     at the same time as the findings list. NULL
--                     on rows that pre-date this migration (the
--                     '[]'::jsonb default covers that case so the
--                     deployment row stays valid). Distinct from
--                     scanned_at (grype CVE scan) so the two
--                     pipelines don't clobber each other.
--
-- The deployments_scan_status_chk CHECK constraint is widened to
-- accept a new 'complete_with_redactions' value. Distinct from
-- 'complete' (scan completed and emitted no redactions) so a
-- dashboard pill can show "X secrets redacted" footer. Replay-safe:
-- the canonical DROP+ADD pair mirrors 00214's
-- edge_rules_kind_validate.sql pattern.
--
-- Replay-safety: every ALTER uses IF NOT EXISTS / IF EXISTS guards
-- so a second MigrateUp (e.g. after a partial-apply where the goose
-- row was lost) is idempotent. Same convention as 00213
-- (deployments_scope) and 00203 (app_envs_scope_shape).
--
-- Slot reservation: 00233 was chosen as the next free real slot
-- past PR #845 (00229_edge_rules_kind_geo), PR #867
-- (00231_edge_rules_kind_maintenance, 00232_apps_maintenance_mode),
-- and PR #864 (00232_edge_rules_kind_budget). No prior reservation
-- existed at this slot; the migration lands fresh.

ALTER TABLE deployments
    ADD COLUMN IF NOT EXISTS secret_findings  jsonb        NOT NULL DEFAULT '[]'::jsonb,
    ADD COLUMN IF NOT EXISTS secret_scanned_at timestamptz NULL;

-- Widen deployments_scan_status_chk to accept
-- 'complete_with_redactions'. The 'complete' value still means
-- "scan ran and emitted no redactions" — a server-side secret-scan
-- hit forces the new value so the deployment row's scan_status
-- reflects what the customer actually uploaded.
--
-- Canonical DROP+ADD pair (mirrors 00214):
ALTER TABLE deployments DROP CONSTRAINT IF EXISTS deployments_scan_status_chk;
ALTER TABLE deployments ADD  CONSTRAINT deployments_scan_status_chk
    CHECK (scan_status IS NULL OR scan_status = ANY (
        ARRAY['pending'::text, 'complete'::text,
              'failed'::text, 'skipped'::text,
              'complete_with_redactions'::text]
    ));

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
-- Mirror the Up-shape in reverse. Loud-fail if any rows are stamped
-- 'complete_with_redactions' at Down time — the narrower CHECK
-- restored below will reject them with 23514, which is the same
-- loud-fail posture the rest of the migration tree uses (00018).
ALTER TABLE deployments DROP CONSTRAINT IF EXISTS deployments_scan_status_chk;
ALTER TABLE deployments ADD  CONSTRAINT deployments_scan_status_chk
    CHECK (scan_status IS NULL OR scan_status = ANY (
        ARRAY['pending'::text, 'complete'::text,
              'failed'::text, 'skipped'::text]
    ));

ALTER TABLE deployments
    DROP COLUMN IF EXISTS secret_scanned_at,
    DROP COLUMN IF EXISTS secret_findings;

-- +goose StatementEnd
