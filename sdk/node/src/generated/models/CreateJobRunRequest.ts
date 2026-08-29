/* generated using openapi-typescript-codegen -- do not edit */
/* istanbul ignore file */
/* tslint:disable */
/* eslint-disable */
/**
 * Atomic fan-out via `generate_series` CTE in pgstore; the
 * handler validates `tasks` against `Plan.JobMaxTasksPerRun`
 * (Hobby=100, Pro=1000, Scale=5000). Per-run overrides
 * (parallelism / retry_max / task_timeout_sec) inherit from
 * the job when null.
 *
 */
export type CreateJobRunRequest = {
  tasks: number;
  parallelism?: number;
  retry_max?: number;
  task_timeout_sec?: number;
  env_overrides?: Record<string, string>;
};

