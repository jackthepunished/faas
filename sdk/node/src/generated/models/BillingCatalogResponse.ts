/* generated using openapi-typescript-codegen -- do not edit */
/* istanbul ignore file */
/* tslint:disable */
/* eslint-disable */
import type { BillingCatalogEntry } from './BillingCatalogEntry.js';
/**
 * Wire shape for GET / POST / DELETE
 * /v1/admin/billing-paddle-catalog (PR-P3). Provider is
 * the active billing provider's name (paddle / stripe);
 * on a Stripe deployment the handler 501s before
 * serializing this struct. SyncedAt is the timestamp of
 * the most recent successful EnsurePlanProducts call;
 * empty string when no hydration has yet completed
 * (POST and DELETE both reset it).
 *
 */
export type BillingCatalogResponse = {
  /**
   * Active provider name (paddle / stripe).
   */
  provider: string;
  /**
   * RFC 3339 last-sync timestamp; empty string when never synced.
   */
  synced_at: string;
  entries: Array<BillingCatalogEntry>;
};

