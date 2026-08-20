/* generated using openapi-typescript-codegen -- do not edit */
/* istanbul ignore file */
/* tslint:disable */
/* eslint-disable */
import type { KafkaSASLMechanism } from './KafkaSASLMechanism.js';
/**
 * Kafka SASL credentials. Required Username + Password for
 * every supported mechanism. xdg-go/scram library derives
 * SCRAM client keypairs from Username + Password at dial
 * time.
 *
 */
export type KafkaSASLConfig = {
  mechanism: KafkaSASLMechanism;
  username: string;
  password: string;
};

