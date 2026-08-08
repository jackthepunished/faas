/* generated using openapi-typescript-codegen -- do not edit */
/* istanbul ignore file */
/* tslint:disable */
/* eslint-disable */
/**
 * Body for PATCH /v1/deployments/{id}/traffic (issue #556 PR-A). Sets the per-deployment traffic-split weight (integer [0, 100]). PR-A uses the zero-siblings rebalance form: setting row R's traffic_percent to N forces every other live row in the same app to 0, keeping Σ = 100 by construction. Pro/Scale only — Free/Hobby are rejected at 403 plan_traffic_split_not_allowed.
 */
export type UpdateDeploymentTrafficRequest = {
  /**
   * Per-deployment traffic-split weight. 0 = no traffic (used during rollback). 100 = sole live deployment.
   */
  traffic_percent: number;
};

