/* generated using openapi-typescript-codegen -- do not edit */
/* istanbul ignore file */
/* tslint:disable */
/* eslint-disable */
import type { TenantHostnameResponse } from './TenantHostnameResponse.js';
/**
 * A tenant surface: a multi-hostname SAN bundle attached to one app.
 */
export type TenantSurfaceResponse = {
  id: string;
  account_id: string;
  app_id: string;
  name: string;
  cert_kind: 'per_host_san';
  status: 'pending' | 'active' | 'suspended' | 'deleted';
  cert_state: 'none' | 'pending' | 'issued' | 'renewing' | 'failed';
  cert_not_after?: string;
  cert_last_error?: string | null;
  created_at?: string;
  updated_at?: string;
  hostnames: Array<TenantHostnameResponse>;
};

