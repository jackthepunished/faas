/* generated using openapi-typescript-codegen -- do not edit */
/* istanbul ignore file */
/* tslint:disable */
/* eslint-disable */
/**
 * Wire shape for POST /v1/admin/builds/sweep-stuck
 * (admin scope + FAAS_ADMIN_EMAILS allowlist). The audit
 * row is emitted under operator.action.reclaim_build with
 * account_id=NULL (fleet-level sweep, not tenant-scoped).
 *
 */
export type SweepStuckBuildsResponse = {
  ok: boolean;
  /**
   * Rows flipped from 'running' to 'failed' with failure_class='timeout'. 0 when none match.
   */
  swept_count: number;
  /**
   * Effective threshold after parsing ?older_than=. Clamped to [60, 3600].
   */
  older_than_seconds: number;
  /**
   * RFC 3339 cutoff timestamp (now - older_than).
   */
  threshold_iso: string;
};

