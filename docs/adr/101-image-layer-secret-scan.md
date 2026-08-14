# ADR-101: Imaged-layer secret scan (post-build OCI secrets gate)

Status: proposed (PR-A of the [secret-scan cluster](./README.md))
Track: Tier A — security / supply-chain
Supersedes: nothing. Stands alone — secret-scan v2 (PR #873) covered the
source-tree upload path; PR-A closes the post-build OCI image path that v2
couldn't reach.
Issue: [#873 follow-up](https://github.com/poyrazK/faas/issues/873) — see
the "next direction" handoff on the v2 PR thread.

## Context

PR #873 (secret-scan v2, merged) added `cmd/apid/secretscan.go::scanExtractedTreeSecrets`,
which walks the customer's source-tree extract spool with
`pkg/secretscan.ScanFile` and rejects the upload at
`/v1/projects[/scan]` with a 422 + `secret_findings` envelope. The v2 path
catches secrets in `.env*` files and arbitrary source-tree bytes — but it
ONLY sees the source-tree tarball. It misses anything that lands in the
assembled OCI image:

- `ENV STRIPE_KEY=sk_live_...` in a Dockerfile — Railpack / BuildKit
  inside the per-deploy builder microVM (ADR-003) bakes it into a layer;
- `--build-arg SECRET=...` to BuildKit — same;
- `COPY .env /app/.env` in a build step — the source-tree `.env` was
  present at upload time but the apid source-tree path doesn't see file
  copies through build steps.

Both the customer-build path (railpack inside the builder microVM) and the
pre-built OCI path (`--image registry...@sha256:...`) land at imaged as
already-baked layers — imaged never sees the Dockerfile or the build args,
it only sees the final merged bytes.

This is the obvious next adversary pivot: a customer who hits the apid 422
just moves the secret into the build step. PR-A closes that gap by walking
the assembled image filesystem with the same `pkg/secretscan` engine on the
imaged side, between `SetDeploymentRootfs` and the `pending → snapshotting`
deployment transition. Mirrors the grype CVE
(`pkg/imaged/handler.go::runDeployScan`) seam structurally so the two
post-build scanners stay aligned.

## Decision

Introduce the **imaged-layer secret scan** as a NEW post-build gate on
the deploy pipeline. The engine is the same `pkg/secretscan` package the
v2 source-tree path uses — same patterns, same providers, same Severity
table, same per-finding Snippet policy. The placement, the posture, and
the audit-row shape differ.

### Posture: loud-fail (not best-effort)

A pattern-level finding in the assembled image is a security boundary
violation, not observability. The grype CVE path is best-effort by design
(ADR-075 AC #4 — CRITICAL-CVE images deploy successfully); the secret path
is intentionally NOT — secrets are not metadata. Mirrors
`errStatefulViolation`'s loud-fail shape (G13 closure).

A finding:

1. Stamps the audit row via `state.Store.UpsertDeploymentSecretFindings`
   (best-effort on the write itself).
2. Transitions the deployment row to `DeployFailed` with
   `error_code = 'image_secret_detected'` via
   `markDeployFailed(... errImageSecretDetected ...)`.

The deployment's `pending → snapshotting` transition does NOT fire
(handleDeployment returns the sentinel which the caller treats as a
terminal failure). The grype CVE scanner still runs on the next retry
attempt the customer makes with the secret removed.

### Placement: post-build, pre-snapshotting

The scan runs AFTER `SetDeploymentRootfs` has stamped the per-app ext4
on disk, and BEFORE the `pending → snapshotting` transition fires. Reuses
the `stageScanExt4` helper the grype path uses (same LocalCacheBackend
short-circuit, same remote-backend staging). The cost is ~1-3 s per
deploy (matches the existing grype path) — already paid in the build
pipeline that's gated by build cold-boot.

Function deploys are out of scope: they have no Dockerfile path (the
runner injects the only customer-tarball contents, scanned at apid
source-tree time). The handler branches on `app.Type == AppTypeFunction`
and skips the layer scan for functions.

### Sidecar coverage

Each sidecar ext4 is scanned individually, after its own
`SetDeploymentSidecarLayer` row lands. The per-walk label is
`"sidecar-<slug>"` so a finding in a sidecar is distinguishable from a
finding in the main image (different blast radius). A finding in any
sidecar fails the deploy — partial sidecar sets are worse than a clean
failure (see the comment block at `buildSidecarLayers`).

### Reuse v2 columns + wire surfaces

PR-A reuses the v2 columns (migration 00264 already added
`deployments.secret_findings` + `secret_scanned_at` + the
`complete_with_redactions` widening on `deployments_scan_status_chk`).
NO new migration needed. The schema:

- `secret_findings jsonb` — typed `[]secretscan.Finding` + `layer`
  label + `image_digest` written via `state.Store.UpsertDeploymentSecretFindings`
- `secret_scanned_at timestamptz` — wall clock, set on every scan
  (clean OR hit) so the dashboard can render "scan complete · 0
  findings" rather than "scan pending" once the deploy lands

Wire surfaces added (all additive, zero breaking change):

- `GET /v1/deployments/{id}/secret-scan` — drill-down route,
  mirrors `/scan` shape (404 on IDOR + scan-pending)
- `DeploymentResponse.SecretScan *SecretScanResult` — mirrors
  `DeploymentResponse.Scan *ScanResult`
- `api.SecretFinding.Layer` — per-finding label (`app` |
  `sidecar-<slug>` | `""` for the legacy apid source-tree path)
- `api.SecretScanResult.ImageDigest` — mirrors
  `api.ScanResult.ImageDigest` so a side-by-side compare renders
- `pkg/api.CodeImageSecretDetected = "image_secret_detected"` —
  wire-stable 422 envelope for the apid-side rejection path (currently
  no caller — the apid path already uses CodeSecretScanStrict from
  v2 — but the constant is here so a future flows-route can use it).
  The deployments.error_code column is free text (migration 00021
  added it without a CHECK), so `image_secret_detected` is a valid
  value without a schema widening.
- `pkg/api.GetDeploymentSecretScan(...)` — SDK method, mirrored in
  Gregale CLI via `gregale deployment <id> [--show-secret-scan]`
  flag (mirrors `--show-scan`).

### No apid-side apid-gap closure

PR-A does NOT close the v2 apid-side "no audit row written on the 422
path" gap. The v2 PR's `UpsertDeploymentSecretFindings` seam has zero
callers today, but the gap is structural (apid scan-service fires the
422 at `cmd/apid/scan_service.go:362` BEFORE `CreateDeployment` at line
212 — there is no deployment row at the rejection point to stamp an
audit row onto). The wire envelope already carries
`api.Problem.SecretFindings` + `WithSecretScan` so the customer sees
the findings in the response.

The imaged-side code in this PR IS the intended first caller of
`UpsertDeploymentSecretFindings`. Closing the apid-side audit-row gap
requires either a new `projects.last_secret_rejection jsonb` column
(out of scope) or a new `secret_scan_audit` table for cross-deploy
history — both deferred to PR-A's successor.

## Sub-decisions

- **D1 — Loud-fail posture** (mirrors `StatefulDenyListMatch` G13
  closure). Reasons above.
- **D2 — Post-build walk** (slot B). Reasons above.
- **D3 — Reuse v2 columns** (migration 00264). Reasons above.
- **D4 — Sidecar coverage.** A secret-baked sidecar is the same risk
  as a secret-baked main image.
- **D5 — Function deploys NOT in scope.** Already scanned at apid
  source-tree time.
- **D6 — No apid-side gap closure in this PR.** Structural — deferred
  to PR-A's successor with new schema.
- **D7 — New sentinel `error_code = 'image_secret_detected'`** on
  the free-text `deployments.error_code` column. No CHECK widening
  needed (column was added without one in migration 00021).
- **D8 — New CLI flag `--show-secret-scan`** (mirrors `--show-scan`).
  Avoids `gregale scan` name collision (taken by Phase 3
  repo-decomposition dry-run surface at
  `cmd/gregale/commands_decompose.go:49`).

## Migration

NONE. The free-text `deployments.error_code` column accepts the new
value without a CHECK widening. v2 widening on
`deployments_scan_status_chk` already covers the `'complete_with_redactions'`
value we stamp on a hit.

## Code map

- `pkg/imaged/secretscan.go` (new) — `runDeployLayerSecretScan`
  + `RunDeployLayerSecretScan` (exported alias for cmd/imaged
  wiring), `withSecretScanFindings` (jsonb marshal), per-file
  walker.
- `pkg/imaged/handler.go` — add `secretScanRun` field
  (mirror `grypeRun`), `WithSecretScanRun` setter (mirror
  `WithGrypeRun`), `runDeployLayerSecretScan` method (mirror
  `runDeployScan`), `errImageSecretDetected` sentinel.
  Slot the call into `handleDeployment` (image-app branch only)
  + `buildSidecarLayers` (per sidecar).
- `cmd/imaged/main.go` — wire `WithSecretScanRun(makeSecretScanRunner())`.
- `cmd/apid/handlers_scan.go` — add `getDeploymentSecretScan` +
  `secretScanResponse` (mirror grype pair).
- `cmd/apid/server.go` — register
  `GET /v1/deployments/{id}/secret-scan`.
- `cmd/apid/handlers_ext.go` — `deploymentResponse` stamps
  `resp.SecretScan = s.secretScanResponse(d)`.
- `pkg/api/errors.go` — `SecretFinding.Layer`,
  `CodeImageSecretDetected`.
- `pkg/api/dto_scan.go` — `SecretScanResult.ImageDigest`.
- `pkg/api/dto.go` — `DeploymentResponse.SecretScan`.
- `pkg/api/client.go` — `GetDeploymentSecretScan`.
- `cmd/gregale/commands_deployments.go` — `--show-secret-scan` flag.
- `cmd/gregale/commands_deployments_test.go` — `TestCmdDeployment_JSON_ShowSecretScanEnvelope`.
- `pkg/api/client_method_sweep2_test.go` — sweep entry for
  `GetDeploymentSecretScan` (sdk-coverage gate).

## Consequences

- The customer contract widens: a future `faas deploy` CLI run that
  hits `image_secret_detected` must inspect
  `GET /v1/deployments/{id}/secret-scan` to see WHERE in the image
  the finding lives.
- Sidecar builds add ~N × layer-scan latency for N sidecars (~200ms
  per sidecar in worst case). Parallelizable with errgroup; budget
  N × 200ms.
- v2's apid-side audit-row seam stays zero-caller until the
  follow-up PR adds `projects.last_secret_rejection jsonb` or a
  new `secret_scan_audit` table.

## Open follow-ups

- Streaming tar-walk of raw OCI layer blobs (vs re-stage via
  stageScanExt4). Would surface secrets in intermediate `RUN`
  layers the merged tree strips. Optimization, not correctness.
- `/v1/audit/secret-scans` listing endpoint with pagination.
- per-customer allowlist (`~/.gregale/scan.toml`).
- Pre-commit hook installer (`gregale init-hooks`).
- JSON / SARIF file output for CI ingestion.
- Close the apid-side audit-row gap (new schema).

## References

- PR #873 — secret-scan v2 (merged). The v2 seam this PR inherits.
- ADR-003 — builder microVMs. Imaged never sees the Dockerfile
  (it sees the merged layers only).
- ADR-075 — supply-chain grype scan placement + loud-fail vs
  best-effort posture. PR-A's `runDeployLayerSecretScan` mirrors
  the seam shape.
- Spec §17 G13 — `StatefulDenyListMatch` loud-fail closure. The
  `errImageSecretDetected` sentinel mirrors its pattern.
- `pkg/secretscan/scan.go` — the engine both paths share.
- `pkg/imaged/handler.go::runDeployScan` — the structural mirror.
- `pkg/api/client_method_sweep2_test.go` — SDK coverage gate.
