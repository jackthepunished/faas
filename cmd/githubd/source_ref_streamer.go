// Source-ref streamer for the headless source-ref deploy path
// (DEPLOY-PROV-4 / ADR-092, issue #739). Bridges pkg/githubd's
// SourceRefStreamer interface to the GitHub codeload archive
// endpoint, scoped to the durable install row's installation token.
//
// The streamer never extracts or buffers the tarball — it returns
// the live HTTP response body wrapped in io.LimitReader. The caller
// (apid's handleSourceRefDeploy) pipes the body into
// validateAndSpool and lets the per-plan cap + shape check happen
// there, exactly the same posture as the multipart path.
//
// Token lifetime:
//   - Resolve install row → state.GitHubInstall.
//   - Mint token via TokenCache.Token(installationID) (singleflight +
//     5-min proactive refresh).
//   - GET https://codeload.github.com/<repo>/tar.gz/<ref> with
//     `Authorization: Bearer <token>`. Body is the tar.gz stream.
//   - On 401 mid-stream: TokenCache.Invalidate + retry once.
//   - Retry 401 → ErrSourceRefUnavailable (the apid handler turns
//     this into 503 + code=source_ref_unavailable).
//   - On any other 4xx/5xx: surface ErrNotFound / ErrBadArchive
//     from pkg/gitfetch (so callers can errors.Is them).
//
// The token is scoped to a single Stream call. It never appears in
// log lines, URLs, response bodies, or error strings.

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/onebox-faas/faas/pkg/gitfetch"
	"github.com/onebox-faas/faas/pkg/githubd"
	"github.com/onebox-faas/faas/pkg/state"
)

// sourceRefTokenLookup is the slice of TokenCache the streamer
// needs. Mirrors the TokenCache.Token / Invalidate seam at
// pkg/githubd/tokencache.go:97 + :220. Defined as an interface
// so the test rig can substitute a recording fake without a
// real pgxpool or real age keypair. The production wiring
// (cmd/githubd/main.go) passes a thin adapter over
// *githubd.TokenCache (defined at the bottom of this file).
type sourceRefTokenLookup interface {
	Token(ctx context.Context, installationID int64) (string, error)
	Invalidate(installationID int64)
}

// sourceRefInstallsLookup is the slice of stateInstallsAdapter
// the streamer needs (same seam as installationSourceFetcher at
// cmd/githubd/source_fetcher.go:56). It returns the install row's
// sealed token envelope so the streamer can hand the raw
// installation_id to the TokenCache; the TokenCache itself owns
// the unseal + cache lifecycle.
type sourceRefInstallsLookup interface {
	ForAccount(ctx context.Context, accountID string) (state.GitHubInstall, error)
}

// sourceRefStreamer implements githubd.SourceRefStreamer against
// the durable install row + TokenCache + a stdlib http.Client.
// One instance per daemon; safe for concurrent use (http.Client
// is goroutine-safe and TokenCache is singleflight-guarded).
type sourceRefStreamer struct {
	installs sourceRefInstallsLookup
	tokens   sourceRefTokenLookup
	http     *http.Client
	log      *slog.Logger
	// maxArchiveBytes is the streaming cap applied via
	// io.LimitReader. 0 disables the cap (tests). Production
	// wiring sets it to the caller's per-plan SourceTarballMaxMB
	// so a Free plan can't balloon the apid spool dir.
	maxArchiveBytes int64
}

// newSourceRefStreamer wires the four dependencies. httpClient
// defaults to a 30s timeout if nil (matches pkg/gitfetch/http.go:74
// — a single archive fetch is bounded by network + extraction).
func newSourceRefStreamer(installs sourceRefInstallsLookup, tokens sourceRefTokenLookup, httpClient *http.Client, log *slog.Logger) *sourceRefStreamer {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	if log == nil {
		log = slog.Default()
	}
	return &sourceRefStreamer{
		installs:        installs,
		tokens:          tokens,
		http:            httpClient,
		log:             log,
		maxArchiveBytes: 0, // overridden by Stream caller per-plan
	}
}

// Stream resolves the install row, mints a token via TokenCache,
// GETs the codeload archive, and returns its body wrapped in
// io.LimitReader(maxArchiveBytes + 1, …). The caller MUST Close
// the returned ReadCloser.
//
// Returns:
//   - io.ReadCloser on success. Close() drains + closes the body.
//   - githubd.ErrNoBinding when state.ErrNotFound surfaces from
//     the install lookup (the apid handler turns this into 404
//     + code=github_install_not_found).
//   - gitfetch.ErrUnauthorized (wrapped) when a 401 survives one
//     cache-invalidate + retry. apid maps this to 503
//     code=source_ref_unavailable per ADR-092 §3.7.
//   - gitfetch.ErrNotFound (wrapped) for a 404 — the repo path
//     was rejected by codeload (rare; install token usually
//     covers any repo the install can see).
//   - gitfetch.ErrBadArchive (wrapped) for any other 4xx/5xx.
//   - any other error is wrapped with operation context for the
//     §12 dashboard's source-fetch error slice.
//
// maxArchiveBytes caps the streaming body. The LimitReader wraps
// maxArchiveBytes + 1 so the caller can detect "the cap was hit"
// by reading one extra byte and rolling the spool file back.
// capBytes <= 0 disables the cap (test-only path).
func (s *sourceRefStreamer) Stream(ctx context.Context, accountID string, installID int64, repoFullName, ref string, maxArchiveBytes int64) (io.ReadCloser, error) {
	// 1. Resolve the install row. ErrNoBinding on
	// state.ErrNotFound so the gRPC handler can return 404.
	inst, err := s.installs.ForAccount(ctx, accountID)
	if err != nil {
		if errors.Is(err, state.ErrNotFound) {
			return nil, githubd.ErrNoBinding
		}
		return nil, fmt.Errorf("githubd: source-ref streamer: resolve install: %w", err)
	}
	if inst.AccountID == "" || inst.InstallationID == 0 {
		// Defensive: same posture as installationSourceFetcher.
		return nil, githubd.ErrNoBinding
	}
	if inst.InstallationID != installID {
		// The apid handler passes the durable install_id it
		// pulled from state.GitHubInstallForAccount; mismatch
		// is a config bug, not a security one (no cross-account
		// take-over is possible because accountID gates the
		// row lookup). Refuse loudly so a stale install row
		// never re-uses the wrong install's token.
		s.log.Warn("githubd: source-ref streamer install id mismatch",
			"account_id", accountID,
			"expected_install_id", installID,
			"stored_install_id", inst.InstallationID,
			"repo", repoFullName)
		return nil, githubd.ErrNoBinding
	}

	// 2. Validate the inputs are well-formed up front so a
	// malformed value never reaches the HTTP layer. The same
	// gates pkg/gitfetch/http.go applies — duplicated here
	// rather than imported, because pkg/githubd does not import
	// pkg/gitfetch (the same decoupling as SourceTree vs
	// gitfetch.Tree). The shape MUST stay byte-for-byte
	// identical to pkg/gitfetch/http.go's isValidRepoPath /
	// isValidCommitSHA; the test suite pins both implementations
	// against the same fixture table.
	if !isValidSourceRefRepo(repoFullName) {
		return nil, fmt.Errorf("githubd: source-ref streamer: invalid repo %q: %w", repoFullName, gitfetch.ErrBadArchive)
	}
	if !isValidSourceRefRef(ref) {
		return nil, fmt.Errorf("githubd: source-ref streamer: invalid ref %q: %w", ref, gitfetch.ErrBadArchive)
	}

	// 3. GET the archive. TokenCache.Token handles
	// singleflight + 5-min proactive refresh. The first call
	// that races a cache miss blocks on api.github.com; the
	// rest piggy-back on the same call.
	token, err := s.tokens.Token(ctx, installID)
	if err != nil {
		return nil, fmt.Errorf("githubd: source-ref streamer: mint install token: %w", err)
	}

	body, err := s.fetchArchive(ctx, repoFullName, ref, token)
	if err == nil {
		return s.capAndWrap(body, maxArchiveBytes), nil
	}

	// 4. On 401 mid-stream: invalidate the cache entry and
	// retry once. The second 401 surfaces as ErrUnauthorized
	// (wrapped) so the apid handler can branch on
	// errors.Is(err, gitfetch.ErrUnauthorized).
	if errors.Is(err, gitfetch.ErrUnauthorized) {
		s.tokens.Invalidate(installID)
		token, terr := s.tokens.Token(ctx, installID)
		if terr != nil {
			return nil, fmt.Errorf("githubd: source-ref streamer: re-mint install token after 401: %w", terr)
		}
		body, err = s.fetchArchive(ctx, repoFullName, ref, token)
		if err == nil {
			return s.capAndWrap(body, maxArchiveBytes), nil
		}
	}
	return nil, err
}

// fetchArchive GETs https://codeload.github.com/<repo>/tar.gz/<ref>
// with a Bearer token and returns the raw response body on 2xx.
// Status mapping mirrors pkg/gitfetch/http.go:166-175 — 401/403
// → ErrUnauthorized, 404 → ErrNotFound, other 4xx/5xx →
// ErrBadArchive. The body is NOT closed here; capAndWrap (or
// the caller's defer) is responsible.
func (s *sourceRefStreamer) fetchArchive(ctx context.Context, repoFullName, ref, token string) (io.ReadCloser, error) {
	url := "https://codeload.github.com" + path.Join("/", repoFullName, "/tar.gz/", ref)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("githubd: source-ref streamer: new request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/x-gzip")
	req.Header.Set("User-Agent", "onebox-faas-githubd/1.0")

	resp, err := s.http.Do(req)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, fmt.Errorf("githubd: source-ref streamer: %w", err)
		}
		return nil, fmt.Errorf("githubd: source-ref streamer: http: %w", err)
	}

	switch {
	case resp.StatusCode == http.StatusUnauthorized, resp.StatusCode == http.StatusForbidden:
		// Drain + close before returning so the connection is
		// released back to the pool. Best-effort — the actual
		// auth problem is what we surface, not any drain error.
		_ = drainAndCloseSourceRef(resp.Body)
		return nil, fmt.Errorf("githubd: source-ref streamer: %d: %w", resp.StatusCode, gitfetch.ErrUnauthorized)
	case resp.StatusCode == http.StatusNotFound:
		_ = drainAndCloseSourceRef(resp.Body)
		return nil, fmt.Errorf("githubd: source-ref streamer: %d: %w", resp.StatusCode, gitfetch.ErrNotFound)
	case resp.StatusCode >= 400:
		_ = drainAndCloseSourceRef(resp.Body)
		return nil, fmt.Errorf("githubd: source-ref streamer: unexpected %d: %w", resp.StatusCode, gitfetch.ErrBadArchive)
	}
	return resp.Body, nil
}

// capAndWrap wraps body in io.LimitReader(maxArchiveBytes + 1, …)
// so the caller can detect "the cap was hit" by reading one extra
// byte and rolling the spool file back. capBytes <= 0 disables the
// cap (test-only path); the body is returned unwrapped.
//
// The wrapper is a small struct so the returned io.ReadCloser's
// Close() closes the underlying body (LimitReader alone doesn't
// implement io.Closer).
func (s *sourceRefStreamer) capAndWrap(body io.ReadCloser, maxArchiveBytes int64) io.ReadCloser {
	if maxArchiveBytes <= 0 {
		return body
	}
	return &sourceRefLimitedBody{
		ReadCloser: body,
		limit:      io.LimitReader(body, maxArchiveBytes+1),
	}
}

// sourceRefLimitedBody couples an io.LimitReader to its
// underlying body so Close() closes both. Read() pulls from the
// LimitReader; Close() drains + closes the underlying body
// (the same pattern pkg/gitfetch/http.go uses internally).
type sourceRefLimitedBody struct {
	io.ReadCloser
	limit io.Reader
}

func (b *sourceRefLimitedBody) Read(p []byte) (int, error) {
	return b.limit.Read(p)
}

func (b *sourceRefLimitedBody) Close() error {
	return drainAndCloseSourceRef(b.ReadCloser)
}

// drainAndCloseSourceRef mirrors pkg/gitfetch/http.go's
// drainAndClose helper — best-effort drain + close. Kept
// package-private to avoid an import cycle on the gitfetch
// helper.
func drainAndCloseSourceRef(rc io.ReadCloser) error {
	if rc == nil {
		return nil
	}
	// Best-effort drain; ignore errors so a slow / stuck body
	// can't wedge the caller.
	_, _ = io.Copy(io.Discard, io.LimitReader(rc, 1<<20))
	return rc.Close()
}

// isValidSourceRefRepo mirrors pkg/gitfetch/http.go's
// isValidRepoPath. Kept in sync because pkg/githubd does not
// import pkg/gitfetch (the decoupling source.go documents).
// Owner: when editing, edit BOTH sides and re-run the fixture
// table in source_ref_streamer_test.go.
func isValidSourceRefRepo(s string) bool {
	if s == "" || len(s) > 200 {
		return false
	}
	parts := strings.Split(s, "/")
	if len(parts) != 2 {
		return false
	}
	for _, seg := range parts {
		if seg == "" || len(seg) > 100 {
			return false
		}
		for _, r := range seg {
			switch {
			case r >= 'a' && r <= 'z':
			case r >= 'A' && r <= 'Z':
			case r >= '0' && r <= '9':
			case r == '-' || r == '.' || r == '_':
			default:
				return false
			}
		}
	}
	return true
}

// isValidSourceRefRef mirrors pkg/gitfetch/http.go's
// isValidCommitSHA. Accepts 40 lowercase-hex SHAs and 7+ short
// SHAs. Branches/tags are NOT matched here — the apid handler
// resolves them via api.github.com/repos/<repo>/commits/<ref>
// before calling Stream, so by the time we reach this function
// the ref is already a SHA.
func isValidSourceRefRef(s string) bool {
	if len(s) < 7 || len(s) > 40 {
		return false
	}
	for _, r := range s {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

// tokenCacheAdapter wires *githubd.TokenCache to the
// sourceRefTokenLookup interface. Production wiring passes
// &tokenCacheAdapter{cache: realCache}; tests pass a stub.
// The adapter is a single line of plumbing because the
// streamer file lives in cmd/githubd (not pkg/githubd), and
// importing the concrete TokenCache type would pull in the
// whole pkg/githubd surface. The interface seam keeps the
// streamer testable in isolation.
type tokenCacheAdapter struct {
	cache tokenCacheBackend
}

// tokenCacheBackend is the slice of *githubd.TokenCache the
// streamer needs. Defined here so the test rig can stub
// without a real cache; production wiring passes the real
// *githubd.TokenCache (it satisfies both methods).
type tokenCacheBackend interface {
	Token(ctx context.Context, installationID int64) (string, error)
	Invalidate(installationID int64)
}

func (a *tokenCacheAdapter) Token(ctx context.Context, installationID int64) (string, error) {
	return a.cache.Token(ctx, installationID)
}

func (a *tokenCacheAdapter) Invalidate(installationID int64) {
	a.cache.Invalidate(installationID)
}

// Compile-time guards.
var (
	_ sourceRefInstallsLookup = (*stateInstallsAdapter)(nil)
	_ sourceRefTokenLookup    = (*tokenCacheAdapter)(nil)
	_ githubd.SourceRefStreamer = (*sourceRefStreamer)(nil)
)