// Bug-regression tests for issue #165 (PR #1, ADR-032).
//
// Pre-#165, POST /login auto-created an account for ANY email, minted
// a "web-console" API key, returned that key in the JSON response body,
// and set a 7-day faas_sid session cookie — with zero verification. A
// single curl was a full pre-auth account-takeover (spec §11 violation).
//
// These tests pin the post-#165 contract: the handler NEVER sets a
// session, NEVER mints a key, NEVER surfaces an api_key in the body,
// and NEVER auto-creates an account — regardless of whether the email
// exists, is unknown, or the request omits the X-Dashboard-Key header.
// The bug-regression test (TestLogin_ArbitraryEmailDoesNotSetSession)
// is the one that closes issue #165.

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
	"testing"

	"github.com/onebox-faas/faas/pkg/api"
	"github.com/onebox-faas/faas/pkg/auth"
	"github.com/onebox-faas/faas/pkg/state"
)

// TestLogin_ArbitraryEmailDoesNotSetSession is the issue #165
// regression test. The pre-#165 handler accepted any well-formed
// email, auto-created the account if it didn't exist, set the
// session cookie, and returned the API key in the body. None of
// that must happen now.
//
// Asserted contract on POST /login with a valid-shape email and
// NO X-Dashboard-Key header:
//   - HTTP status: 401 (not 200, not 500)
//   - No Set-Cookie with name "faas_sid" of any non-empty value
//   - Response JSON body has code="invalid_credentials"
//   - Response JSON body has NO "api_key" field (closed via the
//     fact that the field is no longer in the success struct either,
//     but pinned here against future drift)
func TestLogin_ArbitraryEmailDoesNotSetSession(t *testing.T) {
	store := state.NewMemStore()
	srv := newServer(store, slog.New(slog.NewTextHandler(io.Discard, nil)), "example.com", noopNotifier{})
	h := srv.handler()

	// Use a victim-style email. It does not need to exist in the
	// store — the bug fires whether or not the account is present,
	// because pre-#165 the handler auto-created it.
	const victim = "victim@example.com"
	form := url.Values{"email": {victim}, "password": {"any-password-1234567890"}}
	req := httptest.NewRequest("POST", "/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	// Deliberately do NOT set X-Dashboard-Key.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	// Status must be 401, not 200.
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("/login status = %d, want 401 (body=%s)", rec.Code, rec.Body.String())
	}

	// No faas_sid cookie of any non-empty value may be set.
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookie && c.Value != "" {
			t.Errorf("faas_sid cookie set with non-empty value on /login without X-Dashboard-Key; " +
				"this is the #165 takeover behaviour returning")
		}
	}

	// Body must carry the invalid_credentials code.
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if got := body["code"]; got != api.CodeInvalidCredentials {
		t.Errorf("code = %v, want %q", got, api.CodeInvalidCredentials)
	}

	// Body must NOT carry an api_key field. The pre-#165 handler
	// surfaced the freshly minted key here, which made the
	// takeover reproducible in a single curl + jq.
	if _, has := body["api_key"]; has {
		t.Errorf("response body has api_key field; pre-#165 behaviour has returned")
	}
}

// TestLogin_UnknownEmailWithoutKeyCollapsesTo401 confirms the
// anti-enumeration shape: a POST /login for an email that does
// not exist in the store, without X-Dashboard-Key, returns 401
// with the SAME body the wrong-email-with-valid-key path
// produces. An attacker probing for valid emails must not be
// able to tell "no such account" apart from "wrong key" by the
// response.
func TestLogin_UnknownEmailWithoutKeyCollapsesTo401(t *testing.T) {
	store := state.NewMemStore()
	srv := newServer(store, slog.New(slog.NewTextHandler(io.Discard, nil)), "example.com", noopNotifier{})
	h := srv.handler()

	form := url.Values{"email": {"ghost@example.com"}, "password": {"any-password-1234567890"}}
	req := httptest.NewRequest("POST", "/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("/login status = %d, want 401 (body=%s)", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if got := body["code"]; got != api.CodeInvalidCredentials {
		t.Errorf("code = %v, want %q", got, api.CodeInvalidCredentials)
	}
}

// TestLogin_EmptyPasswordReturns400 covers the input-shape pre-check
// path: a POST /login with a missing password field returns 400
// before the Argon2id verify runs. Confirms the handler does not
// leak "valid email, missing password" as a distinct response.
func TestLogin_EmptyPasswordReturns400(t *testing.T) {
	store := state.NewMemStore()
	srv := newServer(store, slog.New(slog.NewTextHandler(io.Discard, nil)), "example.com", noopNotifier{})
	h := srv.handler()

	form := url.Values{"email": {"alice@example.com"}}
	req := httptest.NewRequest("POST", "/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("/login status = %d, want 400 (body=%s)", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if got := body["code"]; got != api.CodeValidation {
		t.Errorf("code = %v, want %q", got, api.CodeValidation)
	}
}

// TestLogin_BoundEmailPasswordMismatchReturns401 covers the wrong-
// password path: alice has an account with a real password, but
// the form submits the wrong password. The handler must collapse
// to 401 invalid_credentials with the same body as the no-account
// path — an attacker must not learn that the email is bound.
func TestLogin_BoundEmailPasswordMismatchReturns401(t *testing.T) {
	store := state.NewMemStore()
	acct, err := store.CreateAccount(context.Background(), "alice@example.com", api.PlanFree)
	if err != nil {
		t.Fatal(err)
	}
	phc, err := auth.Encode("correct-horse-battery-staple")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetAccountPassword(context.Background(), acct.ID, phc); err != nil {
		t.Fatal(err)
	}

	srv := newServer(store, slog.New(slog.NewTextHandler(io.Discard, nil)), "example.com", noopNotifier{})
	h := srv.handler()

	form := url.Values{"email": {"alice@example.com"}, "password": {"wrong-password-1234567890"}}
	req := httptest.NewRequest("POST", "/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("/login status = %d, want 401 (body=%s)", rec.Code, rec.Body.String())
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookie && c.Value != "" {
			t.Errorf("faas_sid cookie set on wrong-password /login; " +
				"the §11 anti-enumeration pad must not surface a session")
		}
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if got := body["code"]; got != api.CodeInvalidCredentials {
		t.Errorf("code = %v, want %q", got, api.CodeInvalidCredentials)
	}
}

// TestLogin_ValidPasswordIssuesSessionAndNoAPIKeyInBody is the
// happy-path counterpart to the bug regression. PR #2 replaces
// the X-Dashboard-Key path with email + password (Argon2id). The
// response body is {account_id, plan} — NO api_key field. The
// pre-#165 handler returned the freshly minted key here, which
// made the takeover reproducible in a single curl.
func TestLogin_ValidPasswordIssuesSessionAndNoAPIKeyInBody(t *testing.T) {
	store := state.NewMemStore()
	const email = "alice@example.com"
	acct, err := store.CreateAccount(context.Background(), email, api.PlanFree)
	if err != nil {
		t.Fatal(err)
	}
	const password = "correct-horse-battery-staple"
	phc, err := auth.Encode(password)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetAccountPassword(context.Background(), acct.ID, phc); err != nil {
		t.Fatal(err)
	}

	srv := newServer(store, slog.New(slog.NewTextHandler(io.Discard, nil)), "example.com", noopNotifier{})
	h := srv.handler()

	form := url.Values{"email": {email}, "password": {password}}
	req := httptest.NewRequest("POST", "/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("/login status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}

	// Session cookie must be set.
	var gotSession bool
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookie && c.Value != "" {
			gotSession = true
		}
	}
	if !gotSession {
		t.Errorf("expected %s session cookie on valid /login", sessionCookie)
	}

	// Body MUST NOT have api_key. Even on a successful login, the
	// response must not leak a key — the SDK uses the device-code
	// flow for API access, and the dashboard cookie is the only
	// auth artifact on the browser side.
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if _, has := body["api_key"]; has {
		t.Errorf("response body has api_key field on valid login; " +
			"this is the #165 leak path")
	}
	if got := body["account_id"]; got != acct.ID {
		t.Errorf("account_id = %v, want %q", got, acct.ID)
	}
}

// TestLogin_DoesNotAutoCreateAccount pins the spec §11 invariant
// that PR #1 closes: a POST /login for an unknown email must not
// silently create an account. We probe by counting accounts in the
// store after a 401.
func TestLogin_DoesNotAutoCreateAccount(t *testing.T) {
	store := state.NewMemStore()
	srv := newServer(store, slog.New(slog.NewTextHandler(io.Discard, nil)), "example.com", noopNotifier{})
	h := srv.handler()

	form := url.Values{"email": {"newcomer@example.com"}, "password": {"any-password-1234567890"}}
	req := httptest.NewRequest("POST", "/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("/login status = %d, want 401", rec.Code)
	}

	// The store must have zero accounts after the request.
	accts, err := store.ListAllAccounts(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(accts) != 0 {
		t.Errorf("auto-created %d account(s) on a 401 /login; this is the #165 root cause", len(accts))
	}
}
