/* generated using openapi-typescript-codegen -- do not edit */
/* istanbul ignore file */
/* tslint:disable */
/* eslint-disable */
/**
 * Body for POST /v1/apps/{slug}/rollback (SAFE-RELEASES-G, issue #976). All fields optional. Without a body the handler falls back to rolling back to the most-recent superseded deployment (pre-#976 behaviour). With `target_deployment_id` set, the handler validates that the named deployment belongs to this app AND has status='superseded'.
 */
export type RollbackRequest = {
  /**
   * The UUID of the deployment to promote back to 'live'. Must belong to the same app as the URL slug, and must have status='superseded'. Nil/empty falls back to the most-recent superseded deployment (legacy behaviour).
   */
  target_deployment_id?: string;
};

