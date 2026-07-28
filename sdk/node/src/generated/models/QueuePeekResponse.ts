/* generated using openapi-typescript-codegen -- do not edit */
/* istanbul ignore file */
/* tslint:disable */
/* eslint-disable */
import type { QueuePeekMessage } from './QueuePeekMessage.js';
/**
 * 200 — a page of pending rows ordered by created_at ASC, id ASC. Pass `next_before` as `?before=<id>` for the next page.
 */
export type QueuePeekResponse = {
  app_slug: string;
  messages: Array<QueuePeekMessage>;
  /**
   * Omitted when the page is the final one.
   */
  next_before?: string;
};

