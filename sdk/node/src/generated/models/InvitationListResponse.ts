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
};

