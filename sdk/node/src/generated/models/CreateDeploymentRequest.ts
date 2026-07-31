/* generated using openapi-typescript-codegen -- do not edit */
/* istanbul ignore file */
/* tslint:disable */
/* eslint-disable */
import type { CreateDeploymentOverrides } from './CreateDeploymentOverrides.js';
/**
 * Two content-types accepted (see operation description): prebuilt OCI image reference, or multipart source upload. The optional `overrides` object (issue #460 / ADR-053) lets a customer redeploy the same digest-pinned image with a different entrypoint / cmd / env / env_secrets / port / healthcheck without rebuilding the image. The override field list is FROZEN — six fields, no more — and any extra field on the override object 400s the request (the handler's decoder rejects unknown keys; see ADR-053 §Decision 1).
 */
export type CreateDeploymentRequest = {
  /**
   * registry.gregale.dev/...@sha256:... — digest-pinned OCI reference.
   */
  image?: string;
  /**
   * Deploy-time overrides (entrypoint, cmd, env, env_secrets, port, healthcheck). nil/omitted = deploy the image as-is.
   */
  overrides?: (CreateDeploymentOverrides | null);
};

