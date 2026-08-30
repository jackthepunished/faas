/* generated using openapi-typescript-codegen -- do not edit */
/* istanbul ignore file */
/* tslint:disable */
/* eslint-disable */
/**
 * Per-task log tail. Proxied from vmmd's tail endpoint on
 * the compute node that owns the instance. Truncated=true
 * means the tail was capped at MaxBytes; clients re-fetch
 * with a larger limit to see more.
 *
 */
export type JobTaskLogResponse = {
  task_status: 'queued' | 'claimed' | 'succeeded' | 'failed' | 'timeout' | 'cancelled' | 'oom';
  log_content: string;
  truncated: boolean;
  max_bytes: number;
};

