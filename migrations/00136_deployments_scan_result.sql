-- filename: 00136_deployments_scan_result.sql
-- +goose Up
-- +goose StatementBegin

-- Issue #464 / ADR-055 — per-deploy grype CVE scan surface (PR-1
-- data plane). The customer-facing scan result lands on the
-- deployments row so the dashboard, the apid route
-- GET /v1/deployments/{id}/scan, and `gregale deployment --show-scan`
-- can read it. The existing base-ext4 scan sidecar
-- (pkg/imaged/base_stage.go::writeScanSidecar, key
-- wire.ScanKeyForBaseKey) is unchanged — that's the factory scan
-- vmmd's bringUpScanCheck consumes. Issue #464 is the deploy-level
-- scan that runs over the app-layer squashfs after the
-- layers-above-base diff and surfaces the result to the customer.
--
-- Three columns + one partial index:
--
--   1. scan_result jsonb — the full ScanResult payload
--      (scanned_at, scanner_version, image_digest,
--      severity_counts, vulnerabilities[]). NULL on rows that
--      pre-date the feature (handled by the backfill below) and on
--      rows where the scan is still pending. The PR-3 sink writes
--      a non-NULL payload after the grype run completes.
--
--   2. scan_status text — closed enum. The PR-3 sink writes one
--      of:
--        * 'pending'   — sink.Upsert called but grype hasn't returned
--                        yet (PR-3 not yet shipped; this PR only
--                        adds the column, the apid path is read-only
--                        until PR-3 lands)
--        * 'complete'  — scan finished, scan_result carries the
--                        findings, scanned_at is the wall clock
--        * 'failed'    — grype ran but errored after the 1-retry
--                        backoff (see design §Retry policy);
--                        scan_result carries the last error message
--                        in {error: "..."}
--        * 'skipped'   — pre-feature row, or the customer opted
--                        out (the only opt-out path today is
--                        "don't run imaged"; the backfill stamps
--                        this on every existing row at migration
--                        time so the dashboard doesn't show
--                        "scan pending" forever)
--      The CHECK constraint is the closed enum; a typo at the
--      PR-3 sink site is a 23514 and visible in CI.
--
--   3. scanned_at timestamptz — the wall clock the grype run
--      completed (set by the PR-3 sink, not by Postgres default
--      now()). NULL on pending / pre-feature rows. Distinct from
--      deployments.created_at because the deploy ships before
--      the scan lands (AC #1: scan within 5 min of status: live,
--      not at the same instant).
--
-- The partial index covers the dashboard's per-app scan lookup
-- (AC #5 isolation: every read filters on app_id, every row in
-- the index has a finished scan). The dashboard's list view
-- LEFT JOINs the count by app_id and the index keeps that
-- sub-millisecond.
--
-- The backfill is replay-safe (ADR-041): the UPDATE has a
-- `WHERE scan_status IS NULL` predicate so a second MigrateUp is
-- a no-op. The skipped status preserves the customer contract
-- "deployments from before this feature don't claim a scan
-- status that never happened" — the dashboard renders
-- `scan_status='skipped'` as "no scan (pre-feature)" without
-- the "scan pending" chip from showing forever.
--
-- The Down branch drops the columns + the index. forward-only
-- is the convention; the Down branch exists for local-dev
-- replays where a developer rolled back the migration before
-- a fresh `make bootstrap` re-applied it.

ALTER TABLE deployments
    ADD COLUMN IF NOT EXISTS scan_result jsonb,
    ADD COLUMN IF NOT EXISTS scan_status text,
    ADD COLUMN IF NOT EXISTS scanned_at timestamptz;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conname = 'deployments_scan_status_chk'
    ) THEN
        ALTER TABLE deployments
            ADD CONSTRAINT deployments_scan_status_chk
            CHECK (scan_status IS NULL OR scan_status = ANY (
                ARRAY['pending'::text, 'complete'::text,
                      'failed'::text, 'skipped'::text]
            ));
    END IF;
END $$;

-- Per-app partial index. The dashboard's "scan overdue" chip
-- filters on (app_id, scan_status='complete') for the 5-min SLA
-- check; the index covers the hot path. Rows with
-- scan_status='skipped' (pre-feature backfill) and
-- scan_status IS NULL (the 5-min window where the deploy ships
-- ahead of the scan) are excluded.
CREATE INDEX IF NOT EXISTS deployments_app_scan_complete_idx
    ON public.deployments USING btree (app_id, scanned_at DESC)
    WHERE scan_status = 'complete';

-- Replay-safe backfill (ADR-041). Pre-feature rows get the
-- 'skipped' sentinel so the dashboard's list view doesn't show
-- a perpetual "scan pending" chip. The WHERE scan_status IS
-- NULL predicate makes a second MigrateUp a no-op (the rows
-- stamped by the first pass have scan_status='skipped' and
-- are skipped by the predicate on the second pass).
UPDATE public.deployments
   SET scan_status = 'skipped',
       scan_result = jsonb_build_object(
           'reason', 'pre-feature',
           'skipped_at', now()::text
       )
 WHERE scan_status IS NULL;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
-- Forward-only (ADR-055 §Downstream convention). The Down branch
-- exists for local-dev replays where a developer rolled back the
-- migration before a fresh `make bootstrap` re-applied it. The
-- production path never runs this.
DROP INDEX IF EXISTS public.deployments_app_scan_complete_idx;
ALTER TABLE public.deployments DROP CONSTRAINT IF EXISTS deployments_scan_status_chk;
ALTER TABLE public.deployments
    DROP COLUMN IF EXISTS scanned_at,
    DROP COLUMN IF EXISTS scan_status,
    DROP COLUMN IF EXISTS scan_result;
-- +goose StatementEnd
