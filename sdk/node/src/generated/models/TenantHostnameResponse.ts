/* generated using openapi-typescript-codegen -- do not edit */
/* istanbul ignore file */
/* tslint:disable */
/* eslint-disable */
/**
 * A hostname attached to a tenant surface (DNS-01 verified).
 */
export type TenantHostnameResponse = {
  hostname: string;
  challenge_token?: string | null;
  verified: boolean;
  verified_at?: string | null;
  last_error?: string | null;
  /**
   * TXT record the customer must publish (_faas-verify.<hostname>).
   */
  txt_record: string;
};

