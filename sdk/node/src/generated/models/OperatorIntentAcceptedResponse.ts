/* generated using openapi-typescript-codegen -- do not edit */
/* istanbul ignore file */
/* tslint:disable */
/* eslint-disable */
/**
 * Wire shape for the 202 Accepted response of POST
 * /v1/admin/instances/{id}/force-park, POST
 * /v1/admin/instances/{id}/force-restart, and POST
 * /v1/admin/apps/{slug}/force-cold-boot (admin scope +
 * FAAS_ADMIN_EMAILS allowlist). The audit row is emitted
 * under operator.action.{park_instance, restart_instance,
 * force_cold_boot} with target_account_id = the instance's /
 * app's owning account. StatusURL is the relative path;
 * clients prepend the apid base URL.
 *
 */
export type OperatorIntentAcceptedResponse = {
  ok: boolean;
  /**
   * Operator intent UUID. Used to poll status_url.
   */
  intent_id: string;
  /**
   * Relative path to GET /v1/admin/operator-intents/{intent_id}.
   */
  status_url: string;
  /**
   * Recommended horizon to stop polling (UTC, RFC 3339).
   */
  expires_at: string;
  kind: 'force_park' | 'force_cold_boot' | 'force_restart';
  /**
   * Populated for force_park and force_restart. The instance the operator targeted.
   */
  instance_id?: string;
  /**
   * Populated for force_park and force_restart. Gate-time read of `instances.state`.
   */
  previous_state?: 'RUNNING' | 'WAKING' | 'COLD_BOOTING';
  /**
   * Populated for force_cold_boot. The app whose deployment was targeted.
   */
  app_id?: string;
  /**
   * Populated for force_cold_boot. The latest deployment of the app.
   */
  deployment_id?: string;
  reason: string;
};

