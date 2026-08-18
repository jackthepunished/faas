/* generated using openapi-typescript-codegen -- do not edit */
/* istanbul ignore file */
/* tslint:disable */
/* eslint-disable */
/**
 * Create a tenant surface with a seed set of hostnames.
 */
export type CreateTenantSurfaceRequest = {
  app_id: string;
  name: string;
  cert_kind?: 'per_host_san';
  hostnames?: Array<string>;
};

