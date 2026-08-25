/* generated using openapi-typescript-codegen -- do not edit */
/* istanbul ignore file */
/* tslint:disable */
/* eslint-disable */
/**
 * Wire shape for POST /v1/admin/apps/{slug}/force-cold-boot
 * (admin scope + FAAS_ADMIN_EMAILS allowlist). The audit
 * row is emitted under operator.action.force_cold_boot
 * with target_account_id = the app's owning account.
 *
 */
export type ForceColdBootResponse = {
  ok: boolean;
  app_id: string;
  deployment_id: string;
  /**
   * Snap IDs flipped to stale. Empty when the deployment has no snapshots.
   */
  snap_ids_marked_stale: Array<string>;
  reason: string;
  /**
   * Snapshot tiers walked by Engine.ForceColdBootNextWake. Fixed at [warm, init] per ADR-005.
   */
  tier_walked: Array<'warm' | 'init'>;
};

