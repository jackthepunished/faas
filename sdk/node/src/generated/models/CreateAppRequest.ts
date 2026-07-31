/* generated using openapi-typescript-codegen -- do not edit */
/* istanbul ignore file */
/* tslint:disable */
/* eslint-disable */
/**
 * App creation payload: slug, type (app|function), runtime (only for function), RAM MB, max concurrency, idle timeout, and optional manifest.
 */
export type CreateAppRequest = {
  slug: string;
  type?: 'app' | 'function';
  runtime?: 'node22' | 'python312' | 'go124' | 'go124-alpine' | 'node24' | 'python313';
  ram_mb?: number;
  max_concurrency?: number;
  idle_timeout_s?: number;
};

