/* generated using openapi-typescript-codegen -- do not edit */
/* istanbul ignore file */
/* tslint:disable */
/* eslint-disable */
/**
 * Body for POST /v1/apps/{slug}/rollouts/recover (SAFE-RELEASES-R, issue #976 / ADR-122). Closed-set `action` ∈ {advance, promote, abort}; `reason` is the operator-supplied free-text captured into the deployment_audit row's data payload.
 */
export type RecoverRolloutRequest = {
  /**
   * The recovery action. `advance` requires the rollout to be stuck (>30 min in the same canary step); `promote` short-circuits the rollout to 100% / complete; `abort` flips rollout_state='aborted'.
   */
  action: 'advance' | 'promote' | 'abort';
  /**
   * Operator-supplied reason (≤1024 chars). Lands verbatim in the deployment_audit row's data payload under the `reason` key.
   */
  reason?: string;
};

