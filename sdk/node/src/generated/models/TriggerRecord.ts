/* generated using openapi-typescript-codegen -- do not edit */
/* istanbul ignore file */
/* tslint:disable */
/* eslint-disable */
import type { TriggerRecordState } from './TriggerRecordState.js';
/**
 * Audit row for one record passing through a trigger.
 * Surfaced via GET /v1/triggers/{id}/records so customers can
 * answer "did my last N wake-ups succeed?".
 *
 */
export type TriggerRecord = {
  id: string;
  trigger_id: string;
  /**
   * Broker-side identifier (Kafka offset, NATS seq, SQS receipt handle).
   */
  item_identifier: string;
  /**
   * Raw JSON body, decoded lazily by the dashboard.
   */
  payload: string;
  /**
   * Raw JSON of broker headers.
   */
  headers: string;
  /**
   * Raw JSON of broker metadata (delivery count, etc.).
   */
  metadata: string;
  state: TriggerRecordState;
  attempts: number;
  next_fire_at: string;
  received_at: string;
  last_error?: string | null;
  last_dispatched_at?: string | null;
};

