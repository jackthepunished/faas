/* generated using openapi-typescript-codegen -- do not edit */
/* istanbul ignore file */
/* tslint:disable */
/* eslint-disable */
import type { TriggerKind } from './TriggerKind.js';
/**
 * Bulk-create response — per-row trigger ids and any error codes.
 */
export type CreateTriggerBatchResponse = {
  created: Array<{
    slug: string;
    kind: TriggerKind;
    id?: string | null;
    /**
     * RFC 7807 code.
     */
    error?: string | null;
  }>;
};

