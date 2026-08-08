/* generated using openapi-typescript-codegen -- do not edit */
/* istanbul ignore file */
/* tslint:disable */
/* eslint-disable */
/**
 * One row in the Paddle price + product catalog (PR-P3).
 * Plan values are the billable api.Plan constants
 * ("hobby", "pro", "scale") — PlanFree is intentionally
 * absent because it carries no recurring line item.
 * Handle is the Paddle-side id (pri_… for monthly / overage,
 * pro_… for product). SyncedAt is RFC 3339 UTC from the
 * catalog's lastSyncAt.
 *
 */
export type BillingCatalogEntry = {
  plan: 'hobby' | 'pro' | 'scale';
  kind: 'monthly' | 'overage' | 'product';
  /**
   * Paddle price (pri_…) or product (pro_…) ID.
   */
  handle: string;
  synced_at: string;
};

