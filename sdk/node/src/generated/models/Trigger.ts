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
  /**
   * Per-record broker payload byte cap (migration 00274).
   * Records above this size are DLQ'd at insert time with
   * reason='payload_too_large' rather than silently truncated.
   * Plan-level ceiling in /v1/limits TriggerPayloadMaxBytes.
   * Default 6291456 (6 MiB) when omitted on create.
   *
   */
  payload_max_bytes: number;
  /**
   * Kafka-only poison-record handling strategy (migration 00275,
   * audit #10). "commit" (default) advances the broker offset
   * via CommitMessages when the dispatcher dead-letters a
   * record — the broker offset and the DB dead-letter state
   * are permanently out of sync for that offset; operator
   * retry works via the dashboard's "re-drive from DLQ"
   * action which mints a fresh trigger_records row from the
   * same item_id. "seek-to-offset" calls SetOffset(msg.Offset)
   * instead so the next Poll re-fetches the same message —
   * operator retry combines a trigger re-enable with a
   * dashboard "reset offset" action that re-fetches the
   * dead-lettered payload. No effect on non-kafka kinds.
   *
   */
  broker_poison_strategy: 'commit' | 'seek-to-offset';
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

