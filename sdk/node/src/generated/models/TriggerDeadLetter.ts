/* generated using openapi-typescript-codegen -- do not edit */
/* istanbul ignore file */
/* tslint:disable */
/* eslint-disable */
import type { TriggerDeadLetterReason } from './TriggerDeadLetterReason.js';
import type { TriggerRoutedTo } from './TriggerRoutedTo.js';
/**
 * Read-only wire shape for one trigger_dead_letter row.
 */
export type TriggerDeadLetter = {
  record_id: string;
  trigger_id: string;
  reason: TriggerDeadLetterReason;
  routed_to: TriggerRoutedTo;
  /**
   * Opaque per-reason JSON (broker-error vs poison-record shapes differ).
   */
  detail: Record<string, any>;
  created_at: string;
};

