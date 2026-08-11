# ADR-092 · Headless source-ref deploy from CI (issue #739 / DEPLOY-PROV-4)

- **Status:** accepted (PR-A server foundation + Node SDK regen
  merged; PR-B CLI surface + customer doc in flight)
- **Date:** 2026-08-10
- **Issue:** #739 / DEPLOY-PROV-4
- **Supersedes:** the implicit "dashboard-only" `--repo` flow at
  `cmd/gregale/commands2.go:849-861` → `commands2.go:1170-1187`
  (`cmdDeployRepo`), which opens a dashboard bind picker that a
  CI runner has no way to complete.
- **Related:** ADR-014 (existing `--repo` dashboard flow — the
  `gregale connect` push-bind path remains for human-driven
  binds), ADR-020 (install-token sealing at rest), ADR-012
  (install-token exchange + `pkg/githubd.TokenCache`),
  ADR-038 (build lifecycle), ADR-050 (provision apply),
  ADR-083 (function-vs-app shape auto-detection),
  ADR-041 (migration slot reservation fence), spec §14
  milestone M5 (DEPLOY-PROV).

## Context

Today, `gregale deploy --repo OWNER/NAME` opens a dashboard
bind picker (`commands2.go:1170-1187`). The dashboard holds
the GitHub App install token in the browser session — there
is no programmatic re-bind path. CI runners have no browser,
the durable `github_installations` row never materialises
under the customer's account, and pushes never auto-deploy.
The dashboard flow stays for human-driven binds (where a
human wants the repo to auto-deploy on every push to a
production branch); the new endpoint supersedes `--repo` on
the CLI for one-shot deploys.

**Goal:** ship a scripted, headless path for CI: pinned
commit, no install token in the runner, audit-trail-friendly.
Issue #739 closes when `gregale deploy --repo OWNER/NAME
--ref <sha>` works in a CI job with `FAAS_TOKEN` and no
GitHub env vars.

## Three alternatives

| Axis | Option 1 (server-side) | Option 2 (CLI-side install token) | Option 3 (git + deploy key on control plane) |
|------|------------------------|-----------------------------------|---------------------------------------------|
| Code change size | Small. Reuse `pkg/gitfetch`, `apidsource.Enqueue`, `validateAndSpool`. ~6 new files, ~3 small edits. | Moderate. Extend `readInstallToken` + keychain surface, replicate `fetchRepoTarball` plumbing to the deploy path. | Largest. Need a `git` binary on the control plane, SSH/HTTPS-with-deploy-key plumbing, a new `git archive` wrapper. |
| Security posture | Best. Install token is server-side only (sealed at rest per ADR-020, never reaches CLI). CI auth = existing bearer API key (`FAAS_TOKEN`). | Worse. Install token must reach the CI runner; new env-var or keychain surface. Customer's CI vendor must be trusted with the install token. | Mixed. Deploy keys are per-repo and need rotation; the `git` binary on the control plane is a new supply-chain surface. |
| CI DX | Simplest. `gregale deploy --repo OWNER/NAME --ref <sha>` with `FAAS_TOKEN`. No GitHub env vars, no extra auth. | Moderate. CI needs both `FAAS_TOKEN` and `GREGALE_INSTALL_TOKEN_<id>`; customer must provision the install token in the runner. | Hardest. CI must mint or hold a deploy key per repo; rotate on every secret leak. |
| Spec compliance | Clean. No new ADR surface required beyond documenting the route + audit kind. | Adds a new auth boundary (CLI holding install tokens); needs ADR for the keychain surface expansion. | New `git`-binary dependencies on the production control plane; new ADR for the SSH/deploy-key posture. |
| Audit (per #604) | Fits. Same `(*Auditor).Emit` JSONB blob; new `deploy.source_ref` kind with `source_sha`. | Same. | Same. |
| Idempotency / retries | Inherits. Route uses the existing `s.idempotent` middleware (`Idempotency-Key` header); replay returns the same build row. | Custom retry semantics — CLI must cache `Idempotency-Key` locally. | Same complexity as option 1, but the new code path would re-implement the idempotent middleware anyway. |
| Reuse of existing surface | Maximum. `pkg/gitfetch`, `cmd/githubd/source_fetcher.go`, `pkg/githubd.TokenCache`, `apidsource.Enqueue` are all production-wired. | Reuses the curl plumbing at `commands_decompose.go:166-188` but inverts the trust model (token flows the wrong way). | Reuses nothing — needs new infrastructure. |

## Decision

Choose **option 1 (server-side source-ref import)**.

Add `POST /v1/apps/{slug}/deployments/source-ref` accepting
JSON `{ repo, ref, format }`. apid resolves the install
token from the durable `github_installations` row (via the
existing githubd gRPC bridge), fetches the tarball through a
small `pkg/gitfetch` extension (`Streamer` interface),
spools it under `FAAS_SPOOL_ROOT/<id>.tar.gz`, runs
`validateTarballShape`, and calls `apidsource.Enqueue(Kind:
DeploymentKindGitHub, SourceURL: "github://<repo>@<sha>",
CommitSHA: <resolved SHA>)`. Audit row `deploy.source_ref`
carries `{repo, ref, install_id, source_sha}`.

### Sub-decisions

1. **Route shape:** `POST /v1/apps/{slug}/deployments/source-ref`
   (sibling of existing `POST /v1/apps/{slug}/deployments`).
   JSON body. Chain:
   `authLimited → requireMFA → requireScope(ScopesDeployWriteSurface) → idempotent → handler`.

2. **Scope carve-out:** reuse `ScopesDeployWriteSurface =
   [ScopeAdmin, ScopeDeployWrite]` (`pkg/api/apikey.go:236`).
   No new scope constant.

3. **Audit kind vocabulary:** new `deploy.source_ref` kind
   (`cmd/apid/handlers_sidecars.go:415` `s.audit.Emit`).
   Distinct from `app.deployed`. `data.source_sha` carries
   the resolved 40-char SHA (the audit row is post-mortem
   evidence, so the resolved SHA — not the customer's input
   ref — is the load-bearing field).

4. **Kind enum:** reuse `DeploymentKindGitHub`
   (`pkg/state/types.go:62`). The `builds_kind_check`
   constraint was already widened to permit `github` by
   migration 00085; `apidsource.Enqueue` already supports
   it for the githubd push-dispatch path
   (`cmd/apid/githubd_bridge.go::EnqueueBuild`).

5. **Idempotency:** wrap in `s.idempotent`. Same
   `Idempotency-Key` returns the cached row plus
   `Idempotent-Replayed: true` header.

6. **Ref validation:** accept 40 lowercase-hex SHAs, 7+ hex
   short SHAs, branches, tags. Refuse anything failing
   `pkg/gitfetch/http.go::isValidCommitSHA` after
   `resolveCommitSHA` normalises branch/tag inputs to a SHA
   via `GET https://api.github.com/repos/<repo>/commits/<ref>`.
   404 from GitHub → 400 + `code=invalid_ref`.

7. **Install-token lifecycle:** minted on demand via the
   new `MintInstallationToken` gRPC (added to
   `api/proto/onebox/faas/githubd/v1/githubd.proto`).
   githubd's `TokenCache.Token(ctx, installationID)`
   (`pkg/githubd/tokencache.go:97`) handles singleflight +
   5-min proactive refresh. On 401 mid-fetch:
   `TokenCache.Invalidate` + retry once. On retry 401:
   surface 503 + `code=source_ref_unavailable`. Token is
   scoped to one RPC; it never reaches apid process state
   beyond the lifetime of the handler call.

8. **Source tarball cap:** reuse `SourceTarballMaxMB`
   (100 MB Free/Hobby, 250 MB Pro/Scale;
   `pkg/api/limits.go:67`). Same posture as the multipart
   path. Customers who exceed it upgrade plans; a future PR
   can introduce `SourceRefMaxMB` if telemetry shows demand.

9. **Spool pipeline:** refactor
   `cmd/apid/deploy_inputs.go::validateAndSpool` to accept
   `io.Reader` instead of `*multipart.Part`. The new
   `streamSourceTarball` helper calls a new
   `pkg/githubd.SourceRefStreamer.Stream` over the existing
   `/run/faas/apid-githubd.sock` gRPC channel, hands the
   returned `io.ReadCloser` to `validateAndSpool`, which
   writes to a tmp file, enforces the per-plan cap, runs
   `validateTarballShape`, and `os.Rename`s the tmp file to
   `FAAS_SPOOL_ROOT/<id>.tar.gz`. The existing multipart
   path becomes a thin caller of the new
   `io.Reader`-shaped helper.

9a. **Streaming seam — why not in `pkg/gitfetch`.** The
    plan originally proposed adding `Streamer` to
    `pkg/gitfetch` so apid could stream the tarball
    directly. That would have leaked AppAuth credentials
    into apid (the only consumer of an install-token-bound
    stream is the githubd path). The corrected seam is:

   - `pkg/githubd.SourceRefStreamer` — new interface
     declaring `Stream(ctx, accountID, installID, repoFullName, ref) (io.ReadCloser, error)`.
   - `cmd/githubd/source_ref_streamer.go` — impl that
     looks up the install row via `s.installs`, mints a
     token via `s.tokens.Token`, and proxies the body of
     `https://codeload.github.com/<repo>/tar.gz/<ref>` (the
     existing `httpFetcher` in `pkg/gitfetch/http.go` is
     reused here — no new package-internal types added).
     The `io.ReadCloser` is the response body wrapped in
     `io.LimitReader(SourceTarballMaxMB + 1, …)`.
   - `pkg/githubd` server registers `StreamSourceRef` over
     the existing gRPC socket. The token never leaves
     githubd's process address space.

10. **Build row:** `apidsource.Enqueue(Kind:
    DeploymentKindGitHub, SourceURL: "github://<repo>@<sha>",
    CommitSHA: <sha>)` — same shape as
    `cmd/apid/githubd_bridge.go::EnqueueBuild`. The
    `notifyAndAuditDeployment` helper already emits the
    existing `app.deployed` row; the new `deploy.source_ref`
    row is emitted additionally (additive, per
    `cmd/apid/handlers_sidecars.go:415` precedent).

11. **CLI change.** Replace the `--repo` branch at
    `cmd/gregale/commands2.go:849-861` with a new
    `cmdDeployRepoSourceRef` (slot: `commands_decompose.go`,
    next to the now-deleted `fetchRepoTarball`). Add a
    `--ref` flag to `cmdDeployTarball`'s FlagSet.
    Delete `cmdDeployRepo`, `fetchRepoTarball`, and
    `readInstallToken` outright — these were the dashboard
    browser flow, and a CI script that hits `--repo` without
    `--ref` would otherwise silently open a browser.

12. **SDK method:** `DeployFromSourceRef` in
    `pkg/api/client.go` next to `Deploy` / `DeployMultipart`.
    Uses `c.do` (auto-mints `Idempotency-Key` on every POST).
    No `methodRouteMap` entry needed; the
    `cmd/sdk-coverage/main.go::deriveMethodName` derives
    `DeployFromSourceRef` from `POST
    /v1/apps/{slug}/deployments/source-ref`.

13. **No DB migration.** `DeploymentKindGitHub` widened by
    migration 00085; `github_installations` from migration
    00059; `apps.github_install_id` / `apps.github_repo_full_name`
    from migrations 00007 / 00050; audit JSONB already
    accepts the new kind. The PRs are pure additions.

### File-by-file change list (PR-A)

| File | Change |
|------|--------|
| `docs/adr/092-headless-source-ref-deploy.md` | NEW (this file). |
| `pkg/api/dto.go` | ADD `SourceRefDeployRequest` next to `ProjectScanRequest`. |
| `pkg/api/errors.go` | ADD `CodeGitHubInstallNotFound`, `CodeInvalidRef`, `CodeSourceRefUnavailable` + constructors. |
| `pkg/gitfetch/fetcher.go` | ADD `Streamer` interface next to `Fetcher`. |
| `pkg/gitfetch/http.go` | ADD `httpFetcher.Stream` (reuses path.Join + Authorization, returns `io.LimitReader(maxArchiveBytes+1, …)`). |
| `pkg/gitfetch/http_test.go` | ADD `TestHTTPFetcher_Stream_*` (auth header, cap, path escape). |
| `api/proto/onebox/faas/githubd/v1/githubd.proto` | ADD `MintInstallationToken` RPC + `MintInstallationTokenRequest`/`Response`. |
| `pkg/githubd/{githubd_grpc.pb.go, githubd.pb.go}` | regenerated via `make proto`. |
| `pkg/githubd/` | ADD `MintInstallationToken` handler that wraps `s.tokens.Token`. |
| `cmd/apid/githubd_bridge.go` | ADD `MintInstallationToken` client method. |
| `cmd/apid/deploy_inputs.go` | REFACTOR `validateAndSpool` to accept `io.Reader`; multipart path becomes thin caller. |
| `cmd/apid/handlers_source_ref.go` | NEW. `handleSourceRefDeploy` + helpers (`resolveInstallToken`, `resolveCommitSHA`, `streamSourceTarball`). |
| `cmd/apid/server.go` | ADD `POST /v1/apps/{slug}/deployments/source-ref` mount next to the existing deployments mount. |
| `cmd/apid/handlers_source_ref_test.go` | NEW. Whitebox tests (IDOR, audit, spool, ref validation, size cap, rate limit, idempotency, install-token missing). |
| `api/openapi.yaml` | ADD `/v1/apps/{slug}/deployments/source-ref` path + `SourceRefDeployRequest` schema. |
| `pkg/apid/openapi.yaml` | regenerated via `make spec-sync`. |
| `cmd/apid/handlers_audit_test.go` | ADD assertion for `deploy.source_ref`. |

## Consequences

### Positive

- CI runners can deploy a pinned commit from a private repo
  with `FAAS_TOKEN` alone — no GitHub env vars, no install
  token in the runner.
- The install token stays inside the control plane (sealed at
  rest per ADR-020); the customer never sees it.
- The `deploy.source_ref` audit row carries the resolved
  40-char SHA + `install_id` — every post-mortem question
  "what did CI ship?" has a one-row answer.
- The dashboard `gregale connect` flow is unchanged — human
  pushes to a bound production branch still auto-deploy.

### Negative

- Adds a new RPC (`MintInstallationToken`) to the
  `apid-githubd.sock` surface. Small additive change; no
  breaking impact on existing githubd consumers.
- Refactoring `validateAndSpool` to accept `io.Reader` is a
  small behaviour-preserving refactor of a load-bearing
  helper; the multipart path's tests must still pass.
- A small new drift surface: any future change to
  `SourceRefDeployRequest` field tags must update the
  OpenAPI schema, or `make spec-check` fails.

### Neutral

- SourceRefMaxMB is not added now; customers who exceed
  `SourceTarballMaxMB` upgrade plans or wait for telemetry.
- `cmdDeployRepo` (the dashboard browser flow) is deleted
  outright, not gated behind `--legacy-bind`. The dashboard
  `gregale connect` flow is the human-driven bind path; the
  CLI's one-shot `--repo` was always redundant with it for
  human users.

## Verification

### Local

- `make sqlc-check` — green (no sqlc change; new method is
  inline SQL on the `apid-githubd` RPC, not state.go).
- `make proto` — green (new RPC + regenerated stubs).
- `make spec-sync` — green (DTO + route + schema detected).
- `make lint` — green (`gofmt -l` repo-wide).
- `go test ./pkg/api/... ./pkg/gitfetch/...
  ./pkg/githubd/... ./cmd/apid/... ./cmd/gregale/...` —
  green.

### CI

- `lint + build` — green.
- `unit tests (pure Go shard 1/2 + pg shard 1/2)` — green.
- `spec-check` — green.
- `sdk-go` — green (no `methodRouteMap` entry; auto-
  derivation produces `DeployFromSourceRef`).
- `daemonunit-check` — green. No daemon unit change.
- `migrations (contiguity + apply)` — green. **No new
  migration in PR-A.**
- `e2e (4 shards)` — green. Blackbox e2e untouched.

### Acceptance gate (closes issue #739)

```
$ go test ./pkg/api/... -run TestSweep_DeployFromSourceRef -v
--- PASS: TestSweep_DeployFromSourceRef

$ go test ./cmd/apid/... -run TestHandleSourceRefDeploy -v
--- PASS: TestHandleSourceRefDeploy (8 subtests passed)

$ make spec-check && make sdk-check && make test
... all green
```

### Rollback

Revert the PR-A commits in reverse order. The route, the
`Streamer` interface, the gRPC RPC, the new DTO, and the
new error codes are all pure additions; reverting removes
the wire contract and the audit kind. PR-B (CLI + SDK)
reverts separately. No migration rollback.