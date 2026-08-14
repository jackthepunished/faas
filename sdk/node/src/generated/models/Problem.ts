/* generated using openapi-typescript-codegen -- do not edit */
/* istanbul ignore file */
/* tslint:disable */
/* eslint-disable */
import type { FieldError } from './FieldError.js';
import type { SecretFinding } from './SecretFinding.js';
/**
 * RFC 7807 problem+json envelope. The `code` field is the stable
 * machine-readable identifier; clients branch on it. `limit` and
 * `observed` are populated on quota errors. `docs_url` points the
 * user at the next action. `billing_portal_url` is populated on
 * `code: payment_required` so the dashboard can deep-link the
 * customer to the Stripe-hosted billing portal (issue #142).
 * `paddle_checkout_url` + `tx_id` are populated instead when the
 * box is running on the Paddle billing provider
 * (`FAAS_BILLING_PROVIDER=paddle`, ADR-025) — the customer lands
 * on a Paddle-hosted checkout page for the target plan and the
 * dashboard renders the transaction handle as a confirmation id.
 * Exactly one of `billing_portal_url` or `paddle_checkout_url` is
 * populated on a given 402 — never both.
 *
 * `errors` carries per-field detail (Cloudflare / Stripe shape)
 * for 422 sites that emit a list of field-level failures — used
 * today by the kind=validate edge rule so a JSON Schema
 * rejection renders as a form-field list the dashboard can
 * iterate without parsing prose. Optional + omitempty so every
 * other problem+json site keeps its existing flat shape unchanged.
 *
 */
export type Problem = {
  type?: string;
  title: string;
  status: number;
  /**
   * Stable machine-readable error code. See StatusForCode in pkg/api/errors.go.
   */
  code: string;
  detail?: string;
  limit?: number | null;
  observed?: number | null;
  docs_url?: string;
  billing_portal_url?: string;
  /**
   * Paddle-hosted checkout URL on a `payment_required` 402 when
   * the box is running on the Paddle billing provider. Mutually
   * exclusive with `billing_portal_url`.
   *
   */
  paddle_checkout_url?: string;
  /**
   * Paddle transaction handle (`txn_…`) on a `payment_required`
   * 402. Empty on the Stripe path. The dashboard renders this as
   * a confirmation id after the customer completes checkout.
   *
   */
  tx_id?: string;
  /**
   * Per-field validation detail. Populated by 422 sites that
   * emit a list of field-level failures. Each entry is a
   * `FieldError` (Cloudflare / Stripe shape: field + expected
   * + got) so an SDK can drive form-field UI without parsing
   * prose.
   *
   */
  errors?: Array<FieldError>;
  /**
   * Per-line secret-scan detail. Populated by 422 sites with
   * `code: secret_scan_strict` (cmd/apid/secretscan.go
   * server-side scan rejection; cmd/gregale printErr
   * --secret-scan=strict client-side rejection). The shape
   * is shared with the on-disk `SecretScanResult` so a
   * programmatic consumer can render the same UI for both
   * rejection paths. Optional + omitempty.
   *
   */
  secret_findings?: Array<SecretFinding>;
  /**
   * Customer-facing remediation nudge attached to a
   * `code: secret_scan_strict` 422 envelope (e.g. "move
   * detected secrets to `gregale secrets set`"). Mirrors
   * the `FieldError` shape's prose pattern so the dashboard
   * / SDK can render the hint as a one-line footer without
   * parsing prose. Optional + omitempty.
   *
   */
  secret_hint?: string;
};

