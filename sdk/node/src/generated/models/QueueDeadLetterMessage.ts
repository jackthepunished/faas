/* generated using openapi-typescript-codegen -- do not edit */
/* istanbul ignore file */
/* tslint:disable */
/* eslint-disable */
/**
 * One row that exhausted the plan's retry budget (state='dead_letter').
 */
export type QueueDeadLetterMessage = {
  id: string;
  created_at: string;
  /**
   * When the drain transitioned the row to dead_letter.
   */
  failed_at: string;
  attempts: number;
  last_error: string;
  payload: string;
};

