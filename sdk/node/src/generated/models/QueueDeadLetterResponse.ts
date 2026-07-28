/* generated using openapi-typescript-codegen -- do not edit */
/* istanbul ignore file */
/* tslint:disable */
/* eslint-disable */
import type { QueueDeadLetterMessage } from './QueueDeadLetterMessage.js';
/**
 * 200 — a page of dead-letter rows ordered by created_at DESC, id DESC.
 */
export type QueueDeadLetterResponse = {
  app_slug: string;
  messages: Array<QueueDeadLetterMessage>;
  /**
   * Cursor for the previous page; absent on the final page.
   */
  next_before?: string;
};

