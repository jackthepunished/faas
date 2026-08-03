/* generated using openapi-typescript-codegen -- do not edit */
/* istanbul ignore file */
/* tslint:disable */
/* eslint-disable */
import type { DeploymentHealthcheck } from './DeploymentHealthcheck.js';
/**
 * One deployment: id, app, source ref, build status, commit SHA, and lifecycle timestamps. The optional `has_overrides` and `override_*` fields are the persisted echo of the create-time overrides object (issue #460 / ADR-053); they round-trip via `GET /v1/apps/{slug}/deployments/{id}` so a customer can audit what their last deploy pinned. Env values are NEVER echoed — only the keys (`override_env_keys`); env_secrets refs ARE echoed because the ref shape is non-secret by design.
 */
export type DeploymentResponse = {
  id: string;
  app_id: string;
  build_id?: string | null;
  image_digest: string;
  kind: string;
  status: string;
  error?: string | null;
  error_code?: string | null;
  created_at: string;
  /**
   * True when this deployment carries a non-null override_* column set.
   */
  has_overrides?: boolean;
  /**
   * Entrypoint override echoed verbatim from the create request. nil when no override was supplied.
   */
  override_entrypoint?: Array<string>;
  /**
   * Cmd override echoed verbatim from the create request.
   */
  override_cmd?: Array<string>;
  /**
   * Sorted set of env-var keys set by the env override. VALUES ARE NEVER ECHOED (ADR-053 §Decision 4).
   */
  override_env_keys?: Array<string>;
  /**
   * Sorted set of env-var keys set by the env_secrets override. The parallel refs are echoed in `override_env_secret_refs` because the ref shape is non-secret by design.
   */
  override_env_secret_keys?: Array<string>;
  /**
   * Verbatim `secret:NAME` ref map; the customer needs to see which secret they bound to which env var to debug a misconfigured deploy.
   */
  override_env_secret_refs?: Record<string, string>;
  /**
   * Listen-port override; 0 = absent (fall back to image default).
   */
  override_port?: number;
  /**
   * Readiness-probe override. Persisted verbatim; the actual HTTP probe is a follow-up — today waitReady stays a bare TCP accept.
   */
  override_healthcheck?: (DeploymentHealthcheck | null);
  /**
   * Per-deployment cold-wake floor override (issue #557 closure / ADR-072). 0 = inherit from parent app (default); positive value is the deployment's own floor. Effective per-instance floor = max(app.EffectiveMinInstances(), d.EffectiveMinInstances()). Validated against the parent app's plan MaxMinInstances cap on PATCH.
   */
  min_instances?: number;
};

