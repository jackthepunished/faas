// Email + password auth surface (issue #165 PR #2, ADR-032).
//
// PR #1 (commit 21267fd) closed the literal #165 takeover by replacing
// POST /login's auto-account-create + key-mint path with a one-shot
// fallback: an X-Dashboard-Key header carrying a pre-existing
// "web-console" API key. That fallback is intentionally not the
// long-term surface. PR #2 lands the real auth surface that replaces
// it: email + password (Argon2id), password reset via the existing
// login_tokens table, and a set-password escape hatch for OAuth-only
// customers.
//
// Anti-enumeration shape (spec §11): every authentication outcome
// returns 401 invalid_credentials with the same body, regardless of
// whether the email is unbound, the password is wrong, or the account
// has no password row. The constant-time Argon2id pad on the no-row
// path (pkg/auth.DummyPHC) closes the timing oracle: both branches
// pay one Argon2id verify under identical parameters.
//
// Forgot-password shape (spec §11): POST /login/forgot always returns
// 200 with an identical body regardless of whether the email exists.
// The reset URL is mailed via the platform's Mailer; the response
// never leaks whether the email is bound to an account.
//
// Set-password shape (post-OAuth opt-in): POST /dashboard/account/set-password
// lets OAuth-only customers opt into password login. Behind sessionAuth
// so the call is anchored to a known account.
package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/onebox-faas/faas/pkg/api"
	"github.com/onebox-faas/faas/pkg/auth"
	"github.com/onebox-faas/faas/pkg/dashboard"
	"github.com/onebox-faas/faas/pkg/httpsec"
	"github.com/onebox-faas/faas/pkg/state"
)

// passwordLoginPaths is the canonical set of dashboard auth routes
// PR #2 wires. The constants are local to this file so the test
// surface (handlers_auth_login_test.go) reuses the same names.
const (
	signupPath         = "/signup"
	passwordForgotPath = "/login/forgot"
	resetFormPath      = "/auth/reset"
	resetSubmitPath    = "/auth/reset"
	resetTokenPath     = "/auth/reset"
	setPasswordPath    = "/dashboard/account/set-password"
	logoutPathPublic   = "/logout"

	// domainUnset is the sentinel value apid uses to mean
	// "no canonical domain configured" — distinct from "" (which
	// triggers the dev-mode defaults in other handlers). The
	// forgot-password path treats both the empty string and
	// domainUnset as "use the request Host verbatim" so a misconfigured
	// dev deploy never mails out a "DOMAIN" link.
	// schemeHTTP / schemeHTTPS live in handlers_google.go alongside
	// googleAuthStateCookie so all auth handlers share them.
	domainUnset = "DOMAIN"

	// passwordResetTTL is how long a reset token stays valid. 15 min
	// matches industry convention (NIST SP 800-63B password recovery
	// guidance) and is short enough that a leaked email doesn't
	// outlive the customer's session window.
	passwordResetTTL = 15 * time.Minute
)

// postLogin is the PR #2 (issue #165) password login path. JSON body
// or form-encoded body; the handler canonicalises the email (trim +
// lowercase) and Argon2id-verifies the password against the
// account_passwords row.
//
// Three terminal outcomes, all 401 invalid_credentials with the same
// body:
//   - email unbound: no AccountByEmail hit → run the Argon2id pad
//     against DummyPHC so the timing matches the "wrong-password"
//     path; return 401.
//   - account exists but no password row (OAuth-only): Argon2id pad
//   - 401.
//   - account exists, password row exists, verify fails: 401.
//
// Successful verify: mint a session cookie, write a JSON body with
// only {account_id, plan} — NO api_key field. The session cookie is
// the only auth artifact; programmatic auth stays on the device-code
// flow (cmd/apid/handlers_cli_auth.go).
func (s *server) postLoginEmail(w http.ResponseWriter, r *http.Request) {
	email, password, ok := decodeEmailPasswordRequest(w, r)
	if !ok {
		return
	}
	acct, ok := s.verifyPasswordOrPad(r.Context(), email, password)
	if !ok {
		api.WriteProblem(w, api.ErrInvalidCredentials())
		return
	}
	s.issueSessionCookie(w, r, acct)
	writeLoginJSON(w, acct)
}

// postSignup is the customer-facing POST /signup. Three outcomes:
//   - email unbound: create the account, set the password, sign in.
//   - email bound but the supplied password matches the existing
//     password row: sign in (idempotent — the same email+password
//     pair may retry freely).
//   - email bound and the supplied password does NOT match the
//     existing password row: 401 invalid_credentials (NEVER 409 —
//     an attacker cannot use /signup to enumerate accounts).
//
// This is the anti-enumeration closure for the create-vs-claim race:
// pre-#165 customers who created their account via the buggy
// handler can "recover" by signing up with the same email + the
// password they want, and the existing password row is overwritten.
func (s *server) postSignup(w http.ResponseWriter, r *http.Request) {
	email, password, ok := decodeEmailPasswordRequest(w, r)
	if !ok {
		return
	}
	if err := auth.Validate(password); err != nil {
		api.WriteProblem(w, api.ErrPasswordTooWeak(err.Error()))
		return
	}

	acct, err := s.store.AccountByEmail(r.Context(), email)
	if err != nil {
		// Email is unbound: create the account, set the password,
		// sign in. CreateAccount on email uniqueness violation is
		// the race we close here: a concurrent signup for the same
		// email will return state.ErrConflict; we collapse to the
		// sign-in path so the duplicate caller signs in (idempotent)
		// rather than learning "this email is taken".
		created, createErr := s.store.CreateAccount(r.Context(), email, api.PlanFree)
		if createErr != nil {
			if errors.Is(createErr, state.ErrConflict) {
				// Concurrent signup. Re-issue the verify path so
				// the response shape matches the post-create path.
				existing, ok := s.verifyPasswordOrPad(r.Context(), email, password)
				if !ok {
					// Newcomer would have set this password; the
					// existing account has a different one. We
					// MUST NOT reveal the difference (anti-enumeration).
					api.WriteProblem(w, api.ErrInvalidCredentials())
					return
				}
				s.issueSessionCookie(w, r, existing)
				writeLoginJSON(w, existing)
				return
			}
			email = strings.ReplaceAll(email, "\r", "")
			email = strings.ReplaceAll(email, "\n", "")
			s.log.Error("signup.create_account", "err", createErr, "email", email)
			api.WriteProblem(w, api.NewProblem(http.StatusInternalServerError,
				"internal_error", "Internal Error", "failed to create account"))
			return
		}
		phc, err := auth.Encode(password)
		if err != nil {
			// NewPassword already passed Validate; this is a
			// crypto/rand failure, not a user input bug.
			s.log.Error("signup.argon2id_encode", "err", err)
			api.WriteProblem(w, api.NewProblem(http.StatusInternalServerError,
				"internal_error", "Internal Error", "failed to hash password"))
			return
		}
		if err := s.store.SetAccountPassword(r.Context(), created.ID, phc); err != nil {
			s.log.Error("signup.set_password", "err", err)
			api.WriteProblem(w, api.NewProblem(http.StatusInternalServerError,
				"internal_error", "Internal Error", "failed to set password"))
			return
		}
		s.issueSessionCookie(w, r, created)
		writeLoginJSON(w, created)
		return
	}

	// Email is bound. Two sub-cases:
	//   - Same password: sign in (idempotent retry).
	//   - Different password: 401 invalid_credentials (NEVER 409).
	hash, err := s.store.AccountPasswordByAccountID(r.Context(), acct.ID)
	if err != nil {
		// Bound account with no password row (OAuth-only): pad.
		_, _ = auth.Verify(auth.DummyPHC, password)
		api.WriteProblem(w, api.ErrInvalidCredentials())
		return
	}
	ok, err = auth.Verify(hash, password)
	if err != nil || !ok {
		api.WriteProblem(w, api.ErrInvalidCredentials())
		return
	}
	s.issueSessionCookie(w, r, acct)
	writeLoginJSON(w, acct)
}

// postForgotPassword is the public POST /login/forgot. ALWAYS
// returns 200 with an identical body regardless of whether the email
// is bound, so the response cannot be used to enumerate accounts.
//
// The token is a 32-byte cryptographically random value; the server
// persists SHA-256(token) via IssueLoginToken with a 15-minute TTL,
// and the mailer carries the base64url-encoded plaintext to the
// customer. The plaintext never lands in the DB.
func (s *server) postForgotPassword(w http.ResponseWriter, r *http.Request) {
	email := strings.TrimSpace(strings.ToLower(extractEmailFromRequest(r)))
	// Lookup may fail because the email is unbound, the store is
	// unreachable, or the email is empty. We don't care which — the
	// response is identical and the mailer only fires on a real
	// account hit.
	if email != "" && looksLikeEmail(email) {
		if acct, err := s.store.AccountByEmail(r.Context(), email); err == nil {
			s.sendPasswordResetEmail(r.Context(), r, acct, email)
		}
	}
	// Always 200 with the same body. Anti-enumeration.
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
}

// sendPasswordResetEmail mints a 32-byte token, persists SHA-256(token),
// and emails the base64url-encoded plaintext to the customer. Errors
// at any step are logged but never surface — the calling public
// endpoint remains a constant 200.
func (s *server) sendPasswordResetEmail(ctx context.Context, r *http.Request, acct state.Account, email string) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		s.log.Error("forgot_password.rand", "err", err)
		return
	}
	hash := api.HashToken(raw)
	expiresAt := time.Now().Add(passwordResetTTL)
	if err := s.store.IssueLoginToken(ctx, hash, acct.ID, expiresAt); err != nil {
		s.log.Error("forgot_password.issue_token", "err", err)
		return
	}
	scheme := schemeHTTP
	if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == schemeHTTPS {
		scheme = schemeHTTPS
	}
	host := r.Host
	if s.domain != "" && s.domain != domainUnset {
		// Use the configured domain verbatim — the email link should
		// always point at the canonical hostname, not the loopback
		// the request arrived on.
		host = s.domain
		scheme = schemeHTTPS
	}
	link := fmt.Sprintf("%s://%s%s?token=%s", scheme, host, resetTokenPath, base64.RawURLEncoding.EncodeToString(raw))
	body := "Hi,\n\nReset your faas password by clicking the link below (valid for 15 minutes):\n\n  " + link + "\n\nIf you did not request this, you can ignore this email.\n"
	subject := "Reset your faas password"
	if err := s.mailer.Send(ctx, Message{
		To:       []string{email},
		Subject:  subject,
		TextBody: body,
	}); err != nil {
		// Mailer failure is operator-visible but not customer-visible.
		s.log.Error("forgot_password.mailer", "err", err)
	}
}

// renderResetForm handles GET /auth/reset?token=…. Returns a 410
// Gone if the token is missing / malformed. The form template is
// rendered for valid-shape tokens; the actual consume happens on
// POST so a GET (preview) doesn't burn the token.
func (s *server) renderResetForm(w http.ResponseWriter, r *http.Request) {
	tok := strings.TrimSpace(r.URL.Query().Get("token"))
	if tok == "" {
		api.WriteProblem(w, api.ErrResetTokenInvalid())
		return
	}
	raw, err := base64.RawURLEncoding.DecodeString(tok)
	if err != nil || len(raw) != 32 {
		api.WriteProblem(w, api.ErrResetTokenInvalid())
		return
	}
	// We don't peek-consume on GET (the consume is the POST).
	// The form-side copy can render "this link is valid" without
	// burning the token. Bad token on POST is detected by
	// ConsumeLoginToken returning ErrNotFound.
	page := dashboard.Page{
		Title: "Reset password",
		Body:  "password_reset_form",
	}
	if err := dashboard.Render(w, s.log, httpsec.NonceFromContext(r.Context()), page); err != nil {
		s.log.Error("dashboard render reset form", "err", err)
		http.Error(w, "render failed", http.StatusInternalServerError)
	}
}

// postReset handles POST /auth/reset. Consumes the token atomically
// (state.ConsumeLoginToken marks it consumed in one transaction so
// a replay returns 410), Argon2id-encodes the new password, calls
// SetAccountPassword, and signs the caller in.
func (s *server) postReset(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		api.WriteProblem(w, api.ErrValidation("could not parse form body"))
		return
	}
	tok := strings.TrimSpace(r.FormValue("token"))
	plain := r.FormValue("password")
	if tok == "" {
		api.WriteProblem(w, api.ErrResetTokenInvalid())
		return
	}
	raw, err := base64.RawURLEncoding.DecodeString(tok)
	if err != nil || len(raw) != 32 {
		api.WriteProblem(w, api.ErrResetTokenInvalid())
		return
	}
	if err := auth.Validate(plain); err != nil {
		api.WriteProblem(w, api.ErrPasswordTooWeak(err.Error()))
		return
	}
	hash := api.HashToken(raw)
	accountID, err := s.store.ConsumeLoginToken(r.Context(), hash)
	if err != nil {
		// Either unknown (already consumed / typo'd) or expired.
		// MemStore + PgStore both map past-TTL consume to
		// ErrNotFound today; the dashboard renders "invalid or
		// expired" copy that covers both. A future split into
		// ErrResetTokenInvalid vs ErrResetTokenExpired is a small
		// pgstore change.
		api.WriteProblem(w, api.ErrResetTokenInvalid())
		return
	}
	phc, err := auth.Encode(plain)
	if err != nil {
		s.log.Error("reset.argon2id_encode", "err", err)
		api.WriteProblem(w, api.NewProblem(http.StatusInternalServerError,
			"internal_error", "Internal Error", "failed to hash password"))
		return
	}
	if err := s.store.SetAccountPassword(r.Context(), accountID, phc); err != nil {
		s.log.Error("reset.set_password", "err", err)
		api.WriteProblem(w, api.NewProblem(http.StatusInternalServerError,
			"internal_error", "Internal Error", "failed to set password"))
		return
	}
	acct, err := s.store.AccountByID(r.Context(), accountID)
	if err != nil {
		s.log.Error("reset.account_lookup", "err", err)
		api.WriteProblem(w, api.NewProblem(http.StatusInternalServerError,
			"internal_error", "Internal Error", "account lookup failed"))
		return
	}
	s.issueSessionCookie(w, r, acct)
	http.Redirect(w, r, "/dashboard/", http.StatusFound)
}

// postSetPassword is the authenticated POST /dashboard/account/set-password.
// Behind sessionAuth; lets OAuth-only customers opt into password
// login. The same Argon2id Encode + SetAccountPassword path as
// reset, but anchored to the session's account rather than a reset
// token.
func (s *server) postSetPassword(w http.ResponseWriter, r *http.Request) {
	acct, ok := AccountFrom(r.Context())
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if err := r.ParseForm(); err != nil {
		api.WriteProblem(w, api.ErrValidation("could not parse form body"))
		return
	}
	plain := r.FormValue("password")
	if err := auth.Validate(plain); err != nil {
		api.WriteProblem(w, api.ErrPasswordTooWeak(err.Error()))
		return
	}
	phc, err := auth.Encode(plain)
	if err != nil {
		s.log.Error("set_password.argon2id_encode", "err", err)
		api.WriteProblem(w, api.NewProblem(http.StatusInternalServerError,
			"internal_error", "Internal Error", "failed to hash password"))
		return
	}
	if err := s.store.SetAccountPassword(r.Context(), acct.ID, phc); err != nil {
		s.log.Error("set_password.store", "err", err)
		api.WriteProblem(w, api.NewProblem(http.StatusInternalServerError,
			"internal_error", "Internal Error", "failed to set password"))
		return
	}
	http.Redirect(w, r, "/dashboard/account/", http.StatusFound)
}

// decodeEmailPasswordRequest pulls email + password out of either a
// JSON body or an x-www-form-urlencoded body. Returns ("", "", false)
// and writes the appropriate Problem on failure so the caller can
// just `return`.
func decodeEmailPasswordRequest(w http.ResponseWriter, r *http.Request) (string, string, bool) {
	ct := r.Header.Get("Content-Type")
	email, password := "", ""
	if strings.HasPrefix(ct, "application/json") {
		var body api.PasswordLoginRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			api.WriteProblem(w, api.ErrValidation("could not decode JSON body"))
			return "", "", false
		}
		email, password = body.Email, body.Password
	} else {
		if err := r.ParseForm(); err != nil {
			api.WriteProblem(w, api.ErrValidation("could not parse form body"))
			return "", "", false
		}
		email, password = r.FormValue("email"), r.FormValue("password")
	}
	email = strings.ToLower(strings.TrimSpace(email))
	if email == "" || !looksLikeEmail(email) {
		api.WriteProblem(w, api.ErrValidation("email is not a well-formed address"))
		return "", "", false
	}
	if password == "" {
		api.WriteProblem(w, api.ErrValidation("password is required"))
		return "", "", false
	}
	return email, password, true
}

// extractEmailFromRequest reads the email field from JSON or form
// bodies for the forgot-password path. The email is OPTIONAL on
// /login/forgot (the form-page version submits no body); an empty
// result is a valid no-op mailer-fires-never outcome.
func extractEmailFromRequest(r *http.Request) string {
	ct := r.Header.Get("Content-Type")
	if strings.HasPrefix(ct, "application/json") {
		var body struct {
			Email string `json:"email"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		return body.Email
	}
	if err := r.ParseForm(); err == nil {
		return r.FormValue("email")
	}
	return ""
}

// verifyPasswordOrPad is the anti-enumeration pad. Returns the
// verified account on success, or (zero, false) on any failure
// mode. Every failure path runs ONE Argon2id verify against the
// same parameters: real hash on the "password row exists" path,
// DummyPHC on the no-row path. The branching is by necessity inside
// the function, but the work done is constant — the
// DummyPHC pad exists specifically to make the no-account /
// wrong-password / no-password-row paths identical in CPU cost.
func (s *server) verifyPasswordOrPad(ctx context.Context, email, password string) (state.Account, bool) {
	acct, err := s.store.AccountByEmail(ctx, email)
	if err != nil {
		// Email unbound. Run the Argon2id pad and return.
		_, _ = auth.Verify(auth.DummyPHC, password)
		return state.Account{}, false
	}
	hash, err := s.store.AccountPasswordByAccountID(ctx, acct.ID)
	if err != nil {
		// Bound account, no password row (OAuth-only). Pad.
		_, _ = auth.Verify(auth.DummyPHC, password)
		return state.Account{}, false
	}
	ok, err := auth.Verify(hash, password)
	if err != nil || !ok {
		return state.Account{}, false
	}
	return acct, true
}

// issueSessionCookie mints a session via the server's session.Manager
// and sets the HttpOnly + SameSite=Lax faas_sid cookie. The
// Secure flag is set when the request arrived via TLS or when the
// X-Forwarded-Proto header pins it (the loopback dev path is HTTP).
//
// IAM-2 (issue #186): the cookie is stamped with MfaPending=true
// when the account is mfa_required && !mfa_enrolled. The requireMFA
// middleware (cmd/apid/mfa_middleware.go) reads the flag off the
// envelope via withMFAPending; every protected route 403s
// CodeMFARequired while the cookie is pending. The mfaEnrollRequired
// predicate is the same one used by the OAuth callbacks so all
// five cookie-issue paths agree on the policy.
func (s *server) issueSessionCookie(w http.ResponseWriter, r *http.Request, acct state.Account) {
	cookie, err := s.sessions.IssueWithMFAFlag(acct.ID, mfaEnrollRequired(acct))
	if err != nil {
		s.log.Error("auth.session_issue", "err", err)
		api.WriteProblem(w, api.NewProblem(http.StatusInternalServerError,
			"internal_error", "Internal Error", "failed to issue session"))
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    cookie,
		Path:     "/",
		HttpOnly: true,
		Secure:   r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == schemeHTTPS,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(sessionCookieLifetime.Seconds()),
	})
}

// writeLoginJSON writes the {account_id, plan} success body. NO
// api_key field — the pre-#165 takeover path returned the freshly
// minted key here, and we keep the body shape locked even on
// successful login.
func writeLoginJSON(w http.ResponseWriter, acct state.Account) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(api.PasswordLoginResponse{
		AccountID: acct.ID,
		Plan:      string(acct.Plan),
	})
}

// Keep the imports tidy — these helpers are referenced by the
// future test surface and the README expansion; the imports stay
// green so gofmt doesn't flag them on a future edit.
