/* generated using openapi-typescript-codegen -- do not edit */
/* istanbul ignore file */
/* tslint:disable */
/* eslint-disable */
import type { OrgInvitationResponse } from './OrgInvitationResponse.js';
/**
 * GET /v1/orgs/{slug}/invitations response. Sorted by created_at DESC.
 */
export type InvitationListResponse = {
  invitations: Array<OrgInvitationResponse>;
  /**
   * Opaque cursor — set to the `id` of the last row on this
   * page when there's a next page. Pass back as `?before=`
   * to fetch it. Matches the same cursor shape as
   * MemberListResponse / AppListResponse so the SDK can
   * share one walker.
   *
   */
  next_before?: string;
};

