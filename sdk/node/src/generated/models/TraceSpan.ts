/* generated using openapi-typescript-codegen -- do not edit */
/* istanbul ignore file */
/* tslint:disable */
/* eslint-disable */
/**
 * One span in a trace. Mirrors the OTel SDK's ReadOnlySpan
 * attributes (kind, name, status, parent linkage, timing) so
 * the customer's debug session can reconstruct the call tree.
 *
 */
export type TraceSpan = {
  trace_id: string;
  span_id: string;
  parent_span_id?: string;
  name: string;
  start_time?: string;
  end_time?: string;
  status?: 'ok' | 'error';
  status_message?: string;
  attributes?: Record<string, string>;
};

