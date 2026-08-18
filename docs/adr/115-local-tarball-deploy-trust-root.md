# ADR-115 · Local-tarball deploy trust root (issue #961 / Mega-A PR-1)

- **Status:** **Proposed**
- **Date:** 2026-08-18
- **Decision:** Gregale ships a second deploy input path,
  `POST /v1/apps/{slug}/deployments/source-tarball`, accepting a
  multipart body with a CLI-uploaded gzipped tar plus an
  informational `{repo, ref}` JSON sidecar. The CLI binary is the
  trust root on this path; apid does NOT consult `github_installations`
  and does NOT attempt a server-side git fetch. The existing
  `POST /v1/apps/{slug}/deployments/source-ref` handler
  (issue #739 / ADR-092) is unchanged — the install-token + 40-char
  SHA gate stays load-bearing for `--repo X --ref Y` semantics. The
  two paths share only the spool/enqueue helpers (`validateAndSpool`,
  `apidsource.Enqueue`); the trust/verify gates diverge. The audit
  kind is `deploy.local_tarball` (distinct from `deploy.source_ref`)
  so downstream consumers can branch on wire shape without inspecting
  source URLs.

  Wire shape:

  ```
  POST /v1/apps/{slug}/deployments/source-tarball
  Content-Type: multipart/form-data; boundary=…
  Authorization: Bearer <token>
  Idempotency-Key: <uuid>     (auto-minted by the SDK)

  --boundary
  Content-Disposition: form-data; name="tarball"; filename="src.tar.gz"
  Content-Type: application/gzip

  <gzipped tar bytes>

  --boundary
  Content-Disposition: form-data; name="sidecar"
  Content-Type: application/json

  {"repo": "owner/name", "ref": "<40-char SHA>"}
  --boundary--
  ```

  `tarball` is required; `sidecar` is optional (missing → empty
  provenance fields on the build row). The size cap is the
  per-plan `SourceTarballMaxMB` limit (`pkg/api/limits.go:82`),
  enforced by `validateAndSpool` and pinned in the response as
  `413 code=source_too_large` on overflow.

## Why this is needed

The zero-config `gregale deploy` story (issue #961 leaf 1) requires
deploying from a customer's local machine without first installing
the Gregale GitHub App. The existing `handleSourceRefDeploy`
(`cmd/apid/handlers_source_ref.go:62-155`) gates on two preconditions
that a fresh `git clone` does not satisfy:

1. A row in `github_installations` — required by
   `resolveInstallToken` (`handlers_source_ref.go:170-194`); absent
   row → `404 code=github_install_not_found`.
2. A 40-char lowercase SHA — required by `resolveCommitSHA`
   (`handlers_source_ref.go:305-314`); branches / tags / short
   SHAs → `400 code=invalid_ref`.

Forcing the customer to install the Gregale GitHub App before their
first deploy is exactly the friction this mega-PR exists to remove.

## Why not relax the existing gate

The install-token + 40-char-SHA pair is the supply-chain control
that prevents a malicious or revoked `github_installations` row from
swapping the upstream tarball at deploy time. Loosening the gate on
the existing handler weakens a load-bearing security boundary
silently — exactly the failure mode this ADR cluster tries to
prevent.

## Threat model

- **Out of scope:** a malicious CLI binary. The customer is the
  one running the CLI; they trust their own machine. The same
  assumption backs every customer-side `gregale deploy --tarball`
  invocation today.
- **In scope:** an attacker who can MITM the customer's network,
  replay their token, or compromise the apid server itself. The
  existing TLS + apid auth gates (authLimited → requireMFA →
  requireScope(ScopesDeployWriteSurface)) apply unchanged.
- **In scope:** the customer accidentally running `gregale deploy`
  in the wrong directory and uploading a tarball they did not
  intend. The existing dry-run is `gregale scan`; the new path
  prints the `Detected:` line (issue #961 leaf 2) before the
  upload so the customer sees the framework + entrypoint.

## Relationship to the existing source-ref path

| Aspect | source-ref (existing) | source-tarball (this ADR) |
|---|---|---|
| Auth | install token (server-side) | bearer token (customer's CLI) |
| Trust root | GitHub install row | CLI binary on customer's machine |
| Tarball source | codeload via githubd | local FS via CLI |
| Ref shape | branch/tag → resolved to 40-char SHA on apid | whatever the CLI produced (no resolution) |
| SHA pinning | yes (codeload archive is pinned to resolved SHA) | no (ref is informational only) |
| Idempotency-Key | yes (auto-minted) | yes (auto-minted) |
| Source URL on row | `github://<repo>@<sha>` | `local-tar://<repo>` |
| Audit kind | `deploy.source_ref` | `deploy.local_tarball` |
| Use case | CI, SHA-pinned production deploys | first-deploy, local dev, no GitHub App |

The customer picks the path: zero-config → tarball upload; explicit
`--repo X --ref SHA` → source-ref fetch. The CLI never auto-decides
between the two.

## Why a separate file in `cmd/apid/`

The new handler lives in `cmd/apid/handlers_source_tarball.go`
(neither in `handlers_source_ref.go` nor `handlers.go`'s
`createDeployment`). Reasoning:

- `handlers_source_ref.go` is the bit-identical trust chain we are
  NOT relaxing; co-locating would invite a future reviewer to "fix"
  the gate there by accident.
- `handlers.go::createDeployment` is the canonical multipart upload
  path for `--tarball`, `--image`, `--dockerfile`, and `--runtime`;
  its audit kind is `deployment.created` and its Kind dispatch is
  image / tarball / dockerfile. Forcing the new path through it
  would either fork the Kind dispatch (bad) or widen the existing
  audit shape (worse). A separate handler with a separate audit kind
  keeps the wire and the audit log honest.

## Consequences

- **Positive.** A new customer can run `gregale deploy` inside any
  GitHub-hosted git repo and get a live URL without installing the
  Gregale GitHub App. The CI path (`--repo X --ref SHA`) keeps its
  SHA-pinning guarantee; the local path is "whatever the customer
  shipped." Both paths have parity on size caps, idempotency, and
  audit emission.
- **Negative.** The local path has no upstream SHA pinning. A
  customer who needs that guarantee must use `--repo X --ref SHA`
  explicitly. The README documents the trade-off; PR-B of Mega-A
  (out of scope here) will add a `gregale deploy --headless`
  flag-pair to nudge CI users toward the source-ref path.
- **Wire.** Adds one new HTTP route (`POST
  /v1/apps/{slug}/deployments/source-tarball`), one new DTO
  (`api.SourceTarballDeployRequest`), one new SDK method
  (`Client.DeployFromSourceTarball`), one new audit kind
  (`deploy.local_tarball`). No PG migration.

## Reversibility

The new handler, route, SDK method, and audit kind can all be
removed without affecting the existing source-ref path or the
createDeployment multipart path. The single coupling point is the
shared `validateAndSpool` / `apidsource.Enqueue` helpers, which
both paths were already using. A `git revert` of the PR that lands
this ADR cleanly undoes the change.

## Rollout

Wave 1 of the issue #961 mega-PR cluster (the umbrella tracks the
3-wave PR plan). The CLI auto-decides zero-config → tarball upload;
the source-ref path is unchanged and continues to work for all
existing CI customers.

## Cross-references

- Issue #961 (umbrella), #961 PR-1 (this ADR's implementation)
- Issue #739, ADR-092 (existing source-ref)
- `cmd/apid/handlers_source_ref.go` (the unchanged trust chain)
- `cmd/apid/handlers_source_tarball.go` (the new handler)
- `pkg/api/client.go::Client.DeployFromSourceTarball` (the SDK)
- `pkg/api/dto.go::SourceTarballDeployRequest` (the DTO)
- `pkg/api/limits.go::SourceTarballMaxMB` (the cap)
