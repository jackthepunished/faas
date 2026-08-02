/* generated using openapi-typescript-codegen -- do not edit */
/* istanbul ignore file */
/* tslint:disable */
/* eslint-disable */
import type { WakeTimelineEvent } from './WakeTimelineEvent.js';
/**
 * Envelope for `GET /v1/apps/{slug}/wakes/{wake_id}/timeline`.
 */
export type WakeTimelineResponse = {
  /**
   * Echo of the path-segment wake_id.
   */
  wake_id: string;
  /**
   * Resolved app id (the slug's owning app).
   */
  app_id: string;
  events: Array<WakeTimelineEvent>;
  /**
   * Opaque RFC 3339 cursor for the next page. Empty when this is the last page.
   */
  next_cursor?: string;
  /**
   * Effective limit applied (always 1..1000).
   */
  limit: number;
};

