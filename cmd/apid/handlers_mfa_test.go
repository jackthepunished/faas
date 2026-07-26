// MFA handler tests (IAM-2, issue #186).
//
// Covers the five /v1/account/mfa/* endpoints + the
// chokepoint helpers. Tests use the MemStore + an in-process
// age identity so the seal/unseal path runs end-to-end
// without an external key file. The MFA recipient + identity
// are wired into the package-level vars (mfaRecipient,
// mfaIdentity) and restored on t.Cleanup so a parallel test
// suite doesn't observe a stale value.
//
// The mfa_pending=true branch of the requireMFA middleware
// table is covered by mfa_middleware_test.go; this file
// focuses on the handlers themselves.

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"filippo.io/age"

	"github.com/pquerna/otp/totp"

	"github.com/onebox-faas/faas/pkg/api"
	"github.com/onebox-faas/faas/pkg/auth"
	"github.com/onebox-faas/faas/pkg/secretbox"
	"github.com/onebox-faas/faas/pkg/session"
	"github.com/onebox-faas/faas/pkg/state"
	"github.com/onebox-faas/faas/pkg/wire"
)

// mfaTestEnv is the session-cookie twin of testEnv for the MFA
// flow. The cookie is fresh-issued (mfa_pending=false) — every
// test here is exercising the post-enrollment path where the
// cookie is already cleared.
//
// `id` is the in-process age identity used by the seal/unseal
// path inside the handlers. It is also exposed so callers can
// generate recovery codes against the same key the production
// envelope will use.
type mfaTestEnv struct {
	h      http.Handler
	srv    *server
	store  *state.MemStore
	acct   state.Account
	mgr    *session.Manager
	id     *age.X25519Identity
	cookie *http.Cookie
}

// setupWithMFA mints a MemStore, an account, an ephemeral
// session manager, and an in-process age identity wired into
// both mfaRecipient + mfaIdentity. The returned env carries a
// non-mfa_pending cookie so requireMFA passes for the routes
// under test.
//
// `mfaRequired` lets the test pin the policy flag up-front;
// the chokepoint tests use this to avoid standing up a Stripe
// webhook just to arm the flag.
//
// `mfaEnrolled` stamps mfa_enrolled_at via MarkMFAEnrolled,
// which ALSO clears mfa_required. So if the test wants both
// flags set, the order matters: the chokepoint predicate
// (mfaEnrollRequired) is `mfa_required && !mfa_enrolled`, so
// `mfaEnrolled=true` is the post-enrollment state where
// mfa_required is false.
func setupWithMFA(t *testing.T, plan api.Plan, mfaRequired, mfaEnrolled bool) mfaTestEnv {
	t.Helper()
	store := state.NewMemStore()
	acct, err := store.CreateAccount(context.Background(),
		"mfa@example.com", plan)
	if err != nil {
		t.Fatal(err)
	}
	if mfaRequired {
		if err := store.SetMFARequired(context.Background(), acct.ID, true); err != nil {
			t.Fatal(err)
		}
	}
	if mfaEnrolled {
		// MarkMFAEnrolled stamps enrolled_at and clears
		// mfa_required. Order matters: stamp required first
		// (above), then mark enrolled — the final state is the
		// "enrolled customer, policy cleared" post-confirm
		// condition.
		if err := store.MarkMFAEnrolled(context.Background(), acct.ID); err != nil {
			t.Fatal(err)
		}
	}
	acct, _ = store.AccountByID(context.Background(), acct.ID)

	mgr, err := session.NewEphemeralManager(sessionCookieLifetime)
	if err != nil {
		t.Fatal(err)
	}
	token, err := mgr.Issue(acct.ID)
	if err != nil {
		t.Fatal(err)
	}

	// In-process age identity: GenerateAndSaveHostKey writes a
	// fresh X25519 key to a temp file (the same code path
	// production uses). We then close the file to free the
	// handle; the in-memory `id` is what the handlers reach.
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "host.age")
	id, err := secretbox.GenerateAndSaveHostKey(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(keyPath, 0o400); err != nil {
		t.Fatal(err)
	}

	prevRec := mfaRecipient
	prevIdent := mfaIdentity
	SetMFARecipient(func() *age.X25519Recipient { return id.Recipient() })
	SetMFAIdentity(func() *age.X25519Identity { return id })
	t.Cleanup(func() {
		SetMFARecipient(prevRec)
		SetMFAIdentity(prevIdent)
	})

	ops := wire.NewOpsMetrics("apid_mfa_test")
	srv := newServerWithDeps(store, slog.New(slog.NewTextHandler(io.Discard, nil)),
		"example.com", noopNotifier{}, "", noopMailer{}, stubGithubdClient{}, mgr,
		nil, 15*60_000_000_000, "").WithOpsMetrics(ops)

	return mfaTestEnv{
		h:      srv.handler(),
		srv:    srv,
		store:  store,
		acct:   acct,
		mgr:    mgr,
		id:     id,
		cookie: &http.Cookie{Name: sessionCookie, Value: token},
	}
}

// do runs an MFA-cookie-authed request. Mirrors testEnv.do but
// uses req.AddCookie instead of the bearer header.
func (e mfaTestEnv) do(t *testing.T, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	req := httptest.NewRequest(method, path, r)
	req.AddCookie(e.cookie)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	return rec
}

// mfaIssueWithPending re-issues the cookie with mfa_pending set
// the way the login handlers would after a chokepoint flipped
// mfa_required. Returns a fresh cookie the test can pin on the
// next request.
func (e mfaTestEnv) mfaIssueWithPending(t *testing.T, pending bool) *http.Cookie {
	t.Helper()
	tok, err := e.mgr.IssueWithMFAFlag(e.acct.ID, pending)
	if err != nil {
		t.Fatal(err)
	}
	return &http.Cookie{Name: sessionCookie, Value: tok}
}

// generateEnrolledAccount runs /enroll + /confirm on a fresh
// account and returns the response so the test can drive
// /recover or /disable later. The secret is the TOTP secret —
// used to compute a matching code on the next step.
func (e mfaTestEnv) generateEnrolledAccount(t *testing.T) (recovCodes []string, secret string, freshCookie *http.Cookie) {
	t.Helper()
	rec := e.do(t, http.MethodPost, "/v1/account/mfa/enroll", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("/enroll: status %d: %s", rec.Code, rec.Body)
	}
	var out api.MFAEnrollResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode /enroll: %v", err)
	}
	code, err := totp.GenerateCodeCustom(out.Secret, time.Now().UTC(), totp.ValidateOpts{
		Period: 30, Digits: 6, Algorithm: 0, // 0 = SHA1 in pquerna/otp (otp.AlgorithmSHA1)
	})
	if err != nil {
		t.Fatalf("GenerateCodeCustom: %v", err)
	}
	confirmRec := e.do(t, http.MethodPost, "/v1/account/mfa/confirm", api.MFAConfirmRequest{Totp: code})
	if confirmRec.Code != http.StatusOK {
		t.Fatalf("/confirm: status %d: %s", confirmRec.Code, confirmRec.Body)
	}
	// /confirm re-issues the cookie. Pull it from the recorder so
	// subsequent calls use the cleared (mfa_pending=false) cookie.
	for _, c := range confirmRec.Result().Cookies() {
		if c.Name == sessionCookie {
			freshCookie = c
			break
		}
	}
	if freshCookie == nil {
		t.Fatalf("/confirm did not re-issue the cookie")
	}
	return out.RecoveryCodes, out.Secret, freshCookie
}

// --- /enroll ----------------------------------------------------------------

// TestMFAEnroll_ReturnsSecretAndQR pins the wire shape: the
// response MUST carry otpauth_url, secret, qr_code_png_base64,
// and recovery_codes (10 of them). Empty or missing any of
// these breaks the dashboard.
func TestMFAEnroll_ReturnsSecretAndQR(t *testing.T) {
	e := setupWithMFA(t, api.PlanPro, false, false)
	rec := e.do(t, http.MethodPost, "/v1/account/mfa/enroll", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	var out api.MFAEnrollResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.OTPAuthURL == "" || !strings.HasPrefix(out.OTPAuthURL, "otpauth://totp/") {
		t.Errorf("OTPAuthURL = %q, want otpauth://totp/...", out.OTPAuthURL)
	}
	if out.Secret == "" || len(out.Secret) != 32 {
		t.Errorf("Secret = %q (len %d), want 32-char base32", out.Secret, len(out.Secret))
	}
	if len(out.QRCodePNG) == 0 {
		t.Errorf("QRCodePNG empty")
	}
	if len(out.RecoveryCodes) != auth.RecoveryCodeCount {
		t.Errorf("RecoveryCodes count = %d, want %d", len(out.RecoveryCodes), auth.RecoveryCodeCount)
	}
}

// TestMFAEnroll_RejectsAlreadyEnrolled: a 2nd call while the
// account is enrolled returns 409 CodeConflict. The dashboard
// should never land in this state (the /disable flow clears
// enrollment first), but the handler defends against the
// race.
func TestMFAEnroll_RejectsAlreadyEnrolled(t *testing.T) {
	e := setupWithMFA(t, api.PlanPro, false, true)
	rec := e.do(t, http.MethodPost, "/v1/account/mfa/enroll", nil)
	if rec.Code != http.StatusConflict {
		t.Errorf("status = %d, want 409", rec.Code)
	}
}

// TestMFAEnroll_RejectsWhenRecipientMissing: with no host key
// wired, /enroll returns 503 CodeCapacity. The customer's
// primary action fails closed rather than minting a secret
// that can't be sealed.
func TestMFAEnroll_RejectsWhenRecipientMissing(t *testing.T) {
	e := setupWithMFA(t, api.PlanPro, false, false)
	prevRec := mfaRecipient
	SetMFARecipient(func() *age.X25519Recipient { return nil })
	t.Cleanup(func() { SetMFARecipient(prevRec) })

	rec := e.do(t, http.MethodPost, "/v1/account/mfa/enroll", nil)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rec.Code)
	}
}

// --- /confirm + /verify -----------------------------------------------------

// TestMFAConfirm_StampsEnrolledAndClearsMFARequired pins the
// happy path. Before /confirm the account is unenrolled; after
// /confirm mfa_enrolled_at is non-nil and mfa_required is
// false. The cookie re-issue is checked separately.
func TestMFAConfirm_StampsEnrolledAndClearsMFARequired(t *testing.T) {
	e := setupWithMFA(t, api.PlanPro, true, false)
	_, _, freshCookie := e.generateEnrolledAccount(t)

	// After /confirm the account row is enrolled and the policy
	// flag is cleared (MarkMFAEnrolled sets enrolled_at AND
	// clears mfa_required).
	after, err := e.store.AccountByID(context.Background(), e.acct.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !after.MFAEnrolled() {
		t.Errorf("MFAEnrolled = false, want true after /confirm")
	}
	if after.MFARequired {
		t.Errorf("MFARequired = true, want false after /confirm (MarkMFAEnrolled clears it)")
	}

	// The post-/confirm cookie must not be mfa_pending.
	env, err := e.mgr.Verify(freshCookie.Value)
	if err != nil {
		t.Fatalf("verify post-/confirm cookie: %v", err)
	}
	if env.MfaPending {
		t.Errorf("post-/confirm cookie is mfa_pending=true, want false")
	}
}

// TestMFAConfirm_RejectsBadCode: a wrong 6-digit code returns
// 401 CodeMFAInvalidCode and does NOT stamp enrolled_at.
func TestMFAConfirm_RejectsBadCode(t *testing.T) {
	e := setupWithMFA(t, api.PlanPro, false, false)
	// /enroll only, no /confirm.
	rec := e.do(t, http.MethodPost, "/v1/account/mfa/enroll", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("/enroll status %d", rec.Code)
	}

	badRec := e.do(t, http.MethodPost, "/v1/account/mfa/confirm", api.MFAConfirmRequest{Totp: "000000"})
	if badRec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", badRec.Code)
	}

	// And the account row is still unenrolled.
	after, _ := e.store.AccountByID(context.Background(), e.acct.ID)
	if after.MFAEnrolled() {
		t.Errorf("MFAEnrolled = true after failed /confirm, want false")
	}
}

// TestMFAVerify_StepsUpPendingCookie: with an mfa_pending=true
// cookie and an already-enrolled account, /verify clears the
// flag without re-stamping enrolled_at. The next protected
// route would pass through requireMFA — we exercise the cookie
// flag directly here.
func TestMFAVerify_StepsUpPendingCookie(t *testing.T) {
	e := setupWithMFA(t, api.PlanPro, false, false)
	// Drive an actual enrollment so the stored secret is real.
	_, secret, _ := e.generateEnrolledAccount(t)

	// Now flip the cookie to mfa_pending=true so we can exercise
	// the step-up path. The account is enrolled so /verify has a
	// real sealed secret to verify against.
	pendingCookie := e.mfaIssueWithPending(t, true)
	code, err := totp.GenerateCodeCustom(secret, time.Now().UTC(), totp.ValidateOpts{
		Period: 30, Digits: 6, Algorithm: 0, // SHA1 (matches VerifyCode)
	})
	if err != nil {
		t.Fatalf("GenerateCodeCustom: %v", err)
	}

	body, err := json.Marshal(api.MFAVerifyRequest{Totp: code})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/account/mfa/verify", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(pendingCookie)
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("/verify status %d: %s", rec.Code, rec.Body)
	}
	var stepUpCookie *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookie {
			stepUpCookie = c
		}
	}
	if stepUpCookie == nil {
		t.Fatalf("/verify did not re-issue cookie")
	}
	env, err := e.mgr.Verify(stepUpCookie.Value)
	if err != nil {
		t.Fatalf("verify post-/verify cookie: %v", err)
	}
	if env.MfaPending {
		t.Errorf("post-/verify cookie is mfa_pending=true, want false")
	}
}

// --- /recover + /disable ----------------------------------------------------

// TestMFARecover_ConsumesCodeAndReissuesCookie burns one of
// the 10 recovery codes and confirms:
//   - 200 OK with a fresh non-mfa_pending cookie.
//   - The account's mfa_recovery_codes_hash slice now has 9
//     entries (the consumed one is gone).
func TestMFARecover_ConsumesCodeAndReissuesCookie(t *testing.T) {
	e := setupWithMFA(t, api.PlanPro, false, false)
	recovCodes, _, _ := e.generateEnrolledAccount(t)
	pendingCookie := e.mfaIssueWithPending(t, true)

	// Use the first recovery code via the pending cookie.
	code := recovCodes[0]
	body, err := json.Marshal(api.MFARecoverRequest{Code: code})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/account/mfa/recover", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(pendingCookie)
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("/recover status %d: %s", rec.Code, rec.Body)
	}

	_, hashes, err := e.store.GetMFASecrets(context.Background(), e.acct.ID)
	if err != nil {
		t.Fatalf("GetMFASecrets: %v", err)
	}
	if len(hashes) != auth.RecoveryCodeCount-1 {
		t.Errorf("RecoveryCodes remaining = %d, want %d", len(hashes), auth.RecoveryCodeCount-1)
	}
}

// TestMFARecover_RejectsBadCode: an unknown recovery code
// returns 401 CodeMFAInvalidCode and does NOT consume a slot.
func TestMFARecover_RejectsBadCode(t *testing.T) {
	e := setupWithMFA(t, api.PlanPro, false, false)
	e.generateEnrolledAccount(t)

	rec := e.do(t, http.MethodPost, "/v1/account/mfa/recover", api.MFARecoverRequest{Code: "AAAAAAAAAA"})
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}

	_, hashes, _ := e.store.GetMFASecrets(context.Background(), e.acct.ID)
	if len(hashes) != auth.RecoveryCodeCount {
		t.Errorf("RecoveryCodes after bad burn = %d, want %d (unchanged)", len(hashes), auth.RecoveryCodeCount)
	}
}

// TestMFADisable_ByRecoveryCode: a fresh /disable with a
// recovery_code consumes one code and clears MFA state.
func TestMFADisable_ByRecoveryCode(t *testing.T) {
	e := setupWithMFA(t, api.PlanPro, false, false)
	recovCodes, _, _ := e.generateEnrolledAccount(t)

	rec := e.do(t, http.MethodPost, "/v1/account/mfa/disable",
		api.MFADisableRequest{RecoveryCode: recovCodes[0]})
	if rec.Code != http.StatusOK {
		t.Fatalf("/disable status %d: %s", rec.Code, rec.Body)
	}

	after, _ := e.store.AccountByID(context.Background(), e.acct.ID)
	if after.MFAEnrolled() {
		t.Errorf("MFAEnrolled = true after /disable, want false")
	}
}

// TestMFADisable_RejectsBadBody: exactly one of password /
// recovery_code is required. Empty + empty + both fail with
// 400 CodeValidation.
func TestMFADisable_RejectsBadBody(t *testing.T) {
	e := setupWithMFA(t, api.PlanPro, false, false)
	for _, body := range []api.MFADisableRequest{
		{},                                 // neither
		{Password: "p", RecoveryCode: "c"}, // both
	} {
		rec := e.do(t, http.MethodPost, "/v1/account/mfa/disable", body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("body %+v: status = %d, want 400", body, rec.Code)
		}
	}
}

// --- chokepoint helpers -----------------------------------------------------

// TestMFAFlipOnUpgrade covers the plan-upgrade chokepoint
// predicate directly. The chokepoint is wired into changePlan
// + the Stripe webhook; the test pins the predicate's truth
// table so a future edit can't quietly change the boundary
// (e.g. "what counts as crossing the paid threshold?").
func TestMFAFlipOnUpgrade(t *testing.T) {
	cases := []struct {
		old, newP api.Plan
		want      bool
	}{
		{api.PlanFree, api.PlanHobby, false},  // still no-card
		{api.PlanFree, api.PlanPro, true},     // crosses boundary
		{api.PlanFree, api.PlanScale, true},   // crosses boundary
		{api.PlanHobby, api.PlanPro, true},    // crosses boundary
		{api.PlanHobby, api.PlanScale, true},  // crosses boundary
		{api.PlanPro, api.PlanPro, false},     // no-op
		{api.PlanPro, api.PlanScale, false},   // already paid
		{api.PlanPro, api.PlanFree, false},    // downgrade (forbidden, but predicate returns false)
		{api.PlanScale, api.PlanHobby, false}, // downgrade
	}
	for _, c := range cases {
		got := mfaFlipOnUpgrade(c.old, c.newP)
		if got != c.want {
			t.Errorf("mfaFlipOnUpgrade(%s, %s) = %v, want %v", c.old, c.newP, got, c.want)
		}
	}
}

// TestMFAFlipOnDeploy pins the 2nd-deploy predicate. The
// argument is the count BEFORE the about-to-be-created deploy,
// so 0 = first deploy, 1 = about to become the 2nd.
func TestMFAFlipOnDeploy(t *testing.T) {
	cases := []struct {
		currentCount int
		want         bool
	}{
		{0, false}, // first deploy (about to become the 1st)
		{1, true},  // about to become the 2nd
		{2, true},  // already past the threshold
		{5, true},  // well past
	}
	for _, c := range cases {
		got := mfaFlipOnDeploy(c.currentCount)
		if got != c.want {
			t.Errorf("mfaFlipOnDeploy(%d) = %v, want %v", c.currentCount, got, c.want)
		}
	}
}

// TestFlipMFARequiredIfUnenrolled covers the chokepoint helper
// directly. An unenrolled account gets the flag set; an
// already-enrolled account is untouched.
//
// The helper is a method on *server, so we call it via the
// testEnv's srv field. The audit Emit fires against the in-
// memory auditor (newServerWithDeps wires noopNotifier but
// keeps the auditor — best-effort; we don't assert on
// auditor rows here, only on the mfa_required flag flip).
func TestFlipMFARequiredIfUnenrolled(t *testing.T) {
	t.Run("unenrolled_gets_required_true", func(t *testing.T) {
		e := setupWithMFA(t, api.PlanPro, false, false)
		ctx := context.Background()
		fresh, _ := e.store.AccountByID(ctx, e.acct.ID)
		e.srv.flipMFARequiredIfUnenrolled(ctx, fresh, "plan_upgrade",
			map[string]any{"from": "free", "to": "pro"})

		after, _ := e.store.AccountByID(ctx, e.acct.ID)
		if !after.MFARequired {
			t.Errorf("MFARequired = false after flip, want true")
		}
	})
	t.Run("already_enrolled_is_noop", func(t *testing.T) {
		e := setupWithMFA(t, api.PlanPro, false, true)
		ctx := context.Background()
		fresh, _ := e.store.AccountByID(ctx, e.acct.ID)
		e.srv.flipMFARequiredIfUnenrolled(ctx, fresh, "plan_upgrade", nil)

		after, _ := e.store.AccountByID(ctx, e.acct.ID)
		if after.MFARequired {
			t.Errorf("MFARequired = true after no-op flip on enrolled account")
		}
	})
}
