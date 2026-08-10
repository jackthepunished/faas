/* generated using openapi-typescript-codegen -- do not edit */
/* istanbul ignore file */
/* tslint:disable */
/* eslint-disable */
import type { PaymentMethodSummary } from './PaymentMethodSummary.js';
/**
 * Operator-configured billing portal URL (issue #253) plus the
 * card-on-file summary (issue #242). The url field is omitted
 * when the box has FAAS_BILLING_PORTAL_URL unset — that is the
 * "absent" sentinel; the CLI branches on it to print a friendly
 * hint instead of opening the browser to "". The payment_method
 * field is omitted when the account has no card on file (Free
 * plan, or post-checkout before the first paid cycle settles).
 *
 */
export type BillingPortalResponse = {
  /**
   * Substituted portal URL; absent when the box has no portal configured.
   */
  url?: string | null;
  /**
   * Card-on-file summary. Omitted when the account has no
   * payment method on file (Free / never-checked-out). The
   * CLI's `faas billing payment-method` and the dashboard's
   * billing page render from this field.
   *
   */
  payment_method?: PaymentMethodSummary;
};

