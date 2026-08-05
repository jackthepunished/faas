/* generated using openapi-typescript-codegen -- do not edit */
/* istanbul ignore file */
/* tslint:disable */
/* eslint-disable */
export type AppWebhookDeliveryResponse = {
  id: string;
  webhook_id: string;
  app_id: string;
  account_id: string;
  event: string;
  /**
   * The original event payload (omitted on rows past the first attempt; the customer has already seen it).
   */
  payload?: Record<string, any>;
  attempt: number;
  status: 'pending' | 'in_flight' | 'succeeded' | 'failed' | 'dead';
  last_error?: string;
  last_response_code?: number;
  next_attempt_at: string;
  delivered_at?: string;
  created_at: string;
  updated_at: string;
};

