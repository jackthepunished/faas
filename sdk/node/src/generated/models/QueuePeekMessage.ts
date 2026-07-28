/* generated using openapi-typescript-codegen -- do not edit */
/* istanbul ignore file */
/* tslint:disable */
/* eslint-disable */
/**
 * One pending row. No lease was acquired and `attempts` was not incremented.
 */
export type QueuePeekMessage = {
  id: string;
  created_at: string;
  attempts: number;
  /**
   * Stored payload rendered as a JSON string (verbatim from the jsonb column).
   */
  payload: string;
  /**
   * Most recent failure reason, when attempts > 0.
   */
  last_error?: string;
};

