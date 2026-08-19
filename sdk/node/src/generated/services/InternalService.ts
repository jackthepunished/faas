/* generated using openapi-typescript-codegen -- do not edit */
/* istanbul ignore file */
/* tslint:disable */
/* eslint-disable */
import type { TriggerKind } from '../models/TriggerKind.js';
import type { CancelablePromise } from '../core/CancelablePromise.js';
import { OpenAPI } from '../core/OpenAPI.js';
import { request as __request } from '../core/request.js';
export class InternalService {
  /**
   * Internal — schedd posts a batch envelope to the gateway.
   * Internal-only route. Schedd invokes this once per closed
   * batch (size / window / 6MB cap). The function under the
   * trigger responds with `{"batchItemFailures":[{"itemIdentifier":"..."}]}`.
   * Empty / missing response ⇒ full success. Mirrors AWS Lambda's
   * `ReportBatchItemFailures` contract verbatim.
   *
   * @returns any Batch accepted; per-record status derived from response.
   * @throws ApiError
   */
  public static dispatchInvocationBatch({
    requestBody,
  }: {
    requestBody: {
      trigger_id: string;
      app_id?: string;
      kind?: TriggerKind;
      records: Array<{
        item_identifier: string;
        payload_b64: string;
        headers?: Record<string, string>;
        metadata?: Record<string, any>;
      }>;
    },
  }): CancelablePromise<{
    succeeded?: Array<string>;
    retry?: Array<string>;
    dead_letter?: Array<string>;
  }> {
    return __request(OpenAPI, {
      method: 'POST',
      url: '/v1/invocations:dispatch_batch',
      body: requestBody,
      mediaType: 'application/json',
    });
  }
}
