package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/onebox-faas/faas/pkg/session"
	"github.com/onebox-faas/faas/pkg/state"
)

// fakeGithubdClient is a recording GithubdClient that the OAuth
// callback tests can program with a canned (verified, account_login,
// default_branch) response. It also records the
// (installation_id, expected_login) tuple so we can assert the
// handler passed the session's github_login through to the verify
// call — i.e. the §11 ownership proof was actually checked.
type fakeGithubdClient struct {
	GithubdClient

	verified      bool
	accountLogin  string
	defaultBranch string
	verifyErr     error

	gotInstallID     int64
	gotExpectedLogin string
}

func (f *fakeGithubdClient) VerifyInstallation(_ context.Context, installID int64, expectedLogin string) (bool, string, string, error) {
	f.gotInstallID = installID
	f.gotExpectedLogin = expectedLogin
	return f.verified, f.accountLogin, f.defaultBranch, f.verifyErr
}

// newOAuthTestServer seeds an account into MemStore + mints a
// session cookie, then returns the apid handler with a programmable
// GithubdClient so the callback tests can flip verified / branch /
// error without touching a real socket.
//
// PR-B: callers that want the §11 ownership proof path should use
// wrapWithGithubLogin to re-mint the cookie with github_login
// populated — the plain Issue path leaves it empty.
func newOAuthTestServer(t *testing.T, gh GithubdClient) (http.Handler, *http.Cookie) {
	h, c, _ := newOAuthTestServerWithLogin(t, gh, "")
	return h, c
}

// newOAuthTestServerWithLogin is the PR-B variant: it seeds an
// account whose email is <login>@example.com so the dashboard
// tests can assert per-login redirects without touching a real
// GitHub OAuth exchange. Returns (handler, cookie, manager) so
// callers can re-mint the cookie via SealGithubLogin — each
// EphemeralManager signs with a per-instance key, so a cookie
// minted by a different manager won't Verify.
func newOAuthTestServerWithLogin(t *testing.T, gh GithubdClient, login string) (http.Handler, *http.Cookie, *session.Manager) {
	t.Helper()
	store := state.NewMemStore()
	email := "alice@example.com"
	if login != "" {
		email = login + "@example.com"
	}
	acct, err := store.CreateAccount(t.Context(), email, "free")
	if err != nil {
		t.Fatalf("seed account: %v", err)
	}
	mgr, err := session.NewEphemeralManager(sessionCookieLifetime)
	if err != nil {
		t.Fatalf("session manager: %v", err)
	}
	cookie, err := mgr.Issue(acct.ID)
	if err != nil {
		t.Fatalf("issue session: %v", err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := newServerWithDeps(store, log, "example.com", noopNotifier{}, "", noopMailer{}, gh, mgr, nil, 15*60_000_000_000, "")
	return srv.handler(), &http.Cookie{Name: sessionCookie, Value: cookie}, mgr
}

// wrapWithGithubLogin re-mints the cookie so the session envelope
// carries github_login=<login>. The §11 ownership proof in
// handlers_oauth.go reads this field from the cookie via
// s.sessions.Verify, so any test exercising the verified / forged
// path needs the envelope stamped. Mirrors what handlers_github.go
// does after a successful /v1/auth/github.
//
// mgr must be the SAME session.Manager that minted the original
// cookie (EphemeralManager signs with a per-instance key).
func wrapWithGithubLogin(t *testing.T, h http.Handler, c *http.Cookie, login string, mgr *session.Manager) (http.Handler, *http.Cookie) {
	t.Helper()
	if login == "" {
		return h, c
	}
	env, err := mgr.Verify(c.Value)
	if err != nil {
		t.Fatalf("verify existing cookie: %v", err)
	}
	cookie, err := mgr.SealGithubLogin(env.AccountID, login)
	if err != nil {
		t.Fatalf("seal github login: %v", err)
	}
	return h, &http.Cookie{Name: sessionCookie, Value: cookie}
}

// TestOAuthCallback_VerifiedRedirectsToBindPicker is the happy path:
// real install + matching account_login → /dashboard/apps/new?install=N&default_branch=B.
//
// PR-B: the session cookie carries github_login (set by SealGithubLogin
// after /v1/auth/github). The handler pulls that login out of the
// envelope and passes it as expectedLogin to githubd.Verified = true
// requires account_login == expected_login (asserted via the fake).
func TestOAuthCallback_VerifiedRedirectsToBindPicker(t *testing.T) {
	const installID = 4242
	gh := &fakeGithubdClient{verified: true, accountLogin: "alice", defaultBranch: "main"}
	srv, cookie, mgr := newOAuthTestServerWithLogin(t, gh, "alice")
	srv, cookie = wrapWithGithubLogin(t, srv, cookie, "alice", mgr)

	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/oauth/callback?installation_id="+strconv.FormatInt(installID, 10)+"&setup_action=install", nil)
	r.AddCookie(cookie)
	srv.ServeHTTP(rec, r)

	if rec.Code != http.StatusFound {
		t.Fatalf("code = %d, want 302\nbody = %s", rec.Code, rec.Body.String())
	}
	loc := rec.Header().Get("Location")
	// url.Values.Encode sorts keys alphabetically, so default_branch
	// appears before install in the canonical form.
	want := "/dashboard/apps/new?default_branch=main&install=4242"
	if loc != want {
		t.Errorf("redirect = %q, want %q", loc, want)
	}
	if gh.gotInstallID != installID {
		t.Errorf("verify install_id = %d, want %d", gh.gotInstallID, installID)
	}
	if gh.gotExpectedLogin != "alice" {
		t.Errorf("verify expected_login = %q, want %q", gh.gotExpectedLogin, "alice")
	}
}

// TestOAuthCallback_ForgedInstallRedirectsToAccountPage asserts the
// §11 fail-closed behavior: a forged installation_id (verified=false,
// accountLogin empty — install not on api.github.com) must NOT
// persist anything and must send the user to a banner page, not a
// 5xx page. This is the regression test for review finding #2.
//
// PR-B: requires the session to carry github_login so the
// §11 ownership proof has a value to compare against; otherwise
// the handler takes the unauthenticated branch
// (see TestOAuthCallback_UnauthenticatedInstallRefused).
func TestOAuthCallback_ForgedInstallRedirectsToAccountPage(t *testing.T) {
	gh := &fakeGithubdClient{verified: false}
	srv, cookie, mgr := newOAuthTestServerWithLogin(t, gh, "alice")
	srv, cookie = wrapWithGithubLogin(t, srv, cookie, "alice", mgr)

	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/oauth/callback?installation_id=9999", nil)
	r.AddCookie(cookie)
	srv.ServeHTTP(rec, r)

	if rec.Code != http.StatusFound {
		t.Fatalf("code = %d, want 302", rec.Code)
	}
	loc := rec.Header().Get("Location")
	if loc != "/dashboard/account?github=forged" {
		t.Errorf("redirect = %q, want /dashboard/account?github=forged", loc)
	}
}

// TestOAuthCallback_GithubdTransportErrorReturns502 covers the
// "couldn't reach GitHub" path: a non-nil err from VerifyInstallation
// becomes a 502 problem (not a redirect, not a 500) so the dashboard
// renders a retry banner.
//
// PR-B: requires the session to carry github_login so the §11
// ownership proof passes before githubd is called.
func TestOAuthCallback_GithubdTransportErrorReturns502(t *testing.T) {
	gh := &fakeGithubdClient{verifyErr: errors.New("dial tcp: connection refused")}
	srv, cookie, mgr := newOAuthTestServerWithLogin(t, gh, "alice")
	srv, cookie = wrapWithGithubLogin(t, srv, cookie, "alice", mgr)

	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/oauth/callback?installation_id=1", nil)
	r.AddCookie(cookie)
	srv.ServeHTTP(rec, r)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("code = %d, want 502\nbody = %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/problem+json" {
		t.Errorf("content-type = %q, want application/problem+json", ct)
	}
}

// TestOAuthCallback_RejectsMissingInstallID returns 400 — a forged
// callback without installation_id is malformed, not a githubd error.
func TestOAuthCallback_RejectsMissingInstallID(t *testing.T) {
	gh := &fakeGithubdClient{verified: true}
	srv, cookie := newOAuthTestServer(t, gh)

	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/oauth/callback", nil)
	r.AddCookie(cookie)
	srv.ServeHTTP(rec, r)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code = %d, want 400", rec.Code)
	}
}

// TestOAuthCallback_RejectsNonIntegerInstallID returns 400 when the
// installation_id is unparseable (the GitHub install URL is built
// from a numeric id; anything else is hostile input).
func TestOAuthCallback_RejectsNonIntegerInstallID(t *testing.T) {
	gh := &fakeGithubdClient{verified: true}
	srv, cookie := newOAuthTestServer(t, gh)

	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/oauth/callback?installation_id=not-a-number", nil)
	r.AddCookie(cookie)
	srv.ServeHTTP(rec, r)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code = %d, want 400", rec.Code)
	}
	if gh.gotInstallID != 0 {
		t.Errorf("verify should NOT be called for malformed input; got installID=%d", gh.gotInstallID)
	}
}

// TestOAuthCallback_RejectsZeroInstallID asserts the >0 check on
// installationID — "0" and negative numbers are not valid GitHub
// installation IDs and would 404 from api.github.com anyway, but
// catching them client-side is cheaper and avoids a wasted call.
func TestOAuthCallback_RejectsZeroInstallID(t *testing.T) {
	gh := &fakeGithubdClient{verified: true}
	srv, cookie := newOAuthTestServer(t, gh)

	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/oauth/callback?installation_id=0", nil)
	r.AddCookie(cookie)
	srv.ServeHTTP(rec, r)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code = %d, want 400", rec.Code)
	}
	if gh.gotInstallID != 0 {
		t.Errorf("verify should NOT be called for install_id=0; got %d", gh.gotInstallID)
	}
}

// TestOAuthCallback_RequiresSessionAuth asserts the /oauth/callback
// route is gated by sessionAuth — an unauthenticated request is
// redirected to /login rather than letting an anonymous attacker
// forge a binding against any account.
func TestOAuthCallback_RequiresSessionAuth(t *testing.T) {
	gh := &fakeGithubdClient{verified: true}
	store := state.NewMemStore()
	mgr, err := session.NewEphemeralManager(sessionCookieLifetime)
	if err != nil {
		t.Fatalf("session manager: %v", err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := newServerWithDeps(store, log, "example.com", noopNotifier{}, "", noopMailer{}, gh, mgr, nil, 15*60_000_000_000, "").handler()

	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/oauth/callback?installation_id=1", nil)
	// no cookie
	srv.ServeHTTP(rec, r)

	if rec.Code != http.StatusFound {
		t.Fatalf("code = %d, want 302 (redirect to /login)", rec.Code)
	}
	loc := rec.Header().Get("Location")
	if loc != "/login?next=/oauth/callback" {
		t.Errorf("redirect = %q, want /login?next=/oauth/callback", loc)
	}
	if gh.gotInstallID != 0 {
		t.Errorf("verify should NOT be called for unauthed request; got %d", gh.gotInstallID)
	}
}

// TestOAuthCallback_RejectsForeignInstall is the §11 ownership
// proof (PR-B): when the install exists but belongs to a different
// GitHub user than the dashboard session claims to be, the handler
// returns 403 forged. Without this check (the pre-PR-B behaviour),
// any logged-in FaaS account could adopt any installation_id by
// guessing — the §11 anti-takeover invariant is what this closes.
func TestOAuthCallback_RejectsForeignInstall(t *testing.T) {
	gh := &fakeGithubdClient{verified: false, accountLogin: "bob"}
	srv, cookie, mgr := newOAuthTestServerWithLogin(t, gh, "alice")
	srv, cookie = wrapWithGithubLogin(t, srv, cookie, "alice", mgr)

	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/oauth/callback?installation_id=42", nil)
	r.AddCookie(cookie)
	srv.ServeHTTP(rec, r)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("code = %d, want 403\nbody = %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/problem+json" {
		t.Errorf("content-type = %q, want application/problem+json", ct)
	}
	if gh.gotExpectedLogin != "alice" {
		t.Errorf("verify expected_login = %q, want alice", gh.gotExpectedLogin)
	}
}

// TestOAuthCallback_AcceptsOwnInstall is the §11 positive path: the
// install's account.login matches the session's github_login, so
// the handler falls through to the bind picker redirect. Without
// this, a regression in handlers_oauth.go that always returns 403
// would pass the foreign-install test but block legitimate flows.
func TestOAuthCallback_AcceptsOwnInstall(t *testing.T) {
	gh := &fakeGithubdClient{verified: true, accountLogin: "alice", defaultBranch: "main"}
	srv, cookie, mgr := newOAuthTestServerWithLogin(t, gh, "alice")
	srv, cookie = wrapWithGithubLogin(t, srv, cookie, "alice", mgr)

	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/oauth/callback?installation_id=42", nil)
	r.AddCookie(cookie)
	srv.ServeHTTP(rec, r)

	if rec.Code != http.StatusFound {
		t.Fatalf("code = %d, want 302\nbody = %s", rec.Code, rec.Body.String())
	}
	loc := rec.Header().Get("Location")
	want := "/dashboard/apps/new?default_branch=main&install=42"
	if loc != want {
		t.Errorf("redirect = %q, want %q", loc, want)
	}
}

// TestOAuthCallback_UnauthenticatedInstallRefused is the §11
// third path: the user has a FaaS session but never completed
// /v1/auth/github. expectedLogin is empty, so we cannot prove the
// install belongs to them — refuse with 302 to
// /dashboard/account?github=unauthenticated rather than silently
// binding the install under an unverified identity.
//
// This is the regression test for the wire-back-compat worry from
// the plan: pre-PR-B cookies minted without github_login must NOT
// get a free pass.
func TestOAuthCallback_UnauthenticatedInstallRefused(t *testing.T) {
	gh := &fakeGithubdClient{verified: true, accountLogin: "alice"}
	srv, cookie, _ := newOAuthTestServerWithLogin(t, gh, "alice")
	// NB: no wrapWithGithubLogin — cookie has no github_login.

	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/oauth/callback?installation_id=42", nil)
	r.AddCookie(cookie)
	srv.ServeHTTP(rec, r)

	if rec.Code != http.StatusFound {
		t.Fatalf("code = %d, want 302\nbody = %s", rec.Code, rec.Body.String())
	}
	loc := rec.Header().Get("Location")
	if loc != "/dashboard/account?github=unauthenticated" {
		t.Errorf("redirect = %q, want /dashboard/account?github=unauthenticated", loc)
	}
	if gh.gotInstallID != 0 {
		t.Errorf("verify should NOT be called when github_login is empty; got installID=%d", gh.gotInstallID)
	}
}
