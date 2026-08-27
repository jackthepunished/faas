/* generated using openapi-typescript-codegen -- do not edit */
/* istanbul ignore file */
/* tslint:disable */
/* eslint-disable */
import type { DeploymentResponse } from './DeploymentResponse.js';
/**
 * POST /v1/apps/{slug}/rollouts/recover response. The post-recovery Deployment + the audit row id (so the operator's terminal can echo audit_id=…).
 */
export type RolloutTransitionResponse = {
  deployment: DeploymentResponse;
  /**
   * The deployment_audit row id (stringified so JSON-number → int64 → BigInt drift doesn't poison SDK callers).
   */
  audit_id: string;
};

