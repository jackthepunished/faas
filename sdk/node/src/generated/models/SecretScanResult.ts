/* generated using openapi-typescript-codegen -- do not edit */
/* istanbul ignore file */
/* tslint:disable */
/* eslint-disable */
import type { SecretFinding } from './SecretFinding.js';
/**
 * Customer-facing wire shape of one deployment's secret-scan
 * audit row (migrations/00221, secret-scan v2). Mirrors the
 * `ScanResult` shape but for the server-side secret pipeline
 * (cmd/apid/secretscan.go) — the two never overlap because
 * they stamp different columns on the `deployments` row. The
 * `status` enum mirrors the `deployments.scan_status` column
 * for the secret scan: only `pending` (not scanned yet) and
 * `complete_with_redactions` (scan found secrets) are stamped
 * by the secret-scan pipeline; the grype pipeline writes the
 * other values (`complete` / `failed` / `skipped`).
 *
 */
export type SecretScanResult = {
  status: string;
  /**
   * RFC 3339 UTC. Empty when the deployment hasn't been
   * secret-scanned yet (Status = "pending" or the row
   * pre-dates migration 00221).
   *
   */
  scanned_at?: string;
  findings: Array<SecretFinding>;
};

