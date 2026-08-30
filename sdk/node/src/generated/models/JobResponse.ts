/* generated using openapi-typescript-codegen -- do not edit */
/* istanbul ignore file */
/* tslint:disable */
/* eslint-disable */
/**
 * Wire projection of state.Job.
 */
export type JobResponse = {
  id: string;
  account_id: string;
  name: string;
  kind: 'batch' | 'recurring';
  image_ref: string;
  command: Array<string>;
  env_overrides?: Record<string, string>;
  ram_mb: number;
  task_timeout_sec: number;
  max_parallelism: number;
  retry_max: number;
  status: 'active' | 'paused' | 'deleted';
  created_at: string;
  updated_at: string;
};

