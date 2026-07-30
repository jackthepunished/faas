/* generated using openapi-typescript-codegen -- do not edit */
/* istanbul ignore file */
/* tslint:disable */
/* eslint-disable */
/**
 * Operator-configured Stripe billing portal URL (issue #253).
 * The url field is omitted when the box has FAAS_BILLING_PORTAL_URL
 * unset — that is the "absent" sentinel; the CLI branches on it
 * to print a friendly hint instead of opening the browser to "".
 *
 */
export type BillingPortalResponse = {
  /**
   * Substituted portal URL; absent when the box has no portal configured.
   */
  url?: string | null;
};

