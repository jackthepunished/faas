/* generated using openapi-typescript-codegen -- do not edit */
/* istanbul ignore file */
/* tslint:disable */
/* eslint-disable */
import type { TriggerKind } from './TriggerKind.js';
/**
 * Trigger create payload. Kind is immutable after create. Per-kind
 * gating mirrors pkg/gregalemanifest.validateKindConfig:
 * - cron: requires schedule + path (slug ignored)
 * - non-cron: requires slug + config
 *
 */
export type CreateTriggerRequest = {
  app_id: string;
  kind: TriggerKind;
  slug?: string;
  enabled?: boolean | null;
  /**
   * Per-kind opaque config blob.
   */
  config?: Record<string, any>;
  batch_size_max?: number | null;
  batch_window_ms?: number | null;
  max_attempts?: number | null;
  schedule?: string | null;
  path?: string | null;
};

