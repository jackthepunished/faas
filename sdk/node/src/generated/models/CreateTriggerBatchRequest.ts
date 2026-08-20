/* generated using openapi-typescript-codegen -- do not edit */
/* istanbul ignore file */
/* tslint:disable */
/* eslint-disable */
/**
 * Inline-manifest path (POST /v1/triggers:batch_create) — fire a
 * gregale.yaml blob at the server without staging a tarball.
 * The handler re-uses validateManifestBytes from the manifest
 * apply path.
 *
 */
export type CreateTriggerBatchRequest = {
  app_id: string;
  /**
   * Raw gregale.yaml triggers: list.
   */
  manifest_yaml: string;
};

