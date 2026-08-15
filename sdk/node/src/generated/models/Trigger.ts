/* generated using openapi-typescript-codegen -- do not edit */
/* istanbul ignore file */
/* tslint:disable */
/* eslint-disable */
import type { TriggerKind } from './TriggerKind.js';
/**
 * Read shape returned by GET / POST / PATCH on /v1/triggers.
 * The `config` blob is opaque at the wire level — each kind
 * decodes its own per-shape struct lazily. The SDK round-trip
 * preserves the raw JSON so unknown fields survive client
 * versions older than the server.
 *
 */
export type Trigger = {
  id: string;
  account_id: string;
  app_id: string;
  kind: TriggerKind;
  /**
   * Unique-per-app handle. Required for non-cron kinds; ignored on cron.
   */
  slug?: string;
  enabled: boolean;
  /**
   * Per-kind opaque configuration. Decode with the per-kind
   * struct (KafkaTriggerConfig, NATSTriggerConfig, etc).
   *
   */
  config: Record<string, any>;
  /**
   * Records per batch upper bound (per-plan cap in /v1/limits).
   */
  batch_size_max: number;
  /**
   * Milliseconds a partial batch may wait before dispatch.
   */
  batch_window_ms: number;
  max_attempts: number;
  schedule?: string | null;
  path?: string | null;
  cron_id?: string | null;
  /**
   * Source discriminator for kind=queue rows.
   */
  source?: 'queue' | 'delayed_task';
  created_at: string;
  updated_at: string;
};

