package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/onebox-faas/faas/pkg/api"
	"github.com/onebox-faas/faas/pkg/auth"
	"github.com/onebox-faas/faas/pkg/middleware"
	"github.com/onebox-faas/faas/pkg/session"
	"github.com/onebox-faas/faas/pkg/state"
)

// ADR-140: POST /dashboard/account/set-password picks its proof of
// presence from what the account has, instead of demanding a TOTP
// step-up from everyone. The matrix these tests pin:
//
//	fresh step-up stamp            → accepted (unchanged for MFA users)
//	has password, no stamp         → current_password required + verified
//	no password, MFA enrolled      → 403 step_up_required
//	no password, no MFA            → accepted (the opt-in the route exists for)
//
// Every branch sits behind a purpose-bound csrf_token, because the
// route is a same-site form POST (see TestSetPassword_RefusesFormWithoutCSRFToken).
//
// Before this ADR every session-cookie principal hit the blanket
// requireStepUpHandler gate, and the only writer of a step-up stamp is
// /v1/account/mfa/verify — so an OAuth-only customer without MFA could
// never set a password at all, and a customer with one could replace
// it without re-proving anything once they had a stamp.

const (
	seededPassword = "the-seeded-password-1"
	chosenPassword = "correct-horse-battery-staple"
)

func accountID(t *testing.T, store *state.MemStore, email string) string {
	t.Helper()
	acct, err := store.AccountByEmail(t.Context(), email)
	if err != nil {
		t.Fatalf("AccountByEmail: %v", err)
	}
	return acct.ID
}

// seedPassword gives alice a password row so the account counts as
// "has password" for the handler's decision.
func seedPassword(t *testing.T, store *state.MemStore, id string) {
	t.Helper()
	phc, err := auth.Encode(seededPassword)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if err := store.SetAccountPassword(t.Context(), id, phc); err != nil {
		t.Fatalf("SetAccountPassword: %v", err)
	}
}

// postSetPasswordForm submits the form the way the console does: with
// a purpose-bound csrf_token minted for this account and its faas_csrf
// sidecar cookie. `mgr == nil` sends the form bare, for the test that
// pins the token as mandatory.
func postSetPasswordForm(t *testing.T, h http.Handler, sid *http.Cookie, mgr *session.Manager, id string, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	form = cloneValues(form)
	var csrfCookie *http.Cookie
	if mgr != nil {
		tok, err := middleware.IssueForAuthenticated(mgr, "set_password", id)
		if err != nil {
			t.Fatalf("issue csrf: %v", err)
		}
		form.Set("csrf_token", tok)
		csrfCookie = &http.Cookie{Name: middleware.CookieNameAuthenticated, Value: tok}
	}
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/dashboard/account/set-password",
		strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.AddCookie(sid)
	if csrfCookie != nil {
		r.AddCookie(csrfCookie)
	}
	h.ServeHTTP(rec, r)
	return rec
}

func cloneValues(v url.Values) url.Values {
	out := url.Values{}
	for k, vals := range v {
		out[k] = append([]string(nil), vals...)
	}
	return out
}

func problemCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var p api.Problem
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatalf("body is not a problem+json: %v\n%s", err, rec.Body.String())
	}
	return p.Code
}

func storedPasswordVerifies(t *testing.T, store *state.MemStore, id, plain string) bool {
	t.Helper()
	phc, err := store.AccountPasswordByAccountID(t.Context(), id)
	if err != nil {
		return false
	}
	ok, err := auth.Verify(phc, plain)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	return ok
}

// steppedUpCookie re-issues alice's cookie with a fresh step-up stamp,
// mirroring newSteppedUpDashboardServer but against a server whose
// store the test also holds.
func steppedUpCookie(t *testing.T, store *state.MemStore, mgr *session.Manager, id string) *http.Cookie {
	t.Helper()
	sid := "stepped-up-sid"
	if _, err := store.CreateSession(t.Context(), sid, id, "192.0.2.10", "stepped-up-ua"); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	cookie, err := mgr.IssueWithSessionAndBindingHashAndStepUp(sid, id, "", time.Now(), false)
	if err != nil {
		t.Fatalf("issue stepped-up cookie: %v", err)
	}
	return &http.Cookie{Name: sessionCookie, Value: cookie}
}

// The route is a same-site form POST: a function hosted at
// *.apps.gregale.dev is same-site with api.gregale.dev, so SameSite=Lax
// still attaches faas_sid to a form it auto-submits. Without a
// purpose-bound token the `session` proof would let that page choose
// the victim's password.
func TestSetPassword_RefusesFormWithoutCSRFToken(t *testing.T) {
	h, sid, store, _ := newAuthedDashboardServerFull(t)
	id := accountID(t, store, "alice@example.com")

	rec := postSetPasswordForm(t, h, sid, nil, id, url.Values{"password": {chosenPassword}})

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code = %d, want 400\nbody = %s", rec.Code, rec.Body.String())
	}
	if code := problemCode(t, rec); code != api.CodeValidation {
		t.Errorf("code = %q, want %q", code, api.CodeValidation)
	}
	if _, err := store.AccountPasswordByAccountID(t.Context(), id); err == nil {
		t.Fatal("a password was stored from a form with no CSRF token")
	}
}

func TestSetPassword_OAuthOnlyNoMFA_SetsWithoutStepUp(t *testing.T) {
	h, sid, store, mgr := newAuthedDashboardServerFull(t)
	id := accountID(t, store, "alice@example.com")

	rec := postSetPasswordForm(t, h, sid, mgr, id, url.Values{"password": {chosenPassword}})

	if rec.Code != http.StatusFound {
		t.Fatalf("code = %d, want 302\nbody = %s", rec.Code, rec.Body.String())
	}
	if !storedPasswordVerifies(t, store, id, chosenPassword) {
		t.Fatal("password was not stored")
	}
}

func TestSetPassword_HasPassword_RequiresCurrentPassword(t *testing.T) {
	h, sid, store, mgr := newAuthedDashboardServerFull(t)
	id := accountID(t, store, "alice@example.com")
	seedPassword(t, store, id)

	rec := postSetPasswordForm(t, h, sid, mgr, id, url.Values{"password": {chosenPassword}})

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("code = %d, want 401\nbody = %s", rec.Code, rec.Body.String())
	}
	if code := problemCode(t, rec); code != api.CodeInvalidCredentials {
		t.Errorf("code = %q, want %q", code, api.CodeInvalidCredentials)
	}
	if !storedPasswordVerifies(t, store, id, seededPassword) {
		t.Fatal("the seeded password was replaced without proof")
	}
}

func TestSetPassword_HasPassword_RejectsWrongCurrentPassword(t *testing.T) {
	h, sid, store, mgr := newAuthedDashboardServerFull(t)
	id := accountID(t, store, "alice@example.com")
	seedPassword(t, store, id)

	rec := postSetPasswordForm(t, h, sid, mgr, id, url.Values{
		"password":         {chosenPassword},
		"current_password": {"not-the-seeded-password"},
	})

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("code = %d, want 401\nbody = %s", rec.Code, rec.Body.String())
	}
	if code := problemCode(t, rec); code != api.CodeInvalidCredentials {
		t.Errorf("code = %q, want %q", code, api.CodeInvalidCredentials)
	}
	if !storedPasswordVerifies(t, store, id, seededPassword) {
		t.Fatal("the seeded password was replaced on a wrong current_password")
	}
}

func TestSetPassword_HasPassword_AcceptsCorrectCurrentPassword(t *testing.T) {
	h, sid, store, mgr := newAuthedDashboardServerFull(t)
	id := accountID(t, store, "alice@example.com")
	seedPassword(t, store, id)

	rec := postSetPasswordForm(t, h, sid, mgr, id, url.Values{
		"password":         {chosenPassword},
		"current_password": {seededPassword},
	})

	if rec.Code != http.StatusFound {
		t.Fatalf("code = %d, want 302\nbody = %s", rec.Code, rec.Body.String())
	}
	if !storedPasswordVerifies(t, store, id, chosenPassword) {
		t.Fatal("the new password was not stored")
	}
}

func TestSetPassword_HasPassword_FreshStepUpStandsInForCurrentPassword(t *testing.T) {
	h, _, store, mgr := newAuthedDashboardServerFull(t)
	id := accountID(t, store, "alice@example.com")
	seedPassword(t, store, id)
	sid := steppedUpCookie(t, store, mgr, id)

	rec := postSetPasswordForm(t, h, sid, mgr, id, url.Values{"password": {chosenPassword}})

	if rec.Code != http.StatusFound {
		t.Fatalf("code = %d, want 302\nbody = %s", rec.Code, rec.Body.String())
	}
	if !storedPasswordVerifies(t, store, id, chosenPassword) {
		t.Fatal("the new password was not stored")
	}
}

func TestSetPassword_OAuthOnlyWithMFA_RequiresStepUp(t *testing.T) {
	h, sid, store, mgr := newAuthedDashboardServerFull(t)
	id := accountID(t, store, "alice@example.com")
	if err := store.MarkMFAEnrolled(t.Context(), id); err != nil {
		t.Fatalf("MarkMFAEnrolled: %v", err)
	}

	rec := postSetPasswordForm(t, h, sid, mgr, id, url.Values{"password": {chosenPassword}})

	if rec.Code != http.StatusForbidden {
		t.Fatalf("code = %d, want 403\nbody = %s", rec.Code, rec.Body.String())
	}
	if code := problemCode(t, rec); code != api.CodeStepUpRequired {
		t.Errorf("code = %q, want %q", code, api.CodeStepUpRequired)
	}
	if _, err := store.AccountPasswordByAccountID(t.Context(), id); err == nil {
		t.Fatal("a password was stored without a step-up")
	}
}

func TestSetPassword_WeakPasswordStillRefusedAfterProof(t *testing.T) {
	h, sid, store, mgr := newAuthedDashboardServerFull(t)
	id := accountID(t, store, "alice@example.com")
	seedPassword(t, store, id)

	rec := postSetPasswordForm(t, h, sid, mgr, id, url.Values{
		"password":         {"short"},
		"current_password": {seededPassword},
	})

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code = %d, want 400\nbody = %s", rec.Code, rec.Body.String())
	}
	if code := problemCode(t, rec); code != api.CodePasswordTooWeak {
		t.Errorf("code = %q, want %q", code, api.CodePasswordTooWeak)
	}
	if !storedPasswordVerifies(t, store, id, seededPassword) {
		t.Fatal("the seeded password was replaced by a weak one")
	}
}
