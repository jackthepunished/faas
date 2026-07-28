/* generated using openapi-typescript-codegen -- do not edit */
/* istanbul ignore file */
/* tslint:disable */
/* eslint-disable */
/**
 * One FIFO drain row inside ConsumeInvoiceResponse.per_credit.
 * delta_cents is the negative-cents decrement applied to the
 * credit; new_balance is cents_remaining after the call.
 *
 */
export type ConsumedCreditRow = {
  credit_id: string;
  /**
   * Negative integer cents applied to the credit.
   */
  delta_cents: number;
  new_balance: number;
};

