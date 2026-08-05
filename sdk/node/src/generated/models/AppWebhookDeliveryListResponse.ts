/* generated using openapi-typescript-codegen -- do not edit */
/* istanbul ignore file */
/* tslint:disable */
/* eslint-disable */
import type { AppWebhookDeliveryResponse } from './AppWebhookDeliveryResponse.js';
export type AppWebhookDeliveryListResponse = {
  deliveries: Array<AppWebhookDeliveryResponse>;
  /**
   * Cursor for the next page; empty/absent when the page is the last.
   */
  next_token?: string;
};

