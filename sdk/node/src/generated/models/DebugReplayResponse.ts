/* generated using openapi-typescript-codegen -- do not edit */
/* istanbul ignore file */
/* tslint:disable */
/* eslint-disable */
/**
 * POST response from /v1/apps/{slug}/debug/requests/{req_id}/replay (ADR-127 / PR-B stub).
 */
export type DebugReplayResponse = {
  /**
   * Set when the mirror invocation lands in PR-A2.
   */
  mirror_invocation_id?: string | null;
  status: 'queued' | 'running' | 'completed' | 'failed';
};

