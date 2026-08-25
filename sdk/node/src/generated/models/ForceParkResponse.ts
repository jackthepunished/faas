/* generated using openapi-typescript-codegen -- do not edit */
/* istanbul ignore file */
/* tslint:disable */
/* eslint-disable */
/**
 * Wire shape for POST /v1/admin/instances/{id}/force-park
 * (admin scope + FAAS_ADMIN_EMAILS allowlist). The audit
 * row is emitted under operator.action.park_instance with
 * target_account_id = the instance's owning account.
 *
 */
export type ForceParkResponse = {
  ok: boolean;
  instance_id: string;
  /**
   * Gate-time read of `instances.state`. Reflects the read at handler entry, NOT the post-call state.
   */
  previous_state: 'RUNNING' | 'WAKING' | 'COLD_BOOTING';
  reason: string;
};

