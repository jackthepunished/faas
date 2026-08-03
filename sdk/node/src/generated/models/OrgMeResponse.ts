/* generated using openapi-typescript-codegen -- do not edit */
/* istanbul ignore file */
/* tslint:disable */
/* eslint-disable */
import type { OrgWithRole } from './OrgWithRole.js';
/**
 * GET /v1/orgs/me response. `org` is null when no X-Active-Org /
 * ?org= hint was supplied (the passthrough case every
 * pre-PR-5 route depends on).
 *
 */
export type OrgMeResponse = {
  /**
   * null when no active-org hint was supplied; otherwise the active org + caller's role.
   */
  org: OrgWithRole;
};

