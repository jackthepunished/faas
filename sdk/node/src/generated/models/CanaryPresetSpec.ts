/* generated using openapi-typescript-codegen -- do not edit */
/* istanbul ignore file */
/* tslint:disable */
/* eslint-disable */
/**
 * Progressive canary rollout preset requested on deployment creation (issue #976 / ADR-122).
 */
export type CanaryPresetSpec = {
  /**
   * Closed-set canary ladder catalog name.
   */
  preset: 'none' | 'slow' | 'balanced' | 'aggressive' | '1-10-50-100';
  /**
   * Optional step durations encoded as Go time.Duration nanoseconds; reserved for custom-ladder support.
   */
  step_durations?: Array<number>;
};

