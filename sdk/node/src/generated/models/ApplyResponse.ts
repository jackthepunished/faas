/* generated using openapi-typescript-codegen -- do not edit */
/* istanbul ignore file */
/* tslint:disable */
/* eslint-disable */
import type { PlanCron } from './PlanCron.js';
import type { PlanManaged } from './PlanManaged.js';
import type { PlanWorkload } from './PlanWorkload.js';
/**
 * Apply response. Carries the inserted project_id and per-app IDs.
 */
export type ApplyResponse = {
  project_slug: string;
  repo_full_name?: string;
  scan_source: string;
  tier?: string;
  workloads?: Array<PlanWorkload>;
  managed?: Array<PlanManaged>;
  crons?: Array<PlanCron>;
  warnings?: Array<string>;
  observed_apps?: number;
  observed_crons?: number;
  limit_apps?: number;
  limit_crons?: number;
  can_apply: boolean;
  crons_not_allowed?: boolean;
  plan_token: string;
  project_id?: string;
  apps?: Array<{
    slug: string;
    id: string;
  }>;
};

