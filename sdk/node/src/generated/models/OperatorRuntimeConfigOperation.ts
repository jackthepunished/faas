/* generated using openapi-typescript-codegen -- do not edit */
/* istanbul ignore file */
/* tslint:disable */
/* eslint-disable */
/**
 * Durable asynchronous runtime-configuration apply request.
 */
export type OperatorRuntimeConfigOperation = {
  id: string;
  key: string;
  scope: 'global' | 'control_plane' | 'daemon' | 'node';
  scope_id: string;
  version: number;
  desired_value: any;
  effective_value: any;
  apply_mode: 'graceful' | 'rolling' | 'break_glass';
  status: 'pending' | 'running' | 'succeeded' | 'failed' | 'blocked' | 'cancelled';
  phase: string;
  error?: string;
  reason: string;
  target_count: number;
  applied_count: number;
  failed_count: number;
  requested_at: string;
  started_at?: string;
  finished_at?: string;
};

