/* generated using openapi-typescript-codegen -- do not edit */
/* istanbul ignore file */
/* tslint:disable */
/* eslint-disable */
import type { DeploymentHealthcheck } from './DeploymentHealthcheck.js';
import type { DeploymentLivenessProbe } from './DeploymentLivenessProbe.js';
import type { ScanResult } from './ScanResult.js';
import type { SecretScanResult } from './SecretScanResult.js';
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
   * Liveness-probe override echoed verbatim (issue #554 / ADR-078). nil when the deployment used the per-plan default (Hobby/Pro/Scale → 5s / 3 consecutive / 60s cooldown). Echoed on GET /v1/apps/{slug}/deployments/{id} so the customer can audit which probe the host (cmd/vmmd) is running against the VM.
   */
  override_liveness_probe?: (DeploymentLivenessProbe | null);
  /**
   * Per-deployment cold-wake floor override (issue #557 closure / ADR-072). 0 = inherit from parent app (default); positive value is the deployment's own floor. Effective per-instance floor = max(app.EffectiveMinInstances(), d.EffectiveMinInstances()). Validated against the parent app's plan MaxMinInstances cap on PATCH.
   */
  min_instances?: number;
  /**
   * Per-deploy grype CVE scan surface (issue #464 / ADR-055). nil on pre-feature rows (the migration backfilled scan_status='skipped' + scan_result={reason: 'pre-feature'} on those; the apid read path returns nil so the dashboard / CLI see a clean absence — the /scan route surfaces the 'skipped' sentinel for those rows). Non-nil for post-feature rows in any of the {pending, complete, failed, skipped} states. The customer can deploy a CRITICAL-CVE image; the dashboard shows it; that is the contract (no enforcement at the deploy gate).
   */
  scan?: (ScanResult | null);
  /**
   * Per-deployment parking reason (issue #554 / ADR-079 follow-up, migration 00157). Closed-set vocabulary enforced at the schema layer via the deployments_parked_reason_check constraint. nil for never-parked deployments — surfaced as no field on the wire via omitempty.
   */
  parked_reason?: 'liveness_exhausted' | 'lifecycle_park' | 'admin_park';
  /**
   * Wall-clock timestamp the deployment was parked (set once, idempotent across schedd restart cycles). nil for never-parked deployments.
   */
  parked_at?: string | null;
  /**
   * Per-deployment traffic-split weight (issue #556 PR-A). Summed across live rows for the app = 100 by construction.
   */
  traffic_percent?: number;
  /**
   * Per-deployment env scope (ADR-091 / PR-D). Lowercase alnum + dash, 3..40 chars, no leading/trailing dash. nil/omitted = `default`.
   */
  scope?: string | null;
  /**
   * Per-deploy secret-scan audit row (PR-A / ADR-101). Mirrors
   * the `scan` field shape — absent when the row has not been
   * scanned yet (deploy mid-pipeline or pre-PR-A), present
   * with `findings=[]` for a clean walk, present with
   * one-or-more entries for a hit. Read by the dashboard's
   * "secret scan" card and the CLI's `--show-secret-scan`
   * flag. Stamped both for the imaged-side layer walk (main
   * image + each sidecar; post-build, loud-fail on any
   * finding) and — forward-compat — for the apid-side
   * source-tree 422 path. Status closed-set the writer
   * stamps: "complete" (clean) | "complete_with_redactions"
   * (hit). The `image_digest` sub-field records which OCI
   * digest the imaged walk ran against; null on legacy
   * pre-PR-A rows. See `pkg/imaged/secretscan.go`.
   *
   */
  secret_scan?: (SecretScanResult | null);
};

