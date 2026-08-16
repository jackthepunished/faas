/* generated using openapi-typescript-codegen -- do not edit */
/* istanbul ignore file */
/* tslint:disable */
/* eslint-disable */
/**
 * Partial trigger update. nil means "leave unchanged" (same
 * semantics as UpdateCronRequest). Kind is NOT a member — it
 * is immutable. To change kind, create a new trigger and
 * delete the old one.
 *
 */
export type UpdateTriggerRequest = {
  enabled?: boolean | null;
  config?: any | null;
  batch_size_max?: number | null;
  batch_window_ms?: number | null;
  max_attempts?: number | null;
  payload_max_bytes?: number | null;
  schedule?: string | null;
  path?: string | null;
};

