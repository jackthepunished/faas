// cmd_deploy_source_ref.go — headless source-ref deploy (issue #739,
// DEPLOY-PROV-4 / ADR-092). CLI half of the PR-A server foundation
// (cmd/apid/handlers_source_ref.go + the apid↔githubd gRPC bridge).
//
// Replaces the M7.5 dashboard browser flow (the previous cmdDeployRepo
// at commands2.go:1040, deleted in PR-B). The CLI never touches the
// GitHub install token — apid resolves it server-side from
// github_installations via MintInstallationToken, fetches the
// codeload tarball through StreamSourceRef, spools it under
// FAAS_SPOOL_ROOT, validates shape, and enqueues a
// DeploymentKindGitHub build row. The audit row `deploy.source_ref`
// fires server-side (auditSourceRefDeploy); this CLI never emits
// audit data.
//
// CI acceptance gate (closes issue #739):
//
//	FAAS_API=https://api.faas.example \
//	FAAS_TOKEN=$FAAS_TOKEN \
//	gregale deploy --repo OWNER/NAME --ref $(git rev-parse HEAD)
//
// with no GREGALE_INSTALL_TOKEN_* env vars set.

package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/onebox-faas/faas/pkg/api"
)

// cmdDeployRepoSourceRef posts {repo, ref, format:"tarball"} to the
// PR-A endpoint and streams the build log. Idempotency-Key is
// auto-minted by Client.do (pkg/api/client.go:202-208) for any
// non-GET/HEAD; callers do NOT set one — CI retries with the same
// wire line still mint a fresh key on each invocation, producing
// distinct build rows. Customer-side manual idempotency-key plumbing
// is intentionally out of PR-B scope (documented in
// docs/source-ref.md failure-modes section).
//
// On 409 source_ref_unavailable the server sets Retry-After:30; we
// surface the value via printErr + Problem.HasHeader so the operator
// sees a precise backoff hint instead of a bare 503.
//
// jsonOutput true → single JSON-encoded DeploymentResponse on
// osStdout (matches the existing Deploy/DeployTarball wire shape).
// jsonOutput false → streamDeployLogs tail the SSE build log.
func cmdDeployRepoSourceRef(slug, repo, ref string) int {
	client, err := authedClient()
	if err != nil {
		return printErr("Not logged in", err)
	}
	ctx := context.Background()
	req := api.SourceRefDeployRequest{
		Repo:   repo,
		Ref:    ref,
		Format: "tarball",
	}
	dep, err := client.DeployFromSourceRef(ctx, slug, req)
	if err != nil {
		// errors.As (not type-assert) so a future wrapping in
		// the SDK chain (e.g. fmt.Errorf("%w: …")) still surfaces
		// the APIError. The SDK currently returns *APIError
		// directly, but the assertion-via-As is the lint-clean
		// shape (errorlint) and future-proof.
		var apiErr *api.APIError
		if errors.As(err, &apiErr) {
			// Surface the Retry-After hint on transient githubd
			// / codeload blips so the operator doesn't have to
			// reach for the audit log to figure out the backoff.
			if ra := apiErr.Problem.HasHeader("Retry-After"); len(ra) > 0 {
				return printErr("Source-ref unavailable",
					fmt.Errorf("%s (Retry-After: %ss)", apiErr.Problem.Code, ra[0]))
			}
		}
		return printErr("Deploy failed", err)
	}
	if jsonOutput {
		return jsonOut(writeJSON(dep))
	}
	return streamDeployLogs(client, dep)
}
