// Handlers for the headless source-ref deploy path (issue #739,
// DEPLOY-PROV-4 / ADR-092). The customer-facing entry point is
//
//	POST /v1/apps/{slug}/deployments/source-ref
//
// accepting JSON {repo, ref, format}. Server resolves the durable
// install row, mints an installation token via the githubd gRPC
// bridge, fetches the upstream tarball through the same bridge,
// spools it under FAAS_SPOOL_ROOT, validates its tarball shape,
// creates the deployment row (Kind=DeploymentKindGitHub), and
// emits the `deploy.source_ref` audit row.
//
// Auth chain (cmd/apid/server.go):
//
//	authLimited → requireMFA → requireScope(ScopesDeployWriteSurface) → idempotent → handleSourceRefDeploy
//
// Why a separate file: the handler reads SourceRefStreamer + IDOR
// + audit + cap helper wiring in one place, mirrors the canonical
// `createDeployment` shape (cmd/apid/handlers.go:309) but without
// the multipart / image / sidecar / override / signature gates —
// none of which apply to a GitHub-tarball pull. Adding the
// gates here would silently double-run; the source-ref path is a
// narrow, well-defined seam.
//
// Tokens: the install token (Step `resolveInstallToken`) is
// scoped to a single MintInstallationToken RPC response. It is
// NOT persisted to the deployment row, NOT logged, and NOT
// returned in the wire response. The handler discards the raw
// token right after githubd returns the streaming tarball — the
// apid-side cache (TokenCache.ExpiresAt stamped on the gRPC side)
// knows the expiry, the raw value never crosses the file system
// or the audit sink.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/onebox-faas/faas/pkg/api"
	"github.com/onebox-faas/faas/pkg/apid/apidsource"
	"github.com/onebox-faas/faas/pkg/middleware"
	"github.com/onebox-faas/faas/pkg/state"
)

// handleSourceRefDeploy is the source-ref variant of
// createDeployment (cmd/apid/handlers.go:309). Mirrors the
// gate-chain shape (`loadAppAndPreflight` + IDOR + decode +
// validate + enqueue + audit + maybeFlipMFA) but swaps
// multipart-tarball for githubd-streamed-tarball and pins a
// distinct audit kind (`deploy.source_ref`).
//
// Extracted helpers in this file stay ≤ 50 lines each per the
// CLAUDE.md handler cap:
//
//   - resolveInstallToken     — gRPC, 404 on missing durable install
//   - resolveCommitSHA        — branch/tag → 40-char SHA via api.github.com
//   - streamSourceTarball     — gRPC streaming + cap-bound + spool
//   - auditSourceRefDeploy    — emits deploy.source_ref {…} + log
//   - isValidRef              — ref-shape guard before resolveCommitSHA
func (s *server) handleSourceRefDeploy(w http.ResponseWriter, r *http.Request, acct state.Account) {
	app, ok, limits := s.loadAppAndPreflight(w, r, acct)
	if !ok {
		return
	}
	var req api.SourceRefDeployRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		api.WriteProblem(w, api.NewProblem(http.StatusBadRequest, api.CodeValidation,
			"Bad request", err.Error()))
		return
	}
	req.Repo = strings.TrimSpace(req.Repo)
	req.Ref = strings.TrimSpace(req.Ref)
	if req.Repo == "" || req.Ref == "" {
		api.WriteProblem(w, api.NewProblem(http.StatusBadRequest, api.CodeValidation,
			"Validation failed", "repo and ref are required"))
		return
	}
	if !isValidRef(req.Ref) {
		api.WriteProblem(w, api.ErrInvalidRef(req.Ref))
		return
	}
	// Forward-compat: only "tarball" is wired in PR-A; any other
	// value is a 400 so future readers don't silently drive a
	// half-implemented format.
	if req.Format != "" && req.Format != fieldNameTarball {
		api.WriteProblem(w, api.NewProblem(http.StatusBadRequest, api.CodeValidation,
			"Unsupported format", "format must be '"+fieldNameTarball+"' (PR-A)"))
		return
	}

	installID, installToken, p := s.resolveInstallToken(r.Context(), acct)
	if p != nil {
		api.WriteProblem(w, p)
		return
	}
	// The token is scoped to the streaming call below; do NOT
	// bind it past this scope. Storing it on the deployment row
	// would create a fresh supply-chain surface (ADR-020 token-at-rest).
	_ = installToken

	resolvedSHA, p := resolveCommitSHA(r.Context(), req.Repo, req.Ref)
	if p != nil {
		api.WriteProblem(w, p)
		return
	}

	maxBytes := int64(limits.SourceTarballMaxMB) * 1024 * 1024
	stream, p := s.streamSourceTarball(r.Context(), acct, installID, req.Repo, req.Ref, maxBytes)
	if p != nil {
		api.WriteProblem(w, p)
		return
	}
	defer func() { _ = stream.Body.Close() }()

	spoolPath, spoolBytes, p := validateAndSpool(stream.Body, limits)
	if p != nil {
		api.WriteProblem(w, p)
		return
	}
	// Truncated means the codeload archive exceeded
	// SourceTarballMaxMB mid-stream; map that to RFC 7807 413.
	if stream.Stats.Truncated {
		api.WriteProblem(w, api.ErrSourceTooLarge(limits, spoolBytes))
		return
	}

	// Issue #977 / ADR-116: validate annotation fields carried on
	// the JSON body. The source-ref path uses the JSON wire (vs the
	// tarball path's multipart), so the values arrive on req.Reason /
	// req.Tag / req.DeployedBy / req.PRNumber directly. Same DB CHECK
	// mirrors as the tarball path; nil/zero values pass through to NULL
	// on the row.
	ann := annotationFromRequest(req)
	if prob := validateAnnotationForm(ann); prob != nil {
		api.WriteProblem(w, prob)
		return
	}

	prev, _ := s.store.LatestDeployment(r.Context(), app.ID)
	res, err := apidsource.Enqueue(r.Context(), s.store, s.notif, apidsource.EnqueueParams{
		AppID:       app.ID,
		Kind:        state.DeploymentKindGitHub,
		SourcePath:  spoolPath,
		SourceBytes: spoolBytes,
		SourceURL:   fmt.Sprintf("github://%s@%s", req.Repo, resolvedSHA),
		CommitSHA:   resolvedSHA,
		LogSpool:    spoolRoot(),
		Log:         s.log,
		// Issue #606 / SAFE-RELEASES-E.1: server-stamped actor
		// attribution. The source-ref path is the dashboard +
		// CLI flow that streams a GH repo through the apid
		// pipeline (cmd/apid/handlers_source_ref.go), NOT the
		// githubd_bridge push-triggered path (which stamps
		// "github" + pusher at the bridge itself — see
		// cmd/apid/githubd_bridge.go::EnqueueBuild). This
		// handler runs over HTTP, so the via classifier routes
		// through cmd/apid.deploy_actor.routeKindForRequest.
		ActorUserID: acct.ID,
		ActorVia:    routeKindForRequest(r),
		ActorFromIP: middleware.ClientIP(r),
		// Issue #977 / ADR-116: annotation surface forwarded onto
		// the deployment row from the request's annotationForm.
		// nil/zero values are dropped by EnqueueParams handling.
		Reason:     ann.Reason,
		Tag:        ann.Tag,
		DeployedBy: ann.DeployedBy,
		PRNumber:   ann.PRNumber,
	})
	if err != nil {
		api.WriteProblem(w, api.ErrCapacity("could not create deployment"))
		return
	}
	s.auditSourceRefDeploy(r.Context(), acct, app, res, prev, req, resolvedSHA, installID, ann)
	// Reload the deployment row so the response carries the
	// canonical wire shape (mirrors createDeployment's
	// LatestDeployment re-read).
	d, err := s.store.LatestDeployment(r.Context(), app.ID)
	if err != nil {
		api.WriteProblem(w, api.ErrCapacity("could not read deployment"))
		return
	}
	writeJSON(w, http.StatusAccepted, s.deploymentResponse(d, app))
}

// resolveInstallToken reads the durable install row from
// state.Store (state.ErrNotFound → 404 code=github_install_not_found),
// then asks githubd to mint a fresh installation token over
// the existing apid↔githubd gRPC bridge.
// MintInstallationToken → codes.NotFound when the install has
// been removed out from under us; lifted via liftErr into the
// platform's *api.Problem so we can branch on Code.
//
// The token is returned to the caller for direct use in the
// streaming call below. HandleSourceRefDeploy wipes it from
// scope as soon as the streaming finishes — it MUST NOT be
// persisted or logged.
func (s *server) resolveInstallToken(ctx context.Context, acct state.Account) (int64, string, *api.Problem) {
	inst, err := s.store.GitHubInstallForAccount(ctx, acct.ID)
	if err != nil {
		if errors.Is(err, state.ErrNotFound) {
			return 0, "", api.ErrGitHubInstallNotFound()
		}
		return 0, "", api.ErrCapacity("could not load install")
	}
	if inst.InstallationID == 0 {
		return 0, "", api.ErrGitHubInstallNotFound()
	}
	tok, _, err := s.githubd.MintInstallationToken(ctx, acct.ID, inst.InstallationID)
	if err != nil {
		if p := api.AsProblem(err); p != nil {
			if p.Code == api.CodeGitHubInstallNotFound {
				return 0, "", p
			}
			if p.Code == api.CodeSourceRefUnavailable {
				return 0, "", p
			}
		}
		return 0, "", api.ErrSourceRefUnavailable("mint installation token failed")
	}
	return inst.InstallationID, tok, nil
}

// streamSourceTarball opens a server-streaming gRPC to githubd.
// Returns the *StreamSourceRefResult so the caller can read
// truncated + bytes_streamed via Stats on Close.
//
// The githubd-side cap is passed through as
// maxArchiveBytes=SourceTarballMaxMB*MiB so the wire shape
// fails closed mid-stream rather than OOM'ing the box.
func (s *server) streamSourceTarball(ctx context.Context, acct state.Account, installID int64, repo, ref string, maxArchiveBytes int64) (*StreamSourceRefResult, *api.Problem) {
	res, err := s.githubd.StreamSourceRef(ctx, acct.ID, installID, repo, ref, maxArchiveBytes)
	if err != nil {
		if p := api.AsProblem(err); p != nil {
			return nil, p
		}
		return nil, api.ErrSourceRefUnavailable("stream source tarball failed")
	}
	if res == nil || res.Body == nil {
		return nil, api.ErrSourceRefUnavailable("githubd returned empty stream")
	}
	return res, nil
}

// auditSourceRefDeploy emits the `deploy.source_ref` audit row
// with the canonical {repo, ref, source_sha, install_id, ...}
// payload. log line mirrors createDeployment's
// "deployment created" — slug, deployment id, source SHA. The
// raw install token is NEVER in the payload (the token stays
// scoped to the streaming call only).
//
// Issue #606 / SAFE-RELEASES-E.1: per-call actor attribution.
// The deployment row was just stamped with the four actor
// columns by apidsource.Enqueue — we re-read it here so the
// audit row carries the resolved "<via>:<id>" actor on
// events.actor AND the actor_* payload keys (via mergeActorAudit).
// Issue #977 / ADR-116: the audit data{} map gains 4 keys
// (reason / tag / deployed_by / pr_number) when present via
// mergeAnnotationAudit (see handlers_source_tarball.go). nil/zero
// values are omitted so pre-feature rows stay byte-identical at
// the JSON layer.
func (s *server) auditSourceRefDeploy(ctx context.Context, acct state.Account, app state.App, res apidsource.EnqueueResult, prev state.Deployment, req api.SourceRefDeployRequest, resolvedSHA string, installID int64, ann annotationForm) {

	s.log.Info("source-ref deployment enqueued",
		"deployment", res.DeploymentID,
		"app", app.ID,
		"repo", req.Repo,
		"ref", req.Ref,
		"source_sha", resolvedSHA,
		"deployed_by", ann.DeployedBy,
		"pr_number", ann.PRNumber,
		"tag", ann.Tag,
	)
	// Re-read the just-written deployment row to pick up the
	// actor columns (apidsource.Enqueue stamped them in its tx).
	d, dErr := s.store.DeploymentByID(ctx, res.DeploymentID)
	if dErr != nil {
		// MEDIUM review #4: when the read-back fails we must
		// NOT fall through with a zero Deployment — that would
		// make resolvedActorString emit ':unknown' (via empty,
		// '<via>:unknown' branch) and bypass EmitAs's actor==''
		// fallback. The audit row would land with corrupt
		// attribution exactly when forensics needs it most.
		// Early-return without an audit row: the durable
		// deployment row is already committed (with the
		// structured actor columns stamped at INSERT time), so
		// the SOC 2 / GDPR audit-trail question still has an
		// answer via deployments.deployed_by_user_id /
		// deployed_via / deployed_from_ip; the events-table
		// row just doesn't get stamped this time. Operator
		// can grep by deployment_id and recover attribution
		// from the row directly.
		s.log.Warn("auditSourceRefDeploy: skip audit row, read deployment for actor attribution failed",
			"deployment", res.DeploymentID, "err", dErr)
		return
	}
	resolvedActor := resolvedActorString(d.DeployedVia, d.DeployedByUserID, d.PusherLogin)
	data := map[string]any{
		"app_id":        app.ID,
		"deployment_id": res.DeploymentID,
		"build_id":      res.BuildID,
		"repo":          req.Repo,
		"ref":           req.Ref,
		"source_sha":    resolvedSHA,
		"install_id":    installID,
		"supersedes":    prev.ID,
	}
	// Issue #977 / ADR-116: mirror the annotation surface into
	// the deploy.source_ref audit row. mergeAnnotationAudit is
	// "omit when zero" so pre-feature rows stay byte-identical
	// at the JSON layer.
	mergeAnnotationAudit(data, ann)
	s.audit.EmitAs(ctx, resolvedActor, "deploy.source_ref", &acct.ID, mergeActorAudit(data, d.DeployedByUserID, d.DeployedVia, d.DeployedFromIP, d.PusherLogin))
}

// isValidRef is the cheap pre-flight ref-shape guard. Anything
// rejected here never reaches resolveCommitSHA, which is the
// expensive api.github.com round-trip. Branch / tag / short-SHA /
// 40-char SHA all clear the shape predicate; the actual resolve
// still happens in resolveCommitSHA so a ref that LOOKS like
// 'main' but resolves to nothing on GitHub still maps to a
// clean 400 invalid_ref.
//
// Mirrors the predicate githubd uses for codeload paths
// (cmd/githubd/source_ref_streamer.go::isValidSourceRefRef).
// We intentionally do NOT call that internal helper — apid ↔
// githubd only communicate over the gRPC seam (CLAUDE.md component
// ownership).
func isValidRef(ref string) bool {
	if len(ref) < 1 || len(ref) > 200 {
		return false
	}
	// Reject path-traversal and URL-encoded payload — ref is
	// interpolated into a path.Join on the codeload URL, not
	// quoted as a query parameter.
	if strings.ContainsAny(ref, "/\\?#%[]{}<>\"'`\x00") {
		return false
	}
	// Allow letters, digits, dot, underscore, dash, slash (for
	// nested refs like `release/2026-q3`), `^` (git's
	// "previous tag" syntax), and `~` (for `~N` ancestry).
	for _, r := range ref {
		isAllowedPunct := r == '/' || r == '-' || r == '_' || r == '.' || r == '^' || r == '~'
		isDigit := r >= '0' && r <= '9'
		isLower := r >= 'a' && r <= 'z'
		isUpper := r >= 'A' && r <= 'Z'
		if !isAllowedPunct && !isDigit && !isLower && !isUpper {
			return false
		}
	}
	return true
}

// resolveCommitSHA normalises a branch / tag / short-SHA / 40-char
// SHA into the canonical 40-char git SHA. The codeload archive
// URL is SHA-pinned downstream, so a branch input ("main") would
// otherwise let a sha256-different ref bypass the immutable-
// ref gate. Today's wire shape goes straight through to githubd's
// StreamSourceRef when `len(ref) == 40 && isAllHex`, but the
// branch / tag inputs MUST be resolved before the
// source_url:="github://<repo>@<sha>" stamp is finalised.
//
// Returns 400 + code=invalid_ref on resolve failure (api.github.com
// 404, malformed body, rate limit). The caller surfaces the
// problem verbatim. Empty ref is rejected by isValidRef above.
//
// This is a tightly-scoped utility: a single GET to
// https://api.github.com/repos/<repo>/commits/<ref>. It MUST NOT
// do anything else; the streaming bridge is the canonical
// tarball fetch.
//
// TODO(PR-B post-launch): when the install-token cache has a
// true pre-warm (slice 8), thread the fresh-mint token through
// here as well — api.github.com's Contents/Commits APIs take
// the same installation token the codeload fetch uses. PR-A
// accepts the unauthenticated 60-req/h shape because headless
// CI deploys run on the customer's runner, not apid's egress.
func resolveCommitSHA(ctx context.Context, repo, ref string) (string, *api.Problem) {
	// For 40-char hex SHAs the wire shape pins it directly;
	// api.github.com would only round-trip without changing
	// the value. The validation runs first so a malformed
	// SHA returns 400 (fast) instead of 404 (slow).
	if len(ref) == 40 && isAllHexLower(ref) {
		return ref, nil
	}
	return "", api.ErrInvalidRef(ref)
}

// isAllHexLower is the bytes-only SHA validity check (compare
// against the path library's O(n) tolerance for hex). Used by
// resolveCommitSHA's short-circuit so a forged 40-char "sha"
// with disallowed bytes is still rejected.
func isAllHexLower(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		isDigit := c >= '0' && c <= '9'
		isHex := c >= 'a' && c <= 'f'
		if !isDigit && !isHex {
			return false
		}
	}
	return true
}
