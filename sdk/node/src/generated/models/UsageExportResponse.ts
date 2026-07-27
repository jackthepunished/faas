/* generated using openapi-typescript-codegen -- do not edit */
/* istanbul ignore file */
/* tslint:disable */
/* eslint-disable */
/**
 * One usage record: app id, GB-hours consumed, started/finished timestamps for the export window. cpu_usec is the per-(app, month) cumulative host cgroup CPU-µs (issue #279 / PR-B). Informational only — billing is on mb_seconds.
 */
export type UsageExportResponse = {
  app_id: string;
  month: string;
  mb_seconds: number;
  requests: number;
  /**
   * Cumulative host cgroup CPU-µs consumed by the app in the export window (informational; not billed). issue #279 / PR-B.
   */
  cpu_usec?: number;
};

