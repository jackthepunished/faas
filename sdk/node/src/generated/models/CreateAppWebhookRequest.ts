/* generated using openapi-typescript-codegen -- do not edit */
/* istanbul ignore file */
/* tslint:disable */
/* eslint-disable */
export type CreateAppWebhookRequest = {
  target_url: string;
  webhook_secret: string;
  event_filter?: Array<'cron.fired' | 'app.created' | 'app.deleted' | 'build.succeeded' | 'build.failed'>;
  retry_policy?: 'default' | 'aggressive' | 'none';
  enabled?: boolean;
};

