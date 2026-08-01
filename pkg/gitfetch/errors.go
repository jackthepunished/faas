// Package gitfetch — authenticated archive fetch from a Git hosting
// provider (PR-GH.3 of repo decomposition Phase 5).
//
// The package is HTTP-transport-only: it dollies an authenticated
// tar.gz archive from codeload.github.com (or an equivalent
// provider) into a fs.FS root. Credential lifecycle lives in
// pkg/githubd; reconcile stays credential-free. The split mirrors
// the Phase 5 design principle: each package owns one concern.
//
// Source tree lifecycle: Tree.Close() removes the temp dir
// (idempotent). Fetcher.Fetch returns a fresh Tree per call; the
// caller MUST defer Close().

package gitfetch

import (
	"errors"
)

// ErrUnauthorized is the sentinel Fetcher returns when the upstream
// hosting provider rejects the bearer token (HTTP 401). Callers
// surface this via pkg/audit/githubd.token_rejected so the
// operator can see install-token expiry without the response body
// being logged (the wrapped error includes the URL but never the
// Authorization header — wireNewHTTPRequest strips it).
var ErrUnauthorized = errors.New("gitfetch: unauthorized")

// ErrNotFound is the sentinel Fetcher returns when the upstream
// returns 404 (private repo, missing commit, or revoked
// installation). The wrapped error includes the URL + commit SHA
// but never the token. Callers translate this into a 200 +
// {no_binding:true} body on the webhook path (the customer's
// `faas deploy` will surface a separate scan error).
var ErrNotFound = errors.New("gitfetch: not found")

// ErrArchiveTooLarge is the sentinel Fetcher returns when the
// upstream archive exceeds the configured cap. The cap is set per
// plan (matches SourceTarballMaxMB from pkg/api/limits.go,
// inflated by the same 2.5× factor the apid path uses). Callers
// map this to a 422 with the per-plan limit embedded.
var ErrArchiveTooLarge = errors.New("gitfetch: archive too large")

// ErrBadArchive is the sentinel for malformed tar.gz bodies (gzip
// failure, tar header failure, or a `<root>/` prefix that walks
// outside the destination). All three are wrapped with the
// underlying error's message but never include HTTP body content.
var ErrBadArchive = errors.New("gitfetch: bad archive")
