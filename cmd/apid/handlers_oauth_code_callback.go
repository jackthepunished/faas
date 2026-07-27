// PR-C: GET /oauth/code-callback handler — closes the user-to-server
// OAuth handshake.
//
// Why a second /oauth/code-callback route (sibling of /oauth/callback):
//
//	/oauth/callback (handlers_oauth.go) handles the GitHub App INSTALL
//	callback, where GitHub redirects to
//	  /oauth/callback?installation_id=N&setup_action=install
//	after the customer clicks "Install" on github.com. That branch is
//	"trust on first contact" — apid verifies the install exists and
//	belongs to the dashboard user's GitHub identity, then redirects to
//	the bind picker (/dashboard/apps/new).
//
//	/oauth/code-callback (this file) handles the GitHub App USER OAuth
//	callback, where the dashboard's "Connect GitHub" button sends the
//	user to
//	  https://github.com/login/oauth/authorize?client_id=…&state=…
//	and GitHub redirects back with
//	  /oauth/code-callback?code=…&state=…
//	That branch is the user-to-server OAuth handshake: githubd
//	exchanges the code for an install token, seals it under the host
//	age key, persists to the durable github_installations table, and
//	emits the auth.install.token_sealed audit event. After that, the
//	/v1/install/* and /v1/apps/{slug}/install/bind routes succeed
//	across githubd restarts.
//
// Two callbacks, two URLs, two CSRF tokens — both scoped narrowly
// so a stale tab can't replay one against the other.
//
// CSRF posture: the state token here is generated when the dashboard
// "Connect GitHub" button is rendered and stored in a short-lived
// 5-minute cookie scoped to /oauth/code-callback. The handler reads
// the cookie + the query string, constant-time compares, and
// rejects mismatch (with an audit event). The AEAD-sealed session
// cookie is NOT touched — adding a ConnectState field to the
// envelope would require rotating session cookies for every other
// auth method; this cookie approach keeps the change isolated to
// PR-C's surface.
//
// Failure surfaces:
//   - missing code or state query param   → 400 problem
//   - missing or mismatched CSRF cookie   → 403 csrf_rejected + audit
//   - githubd.ExchangeOAuthCode errs      → 502 problem
//   - success                             → 302 to
//     /dashboard/account?github=connected&install=<id>&default_branch=<branch>
package main

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"time"

	"github.com/onebox-faas/faas/pkg/api"
)

const (
	// oauthCodeCallbackPath is the GitHub App user-to-server OAuth
	// callback URL. Sibling of oauthCallbackPath in handlers_oauth.go.
	oauthCodeCallbackPath = "/oauth/code-callback"

	// oauthCodeStateCookie is the CSRF state cookie for the
	// /oauth/code-callback handler. Scoped narrowly to
	// /oauth/code-callback so it cannot be replayed against the
	// /oauth/callback handler (PR-B's existing flow) or the
	// /v1/auth/github/* login flow.
	oauthCodeStateCookie = "faas_oauth_code_state"

	// oauthCodeStateTTL is the cookie + state token lifetime. The
	// GitHub OAuth round-trip is typically well under 60 s; 5
	// minutes is the upper bound for a user completing the dialog.
	oauthCodeStateTTL = 5 * time.Minute
)

// issueOAuthCodeState mints a 16-byte CSRF state token and writes it
// to a short-lived cookie scoped to /oauth/code-callback. The
// dashboard's "Connect GitHub" button renderer calls this before
// building the GitHub authorize URL. Random source: crypto/rand
// (not math/rand) — the state is the only thing standing between
// an attacker and a forged user-to-server token exchange.
func (s *server) issueOAuthCodeState(w http.ResponseWriter, r *http.Request) (string, error) {
	tokenBytes := make([]byte, 16)
	if _, err := rand.Read(tokenBytes); err != nil {
		return "", err
	}
	stateToken := hex.EncodeToString(tokenBytes)
	http.SetCookie(w, &http.Cookie{
		Name:     oauthCodeStateCookie,
		Value:    stateToken,
		Path:     oauthCodeCallbackPath,
		HttpOnly: true,
		Secure:   r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == schemeHTTPS,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(oauthCodeStateTTL.Seconds()),
	})
	return stateToken, nil
}

// renderOAuthCodeCallback is the GET /oauth/code-callback handler.
// Mounted in server.go behind sessionAuth so the request already
// carries an authenticated account in the context — the OAuth
// handshake is account-scoped (github_installations.account_id is
// the FK).
func (s *server) renderOAuthCodeCallback(w http.ResponseWriter, r *http.Request) {
	const op = "renderOAuthCodeCallback"
	log := s.log.With("op", op)

	acct, ok := AccountFrom(r.Context())
	if !ok {
		// sessionAuth would have redirected before this; defend
		// against a future refactor that drops the middleware.
		api.WriteProblem(w, api.NewProblem(http.StatusUnauthorized, api.CodeUnauthorized,
			"Unauthorized", "sign in to connect GitHub"))
		return
	}

	// Read query parameters. We order state check BEFORE code
	// check so a malformed callback can't probe the githubd gRPC
	// surface with bad inputs — the CSRF gate runs first.
	queryState := r.URL.Query().Get("state")
	code := r.URL.Query().Get("code")
	if queryState == "" {
		api.WriteProblem(w, api.NewProblem(http.StatusBadRequest, "invalid_request",
			"Missing state", "/oauth/code-callback requires a state query parameter"))
		return
	}
	if code == "" {
		api.WriteProblem(w, api.NewProblem(http.StatusBadRequest, "invalid_request",
			"Missing code", "/oauth/code-callback requires a code query parameter"))
		return
	}

	// CSRF check via the narrowly-scoped state cookie.
	stateCookie, err := r.Cookie(oauthCodeStateCookie)
	if err != nil || stateCookie.Value == "" {
		log.Warn("oauth code callback: missing CSRF state cookie",
			"account_id", acct.ID)
		acctID := acct.ID
		s.audit.Emit(r.Context(), "auth.install.csrf_rejected", &acctID, map[string]any{
			"reason": "missing_state_cookie",
		})
		api.WriteProblem(w, api.NewProblem(http.StatusForbidden, "csrf_missing",
			"Missing CSRF state", "the Connect GitHub flow requires a recent 'Connect GitHub' click — restart the flow"))
		return
	}
	// Constant-time compare against the query string. The cookie
	// and query are both 32 hex chars (16 bytes random), so the
	// only sane attack is replay with a leaked cookie — the
	// short TTL + narrow Path scope is what makes that tractable.
	if subtle.ConstantTimeCompare([]byte(stateCookie.Value), []byte(queryState)) != 1 {
		log.Warn("oauth code callback: state mismatch",
			"account_id", acct.ID,
			// CodeQL go/log-injection (CWE-117): both halves are
			// already hex-encoded and bounded to 32 chars by the
			// generator, but defense-in-depth keeps the same
			// sanitiser shape every other audit log in this file
			// uses (MEMORY.md/codeql-go-log-injection-sanitisers).
			"cookie_len", len(stateCookie.Value),
			"query_len", len(queryState))
		acctID := acct.ID
		s.audit.Emit(r.Context(), "auth.install.csrf_rejected", &acctID, map[string]any{
			"reason": "state_mismatch",
		})
		api.WriteProblem(w, api.NewProblem(http.StatusForbidden, "csrf_mismatch",
			"CSRF mismatch", "the OAuth state token does not match the cookie — restart the Connect GitHub flow"))
		return
	}

	// Clear the state cookie immediately. It's single-use: a
	// successful exchange shouldn't leave the token lying around
	// for a redirect-replay to use.
	http.SetCookie(w, &http.Cookie{
		Name:     oauthCodeStateCookie,
		Value:    "",
		Path:     oauthCodeCallbackPath,
		HttpOnly: true,
		Secure:   r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == schemeHTTPS,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})

	// Hand the code to githubd. githubd mints the install token,
	// seals under the host age key, persists to github_installations,
	// and emits auth.install.token_sealed via its audit callback. The
	// apid-side audit here fires only on the githubd error path; the
	// success path's audit row is the githubd one (sealed-token
	// permanence is the audit-worthy event).
	installID, defaultBranch, err := s.githubd.ExchangeOAuthCode(r.Context(), acct.ID, code, queryState)
	if err != nil {
		log.Error("oauth code callback: githubd ExchangeOAuthCode failed",
			"account_id", acct.ID,
			"err", err)
		acctID := acct.ID
		s.audit.Emit(r.Context(), "auth.install.token_exchange_failed", &acctID, map[string]any{
			"githubd_err": stripLogCRLF(err.Error()),
		})
		api.WriteProblem(w, api.NewProblem(http.StatusBadGateway, "github_unreachable",
			"Could not reach GitHub", "retry the connect flow in a minute: https://docs/connect-github"))
		return
	}

	// PR-C also adds defaultBranch to the redirect query — the
	// dashboard success page uses it to highlight "we picked
	// <branch> as your default" and prime the bind picker with the
	// right branch. Empty is fine (some installs use a non-default
	// branch and the user picks manually).
	q := url.Values{}
	q.Set("github", "connected")
	if installID != "" {
		q.Set("install", installID)
	}
	if defaultBranch != "" {
		q.Set("default_branch", defaultBranch)
	}
	q.Set("connected_at", strconv.FormatInt(time.Now().Unix(), 10))
	http.Redirect(w, r, "/dashboard/account?"+q.Encode(), http.StatusFound)
}

// startConnectGitHub (POST /dashboard/install/connect) is the
// dashboard "Connect GitHub" button click handler. It mints a
// narrow CSRF state cookie scoped to /oauth/code-callback and
// 302s the browser to GitHub's authorize URL.
//
// POST-only (not GET) so an opportunistic <img src=…> cannot
// mint a state cookie and trip a CSRF path. Same posture as
// handlers_github.go:81 (renderGitHubAuthRedirect is the same
// shape, but the dashboard chrome triggers it on click; here we
// POST because the click is rendered from a form).
//
// We don't read FAAS_GITHUB_APP_CLIENT_ID env directly here —
// GitHub App OAuth credentials are operator-controlled and
// /oauth/callback's renderOAuthCallback handler reads them at
// exchange time. This handler builds the authorize URL from
// the same env var the githubd side uses (FAAS_GITHUB_APP_CLIENT_ID)
// so the two URLs agree.
func (s *server) startConnectGitHub(w http.ResponseWriter, r *http.Request) {
	const op = "startConnectGitHub"
	log := s.log.With("op", op)

	if _, ok := AccountFrom(r.Context()); !ok {
		api.WriteProblem(w, api.NewProblem(http.StatusUnauthorized, api.CodeUnauthorized,
			"Unauthorized", "sign in to connect GitHub"))
		return
	}

	clientID := os.Getenv("FAAS_GITHUB_APP_CLIENT_ID")
	if clientID == "" {
		log.Error("FAAS_GITHUB_APP_CLIENT_ID not configured")
		api.WriteProblem(w, api.NewProblem(http.StatusInternalServerError, "github_oauth_misconfigured",
			"OAuth Misconfigured", "FAAS_GITHUB_APP_CLIENT_ID environment variable is required"))
		return
	}

	stateToken, err := s.issueOAuthCodeState(w, r)
	if err != nil {
		log.Error("issue oauth code state", "err", err)
		api.WriteProblem(w, api.NewProblem(http.StatusInternalServerError, "internal_error",
			"Internal Error", "failed to generate CSRF state"))
		return
	}

	redirectURI := os.Getenv("FAAS_GITHUB_APP_REDIRECT_URI")
	if redirectURI == "" {
		host := r.Host
		scheme := schemeHTTP
		if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == schemeHTTPS {
			scheme = schemeHTTPS
		}
		redirectURI = scheme + "://" + host + oauthCodeCallbackPath
	}

	// GitHub App OAuth authorize URL.
	// https://docs.github.com/en/apps/building-github-apps/identifying-users-for-github-apps
	u := "https://github.com/login/oauth/authorize?client_id=" + url.QueryEscape(clientID) +
		"&redirect_uri=" + url.QueryEscape(redirectURI) +
		"&state=" + url.QueryEscape(stateToken) +
		"&scope=" // Empty scope: the user authorizes the App's own resources.
	http.Redirect(w, r, u, http.StatusFound)
}

// stripLogCRLF / stripLogInt64 are defined in handlers_install_github.go
// (same package). Both files share the helpers automatically; see that
// file for the rationale on why CodeQL go/log-injection still flags
// seemingly-safe values — defense in depth.

// _ keeps slog import live for future structured logging.
var _ = slog.Default
