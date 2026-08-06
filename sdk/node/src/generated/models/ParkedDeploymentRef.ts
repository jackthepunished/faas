/* generated using openapi-typescript-codegen -- do not edit */
/* istanbul ignore file */
/* tslint:disable */
/* eslint-disable */
/**
 * Reference to a deployment that was parked (issue #554 / ADR-079 follow-up). Returned in AppResponse.parked_deployment when the app has at least one parked deployment. The `parked_reason` field is closed-set (liveness_exhausted | lifecycle_park | admin_park) — enforced at the schema layer via the deployments_parked_reason_check constraint from migration 00157.
 */
export type ParkedDeploymentRef = {
  id: string;
  /**
   * Closed-set parking reason from the schema-layer CHECK constraint.
   */
  parked_reason: 'liveness_exhausted' | 'lifecycle_park' | 'admin_park';
  /**
   * Wall-clock timestamp the deployment was parked (set once, idempotent across schedd restart cycles).
   */
  parked_at: string;
};

