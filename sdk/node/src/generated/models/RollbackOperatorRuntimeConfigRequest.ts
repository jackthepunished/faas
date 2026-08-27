/* generated using openapi-typescript-codegen -- do not edit */
/* istanbul ignore file */
/* tslint:disable */
/* eslint-disable */
/**
 * Request to apply a historical runtime configuration revision.
 */
export type RollbackOperatorRuntimeConfigRequest = {
  version: number;
  reason: string;
  expected_version?: number;
};

