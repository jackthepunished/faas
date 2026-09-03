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
// Before this ADR every session-cookie principal hit the blanket
// requireStepUpHandler gate, and the only writer of a step-up stamp is
// /v1/account/mfa/verify — so an OAuth-only customer without MFA could
// never set a password at all, and a customer with one could replace
// it without re-proving anything once they had a stamp.

const (
	seededPassword = "the-seeded-password-1"
	chosenPassword = "correct-horse-battery-staple"
)

// seedPassword gives alice a password row so the account counts as
// "has password" for the handler's decision.
func seedPassword(t *testing.T, store *state.MemStore, email string) string {
	t.Helper()
	acct, err := store.AccountByEmail(t.Context(), email)
	if err != nil {
		t.Fatalf("AccountByEmail: %v", err)
	}
	phc, err := auth.Encode(seededPassword)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if err := store.SetAccountPassword(t.Context(), acct.ID, phc); err != nil {
		t.Fatalf("SetAccountPassword: %v", err)
	}
	return acct.ID
}

func postSetPasswordForm(t *testing.T, h http.Handler, sid *http.Cookie, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/dashboard/account/set-password",
		strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.AddCookie(sid)
	h.ServeHTTP(rec, r)
	return rec
}

func problemCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var p api.Problem
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatalf("body is not a problem+json: %v\n%s", err, rec.Body.String())
	}
	return p.Code
}

func storedPasswordVerifies(t *testing.T, store *state.MemStore, accountID, plain string) bool {
	t.Helper()
	phc, err := store.AccountPasswordByAccountID(t.Context(), accountID)
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
func steppedUpCookie(t *testing.T, store *state.MemStore, mgr sessionIssuer, accountID string) *http.Cookie {
	t.Helper()
	sid := "stepped-up-sid"
	if _, err := store.CreateSession(t.Context(), sid, accountID, "192.0.2.10", "stepped-up-ua"); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	cookie, err := mgr.IssueWithSessionAndBindingHashAndStepUp(sid, accountID, "", time.Now(), false)
	if err != nil {
		t.Fatalf("issue stepped-up cookie: %v", err)
	}
	return &http.Cookie{Name: sessionCookie, Value: cookie}
}

type sessionIssuer interface {
	IssueWithSessionAndBindingHashAndStepUp(sid, accountID, bindingHash string, stepUpAt time.Time, mfaPending bool) (string, error)
}

func TestSetPassword_OAuthOnlyNoMFA_SetsWithoutStepUp(t *testing.T) {
	h, sid, store, _ := newAuthedDashboardServerFull(t)
	acct, err := store.AccountByEmail(t.Context(), "alice@example.com")
	if err != nil {
		t.Fatalf("AccountByEmail: %v", err)
	}

	rec := postSetPasswordForm(t, h, sid, url.Values{"password": {chosenPassword}})

	if rec.Code != http.StatusFound {
		t.Fatalf("code = %d, want 302\nbody = %s", rec.Code, rec.Body.String())
	}
	if !storedPasswordVerifies(t, store, acct.ID, chosenPassword) {
		t.Fatal("password was not stored")
	}
}

func TestSetPassword_HasPassword_RequiresCurrentPassword(t *testing.T) {
	h, sid, store, _ := newAuthedDashboardServerFull(t)
	id := seedPassword(t, store, "alice@example.com")

	rec := postSetPasswordForm(t, h, sid, url.Values{"password": {chosenPassword}})

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
	h, sid, store, _ := newAuthedDashboardServerFull(t)
	id := seedPassword(t, store, "alice@example.com")

	rec := postSetPasswordForm(t, h, sid, url.Values{
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
	h, sid, store, _ := newAuthedDashboardServerFull(t)
	id := seedPassword(t, store, "alice@example.com")

	rec := postSetPasswordForm(t, h, sid, url.Values{
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
	id := seedPassword(t, store, "alice@example.com")
	sid := steppedUpCookie(t, store, mgr, id)

	rec := postSetPasswordForm(t, h, sid, url.Values{"password": {chosenPassword}})

	if rec.Code != http.StatusFound {
		t.Fatalf("code = %d, want 302\nbody = %s", rec.Code, rec.Body.String())
	}
	if !storedPasswordVerifies(t, store, id, chosenPassword) {
		t.Fatal("the new password was not stored")
	}
}

func TestSetPassword_OAuthOnlyWithMFA_RequiresStepUp(t *testing.T) {
	h, sid, store, _ := newAuthedDashboardServerFull(t)
	acct, err := store.AccountByEmail(t.Context(), "alice@example.com")
	if err != nil {
		t.Fatalf("AccountByEmail: %v", err)
	}
	if err := store.MarkMFAEnrolled(t.Context(), acct.ID); err != nil {
		t.Fatalf("MarkMFAEnrolled: %v", err)
	}

	rec := postSetPasswordForm(t, h, sid, url.Values{"password": {chosenPassword}})

	if rec.Code != http.StatusForbidden {
		t.Fatalf("code = %d, want 403\nbody = %s", rec.Code, rec.Body.String())
	}
	if code := problemCode(t, rec); code != api.CodeStepUpRequired {
		t.Errorf("code = %q, want %q", code, api.CodeStepUpRequired)
	}
	if _, err := store.AccountPasswordByAccountID(t.Context(), acct.ID); err == nil {
		t.Fatal("a password was stored without a step-up")
	}
}

func TestSetPassword_WeakPasswordStillRefusedAfterProof(t *testing.T) {
	h, sid, store, _ := newAuthedDashboardServerFull(t)
	id := seedPassword(t, store, "alice@example.com")

	rec := postSetPasswordForm(t, h, sid, url.Values{
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
