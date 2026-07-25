package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/onebox-faas/faas/pkg/state"
)

// fakeGitHubAPI impersonates api.github.com + github.com for the OAuth
// dashboard login flow. The three endpoints the handler hits are:
//
//	POST https://github.com/login/oauth/access_token
//	GET  https://api.github.com/user
//	GET  https://api.github.com/user/emails
//
// The RoundTripper below rewrites those three URLs to the httptest
// server; everything else passes through DefaultTransport so the rest
// of the test process keeps working.
type fakeGitHubAPI struct {
	server *httptest.Server

	// token body + status returned for the access_token exchange.
	tokenStatus int
	tokenBody   []byte

	// user status + body returned for /user.
	userStatus int
	userBody   []byte

	// emails status + body returned for /user/emails.
	emailsStatus int
	emailsBody   []byte

	// acceptSeen counts how many requests to access_token carried
	// Accept: application/json. Used by the Accept-header regression
	// (case #13).
	acceptSeen *atomic.Int32
}

func newFakeGitHubAPI(t *testing.T) *fakeGitHubAPI {
	t.Helper()
	f := &fakeGitHubAPI{
		tokenStatus:  http.StatusOK,
		tokenBody:    []byte(`{"access_token":"fake_gh_token","scope":"read:user,user:email","token_type":"bearer"}`),
		userStatus:   http.StatusOK,
		emailsStatus: http.StatusOK,
		acceptSeen:   &atomic.Int32{},
	}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/login/oauth/access_token":
			if strings.EqualFold(r.Header.Get("Accept"), "application/json") {
				f.acceptSeen.Add(1)
			}
			w.WriteHeader(f.tokenStatus)
			_, _ = w.Write(f.tokenBody)
		case r.URL.Path == "/user" && strings.HasPrefix(r.Header.Get("Authorization"), "Bearer "):
			w.WriteHeader(f.userStatus)
			_, _ = w.Write(f.userBody)
		case r.URL.Path == "/user/emails" && strings.HasPrefix(r.Header.Get("Authorization"), "Bearer "):
			w.WriteHeader(f.emailsStatus)
			_, _ = w.Write(f.emailsBody)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.server.Close)
	return f
}

// rewriteTo is a RoundTripper that redirects only the three GitHub
// outbound URLs to the impersonator. Other URLs pass through to the
// wrapped RoundTripper.
type rewriteTo struct {
	origin *url.URL
	next   http.RoundTripper
}

func (r *rewriteTo) RoundTrip(req *http.Request) (*http.Response, error) {
	switch {
	case req.URL.Host == "github.com" && req.URL.Path == "/login/oauth/access_token":
		req.URL.Scheme = r.origin.Scheme
		req.URL.Host = r.origin.Host
		req.URL.Path = "/login/oauth/access_token"
	case req.URL.Host == "api.github.com" && req.URL.Path == "/user":
		req.URL.Scheme = r.origin.Scheme
		req.URL.Host = r.origin.Host
		req.URL.Path = "/user"
	case req.URL.Host == "api.github.com" && req.URL.Path == "/user/emails":
		req.URL.Scheme = r.origin.Scheme
		req.URL.Host = r.origin.Host
		req.URL.Path = "/user/emails"
	default:
		return r.next.RoundTrip(req)
	}
	return r.next.RoundTrip(req)
}

// withGitHubTransport swaps http.DefaultClient.Transport for a
// GitHub-rewriter for the duration of the test. Returns a cleanup the
// caller MUST defer (or register via t.Cleanup).
func withGitHubTransport(t *testing.T, f *fakeGitHubAPI) func() {
	t.Helper()
	origin, err := url.Parse(f.server.URL)
	if err != nil {
		t.Fatalf("parse impersonator URL: %v", err)
	}
	prev := http.DefaultClient.Transport
	if prev == nil {
		prev = http.DefaultTransport
	}
	http.DefaultClient.Transport = &rewriteTo{origin: origin, next: prev}
	return func() { http.DefaultClient.Transport = prev }
}

func newGitHubTestServer(t *testing.T) http.Handler {
	t.Helper()
	store := state.NewMemStore()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	return newServer(store, log, "example.com", noopNotifier{}).handler()
}

// --- 1. Consent redirect -------------------------------------------------

// TestGitHubAuthRedirect drives GET /v1/auth/github with
// GITHUB_CLIENT_ID set. Expects a 302 to github.com/login/oauth/authorize
// carrying the requested scope, plus a faas_github_state CSRF cookie
// scoped to /v1/auth/github/callback.
func TestGitHubAuthRedirect(t *testing.T) {
	t.Setenv("GITHUB_CLIENT_ID", "test_gh_client_id")
	h := newGitHubTestServer(t)

	req := httptest.NewRequest(http.MethodGet, "/v1/auth/github", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusFound {
		t.Fatalf("expected 302 Found, got %d; body=%s", rec.Code, rec.Body.String())
	}
	loc := rec.Header().Get("Location")
	if !strings.HasPrefix(loc, "https://github.com/login/oauth/authorize?") {
		t.Errorf("Location = %q, want GitHub consent URL", loc)
	}
	q, err := url.ParseQuery(loc[strings.Index(loc, "?")+1:])
	if err != nil {
		t.Fatalf("parse Location query: %v", err)
	}
	if got := q.Get("client_id"); got != "test_gh_client_id" {
		t.Errorf("client_id = %q, want test_gh_client_id", got)
	}
	if got := q.Get("scope"); got != "read:user user:email" {
		t.Errorf("scope = %q, want read:user user:email", got)
	}
	if q.Get("allow_signup") != "true" {
		t.Errorf("allow_signup = %q, want true", q.Get("allow_signup"))
	}
	stateToken := q.Get("state")
	if stateToken == "" {
		t.Fatalf("expected non-empty state query param")
	}
	var seenState bool
	for _, c := range rec.Result().Cookies() {
		if c.Name == githubAuthStateCookie {
			seenState = true
			if c.Value != stateToken {
				t.Errorf("state cookie = %q, want %q", c.Value, stateToken)
			}
			if c.Path != githubCallbackPath {
				t.Errorf("state cookie path = %q, want %q", c.Path, githubCallbackPath)
			}
		}
	}
	if !seenState {
		t.Errorf("expected faas_github_state CSRF cookie to be set")
	}
}

// TestGitHubAuthRedirect_MissingClientID asserts the consent-redirect
// path is fail-closed when GITHUB_CLIENT_ID is unset. 500
// github_oauth_misconfigured so operators see the misconfiguration in
// logs immediately rather than a misleading CSRF flow.
func TestGitHubAuthRedirect_MissingClientID(t *testing.T) {
	t.Setenv("GITHUB_CLIENT_ID", "")
	h := newGitHubTestServer(t)

	req := httptest.NewRequest(http.MethodGet, "/v1/auth/github", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", rec.Code)
	}
	var p map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &p)
	if p["code"] != "github_oauth_misconfigured" {
		t.Errorf("code = %v, want github_oauth_misconfigured", p["code"])
	}
}

// --- 2. Callback CSRF / input validation --------------------------------

func TestGitHubAuthCallback_CSRFMismatch(t *testing.T) {
	h := newGitHubTestServer(t)
	// Cookie present (anchor) but query state doesn't match — that's
	// the csrf_mismatch branch. Without the cookie, the handler
	// short-circuits at invalid_state (covered by
	// TestGitHubAuthCallback_MissingStateCookie below).
	req := httptest.NewRequest(http.MethodGet, "/v1/auth/github/callback?state=invalid_state&code=test_code", nil)
	req.AddCookie(&http.Cookie{Name: githubAuthStateCookie, Value: "expected_state"})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 CSRF mismatch, got %d; body=%s", rec.Code, rec.Body.String())
	}
	var p map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &p)
	if p["code"] != "csrf_mismatch" {
		t.Errorf("code = %v, want csrf_mismatch", p["code"])
	}
}

// TestGitHubAuthCallback_MissingStateCookie sends a callback with a
// state query but no faas_github_state cookie at all. Must 400
// invalid_state — the cookie isn't optional because the entire flow
// exists to anchor the state cookie to the browser.
func TestGitHubAuthCallback_MissingStateCookie(t *testing.T) {
	h := newGitHubTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/v1/auth/github/callback?state=any&code=any", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
	var p map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &p)
	if p["code"] != "invalid_state" {
		t.Errorf("code = %v, want invalid_state", p["code"])
	}
}

func TestGitHubAuthCallback_MissingCode(t *testing.T) {
	h := newGitHubTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/v1/auth/github/callback?state=any", nil)
	// Cookie + matching query state is the precondition for the
	// handler to reach the code-presence check. Without these, the
	// handler short-circuits at invalid_state (covered above).
	req.AddCookie(&http.Cookie{Name: githubAuthStateCookie, Value: "any"})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 missing_code, got %d", rec.Code)
	}
	var p map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &p)
	if p["code"] != "missing_code" {
		t.Errorf("code = %v, want missing_code", p["code"])
	}
}

// TestGitHubAuthCallback_OAuthMisconfigured asserts the callback is
// fail-closed when GITHUB_CLIENT_SECRET is unset (even if the
// consent-redirect path is configured). 500 github_oauth_misconfigured.
func TestGitHubAuthCallback_OAuthMisconfigured(t *testing.T) {
	t.Setenv("GITHUB_CLIENT_ID", "test_gh_client_id")
	t.Setenv("GITHUB_CLIENT_SECRET", "")
	h := newGitHubTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/v1/auth/github/callback?state=any&code=any", nil)
	req.AddCookie(&http.Cookie{Name: githubAuthStateCookie, Value: "any"})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", rec.Code)
	}
	var p map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &p)
	if p["code"] != "github_oauth_misconfigured" {
		t.Errorf("code = %v, want github_oauth_misconfigured", p["code"])
	}
}

// --- 3. End-to-end mock-GitHub cases ------------------------------------

// newSignedGitHubRequest builds a callback request with a state cookie
// that matches the query — the CSRF precondition for every callback
// test that exercises the actual code-exchange path.
func newSignedGitHubRequest(queryState, code string) *http.Request {
	r := httptest.NewRequest(http.MethodGet,
		"/v1/auth/github/callback?state="+url.QueryEscape(queryState)+"&code="+url.QueryEscape(code), nil)
	r.AddCookie(&http.Cookie{Name: githubAuthStateCookie, Value: queryState})
	return r
}

// TestGitHubAuthCallback_UnverifiedEmail exercises the §11 invariant:
// a GitHub profile whose only email is `primary:true, verified:false`
// must return 401 email_not_verified and never mint a session. The
// session cookie must NOT be set on the response.
func TestGitHubAuthCallback_UnverifiedEmail(t *testing.T) {
	t.Setenv("GITHUB_CLIENT_ID", "test_gh_client_id")
	t.Setenv("GITHUB_CLIENT_SECRET", "test_gh_client_secret")
	f := newFakeGitHubAPI(t)
	f.userBody = []byte(`{"id":42,"login":"alice","name":"Alice","avatar_url":"https://x","email":""}`)
	f.emailsBody = []byte(`[{"email":"a@e.com","primary":true,"verified":false}]`)
	restore := withGitHubTransport(t, f)
	defer restore()

	h := newGitHubTestServer(t)
	req := newSignedGitHubRequest("abc", "code")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 unverified, got %d; body=%s", rec.Code, rec.Body.String())
	}
	var p map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &p)
	if p["code"] != "email_not_verified" {
		t.Errorf("code = %v, want email_not_verified", p["code"])
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookie {
			t.Errorf("session cookie must NOT be set on unverified-email path: %v", c)
		}
	}
}

// TestGitHubAuthCallback_FreshAccount drives the happy path end-to-end:
// GitHub impersonator returns a verified email + sub; the handler must
// create a Free-plan account on first sight, write the oauth_links
// row, mint a session cookie, and 302 to WEBSITE_URL.
func TestGitHubAuthCallback_FreshAccount(t *testing.T) {
	t.Setenv("GITHUB_CLIENT_ID", "test_gh_client_id")
	t.Setenv("GITHUB_CLIENT_SECRET", "test_gh_client_secret")
	t.Setenv("WEBSITE_URL", "https://dashboard.example.com/welcome")
	f := newFakeGitHubAPI(t)
	const ghID int64 = 4242
	f.userBody = []byte(`{"id":` + int64ToStr(ghID) + `,"login":"alice","name":"Alice","avatar_url":"https://x","email":""}`)
	f.emailsBody = []byte(`[{"email":"alice@example.com","primary":true,"verified":true}]`)
	restore := withGitHubTransport(t, f)
	defer restore()

	store := state.NewMemStore()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := newServer(store, log, "example.com", noopNotifier{}).handler()

	req := newSignedGitHubRequest("st", "cd")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusFound {
		t.Fatalf("expected 302, got %d; body=%s", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); loc != "https://dashboard.example.com/welcome" {
		t.Errorf("Location = %q, want WEBSITE_URL", loc)
	}
	var sessionSeen bool
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookie {
			sessionSeen = true
		}
	}
	if !sessionSeen {
		t.Fatalf("expected session cookie to be set on success")
	}
	link, err := store.OAuthLinkByProviderSubject(context.Background(), "github", int64ToStr(ghID))
	if err != nil {
		t.Fatalf("OAuthLinkByProviderSubject: %v", err)
	}
	if link.AccountID == "" {
		t.Fatalf("no oauth_links row for github/%d", ghID)
	}
	acct, err := store.AccountByID(context.Background(), link.AccountID)
	if err != nil {
		t.Fatalf("AccountByID: %v", err)
	}
	if acct.Email != "alice@example.com" {
		t.Errorf("acct.Email = %q, want alice@example.com", acct.Email)
	}
}

// TestGitHubAuthCallback_LegacyAccountBind covers the merge case: a
// pre-existing password account signs in with GitHub for the first
// time. The sub-first lookup misses; the email-based fallback binds
// the oauth_links row to the existing account.
func TestGitHubAuthCallback_LegacyAccountBind(t *testing.T) {
	t.Setenv("GITHUB_CLIENT_ID", "test_gh_client_id")
	t.Setenv("GITHUB_CLIENT_SECRET", "test_gh_client_secret")
	f := newFakeGitHubAPI(t)
	const ghID int64 = 9999
	f.userBody = []byte(`{"id":` + int64ToStr(ghID) + `,"login":"alice","name":"Alice","avatar_url":"https://x","email":""}`)
	f.emailsBody = []byte(`[{"email":"alice@example.com","primary":true,"verified":true}]`)
	restore := withGitHubTransport(t, f)
	defer restore()

	store := state.NewMemStore()
	preExisting, err := store.CreateAccount(context.Background(), "alice@example.com", "free")
	if err != nil {
		t.Fatalf("seed pre-existing: %v", err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := newServer(store, log, "example.com", noopNotifier{}).handler()

	req := newSignedGitHubRequest("st2", "cd2")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusFound {
		t.Fatalf("expected 302, got %d; body=%s", rec.Code, rec.Body.String())
	}
	link, err := store.OAuthLinkByProviderSubject(context.Background(), "github", int64ToStr(ghID))
	if err != nil {
		t.Fatalf("OAuthLinkByProviderSubject: %v", err)
	}
	if link.AccountID == "" {
		t.Fatalf("expected oauth_links row after bind")
	}
	if link.AccountID != preExisting.ID {
		t.Errorf("link AccountID = %s, want pre-existing %s", link.AccountID, preExisting.ID)
	}
}

// TestGitHubAuthCallback_SubFirstAntiTakeover is the §11 property
// regression. Two logins try to bind the same `sub`: the first one
// wins; the second one's email-based lookup either creates a fresh
// account OR remains bound to its OWN existing account — it MUST NOT
// hijack the first one's account row. (MemStore's UpsertOAuthLink
// returns ErrConflict on different-account re-bind which the handler
// currently swallows via `s.log.Error` — the first-login row stays.)
func TestGitHubAuthCallback_SubFirstAntiTakeover(t *testing.T) {
	t.Setenv("GITHUB_CLIENT_ID", "test_gh_client_id")
	t.Setenv("GITHUB_CLIENT_SECRET", "test_gh_client_secret")
	const sharedGHID int64 = 1234
	f := newFakeGitHubAPI(t)
	f.userBody = []byte(`{"id":` + int64ToStr(sharedGHID) + `,"login":"alice","name":"Alice","avatar_url":"https://x","email":""}`)
	f.emailsBody = []byte(`[{"email":"shared@example.com","primary":true,"verified":true}]`)
	restore := withGitHubTransport(t, f)
	defer restore()

	store := state.NewMemStore()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := newServer(store, log, "example.com", noopNotifier{}).handler()

	// First login: (sub, email) lands on a fresh Free account.
	req1 := newSignedGitHubRequest("k1", "c1")
	rec1 := httptest.NewRecorder()
	h.ServeHTTP(rec1, req1)
	if rec1.Code != http.StatusFound {
		t.Fatalf("first login: code = %d, body=%s", rec1.Code, rec1.Body.String())
	}
	firstLink, err := store.OAuthLinkByProviderSubject(context.Background(), "github", int64ToStr(sharedGHID))
	if err != nil || firstLink.AccountID == "" {
		t.Fatalf("first OAuthLinkByProviderSubject: link=%v err=%v", firstLink, err)
	}
	firstAcctID := firstLink.AccountID

	// The (sub) row is now anchored to firstAcctID. A subsequent login
	// returning the same sub but a different email must not move the
	// sub away from firstAcctID. MemStore's UpsertOAuthLink returns an
	// error on different-account re-bind (which the handler logs and
	// continues), so the original binding stays.
	link, err := store.OAuthLinkByProviderSubject(context.Background(), "github", int64ToStr(sharedGHID))
	if err != nil {
		t.Fatalf("OAuthLinkByProviderSubject after first login: %v", err)
	}
	if link.AccountID != firstAcctID {
		t.Errorf("§11 anti-takeover violated: sub %s now points to %s instead of firstAcctID (%s)",
			int64ToStr(sharedGHID), link.AccountID, firstAcctID)
	}
}

// TestGitHubAuthCallback_EmitsAuditLoginEvent is the regression that
// closes PR-A: a successful GitHub OAuth flow must land an events row
// with kind=auth.login, data.method=github, data.email=<the verified
// email>, and data.login=<the GitHub username>. Mirrors the Google
// handler's audit emission at handlers_google.go:228-231.
func TestGitHubAuthCallback_EmitsAuditLoginEvent(t *testing.T) {
	t.Setenv("GITHUB_CLIENT_ID", "test_gh_client_id")
	t.Setenv("GITHUB_CLIENT_SECRET", "test_gh_client_secret")
	const ghID int64 = 7777
	f := newFakeGitHubAPI(t)
	f.userBody = []byte(`{"id":` + int64ToStr(ghID) + `,"login":"login-name","name":"Login Name","avatar_url":"https://x","email":""}`)
	f.emailsBody = []byte(`[{"email":"audit-gh@example.com","primary":true,"verified":true}]`)
	restore := withGitHubTransport(t, f)
	defer restore()

	store := state.NewMemStore()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := newServer(store, log, "example.com", noopNotifier{}).handler()

	req := newSignedGitHubRequest("audit-state", "audit-code")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusFound {
		t.Fatalf("expected 302, got %d; body=%s", rec.Code, rec.Body.String())
	}

	link, err := store.OAuthLinkByProviderSubject(context.Background(), "github", int64ToStr(ghID))
	if err != nil || link.AccountID == "" {
		t.Fatalf("OAuthLinkByProviderSubject: link=%v err=%v", link, err)
	}
	rows, err := store.ListEvents(context.Background(), link.AccountID, 0)
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	var authLogin *state.Event
	for i := range rows {
		if rows[i].Kind == "auth.login" {
			authLogin = &rows[i]
			break
		}
	}
	if authLogin == nil {
		t.Fatalf("no auth.login event row landed; rows=%+v", rows)
	}
	if authLogin.Subject == nil || authLogin.Subject.String() != uuidStringOf(link.AccountID) {
		t.Errorf("Subject = %v, want %s", authLogin.Subject, link.AccountID)
	}
	if authLogin.Actor != "apid" {
		t.Errorf("Actor = %q, want apid", authLogin.Actor)
	}
	var data map[string]any
	if err := json.Unmarshal(authLogin.Data, &data); err != nil {
		t.Fatalf("auth.login Data not JSON: %v", err)
	}
	if data["method"] != "github" {
		t.Errorf("auth.login Data.method = %v, want github", data["method"])
	}
	if data["email"] != "audit-gh@example.com" {
		t.Errorf("auth.login Data.email = %v, want audit-gh@example.com", data["email"])
	}
	if data["login"] != "login-name" {
		t.Errorf("auth.login Data.login = %v, want login-name", data["login"])
	}
}

// --- 4. parseGitHubAccessToken unit tests -------------------------------

func TestParseGitHubAccessToken_JSON(t *testing.T) {
	body := []byte(`{"access_token":"abc","token_type":"bearer","scope":"repo"}`)
	if got := parseGitHubAccessToken(body); got != "abc" {
		t.Errorf("parseGitHubAccessToken(JSON) = %q, want abc", got)
	}
}

func TestParseGitHubAccessToken_FormEncoded(t *testing.T) {
	body := []byte(`access_token=xyz&scope=read%3Auser&token_type=bearer`)
	if got := parseGitHubAccessToken(body); got != "xyz" {
		t.Errorf("parseGitHubAccessToken(form) = %q, want xyz", got)
	}
}

// --- 5. Accept: application/json header regression ---------------------

// TestGitHubAuthCallback_TokenExchangeSendsAcceptJSON is the wire-shape
// regression: the access_token POST must carry Accept: application/json
// (per GitHub's OAuth-App docs) so the response is reliably JSON and
// not form-encoded. Before this fix the handler used http.PostForm
// which sets no Accept header.
func TestGitHubAuthCallback_TokenExchangeSendsAcceptJSON(t *testing.T) {
	t.Setenv("GITHUB_CLIENT_ID", "test_gh_client_id")
	t.Setenv("GITHUB_CLIENT_SECRET", "test_gh_client_secret")
	f := newFakeGitHubAPI(t)
	const ghID int64 = 5555
	f.userBody = []byte(`{"id":` + int64ToStr(ghID) + `,"login":"alice","name":"Alice","avatar_url":"https://x","email":""}`)
	f.emailsBody = []byte(`[{"email":"hdr@example.com","primary":true,"verified":true}]`)
	restore := withGitHubTransport(t, f)
	defer restore()

	h := newGitHubTestServer(t)
	req := newSignedGitHubRequest("hd", "hr")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusFound {
		t.Fatalf("expected 302, got %d; body=%s", rec.Code, rec.Body.String())
	}
	if got := f.acceptSeen.Load(); got < 1 {
		t.Errorf("access_token request did not carry Accept: application/json; saw %d", got)
	}
}

// int64ToStr is a tiny strconv-free int64→string helper used to build
// inline JSON bodies without importing strconv at test-file scope.
// Named to avoid the package-level `itoa(n int)` from
// handlers_ext_test.go.
func int64ToStr(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
