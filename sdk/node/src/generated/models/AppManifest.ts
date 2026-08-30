/* generated using openapi-typescript-codegen -- do not edit */
/* istanbul ignore file */
/* tslint:disable */
/* eslint-disable */
import type { AppManifestHealthcheck } from './AppManifestHealthcheck.js';
/**
 * App manifest: environment variables, build commands, working directory, healthcheck, user, and Dockerfile-as-source flag (§ux 6.3). The optional `env_secrets` field carries sealed-secret refs ("secret:NAME" strings) resolved by the host at wake time against the app_secrets table (issue #460 / ADR-053 §Decision 1). Values are NEVER sealed ciphertext — only refs. M-1 (ADR-136) widens the contract additively with `healthcheck`, `stop_signal`, `stop_grace_period` from the OCI image-config spec; old guest-init ignores unknown fields per JSON semantics, so the widen is wire-compatible.
 */
export type AppManifest = {
  entrypoint: Array<string>;
  env?: Record<string, string>;
  /**
   * Env override via sealed-secret refs. Each value is "secret:NAME"; the host resolver looks up NAME against the app_secrets table at wake.
   */
  env_secrets?: Record<string, string>;
  working_dir?: string | null;
  port?: number | null;
  healthz?: string | null;
  user?: string | null;
  healthcheck?: AppManifestHealthcheck;
  /**
   * OCI STOPSIGNAL (default SIGTERM). Runtime wiring lands in M-2.
   */
  stop_signal?: string | null;
  /**
   * OCI StopGracePeriod as a Go duration string (e.g. "5m"). Capped at MaxAppManifestStopGracePeriod (5m). Currently always zero — populated by M-2.
   */
  stop_grace_period?: string | null;
};

