/* generated using openapi-typescript-codegen -- do not edit */
/* istanbul ignore file */
/* tslint:disable */
/* eslint-disable */
/**
 * Wire shape for GET /v1/admin/operator-intents/{id}
 * (admin scope + FAAS_ADMIN_EMAILS allowlist; NO MFA —
 * mirrors getFireCronRequest). IDOR closure: 404 (not 403)
 * on wrong-owner so an admin cannot distinguish "wrong id"
 * from "wrong owner".
 *
 */
export type OperatorIntentResponse = {
  intent_id: string;
  kind: 'force_park' | 'force_cold_boot';
  status: 'pending' | 'running' | 'succeeded' | 'failed' | 'cancelled';
  /**
   * Instance UUID (force_park) or deployment UUID (force_cold_boot).
   */
  target_id: string;
  /**
   * Owning account. NULL for fleet-level intents (e.g. P2c reclaim_build).
   */
  account_id?: string;
  requested_at: string;
  /**
   * Set when schedd claims the intent (pending → running).
   */
  started_at?: string;
  /**
   * Set on terminal status (succeeded/failed/cancelled).
   */
  finished_at?: string;
  /**
   * Bounded dispatch error message (1 KB cap) on failed status.
   */
  error?: string;
  /**
   * Populated for force_cold_boot on succeeded status. Empty when no snapshots existed.
   */
  snap_ids_marked_stale?: Array<string>;
};

