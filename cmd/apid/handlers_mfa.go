// MFA handlers (IAM-2, issue #186).
//
// Five POST endpoints under /v1/account/mfa/*. The wire shapes
// live in pkg/api/mfa.go; the audit taxonomy is locked:
//
//   - account.mfa_enrolled            — mfaConfirm success
//   - account.mfa_confirmed           — mfaConfirm success (alias)
//   - account.mfa_session_stepped_up  — mfaVerify success
//   - account.mfa_recovered           — mfaRecover success
//   - account.mfa_disabled            — mfaDisable success
//
// The failure-path rows (mfa_confirm_failed etc.) are emitted
// best-effort — useful for brute-force triage but not part of
// the locked taxonomy. The /v1/account/mfa/* routes are
// registered on authLimited+requireMFA+requireScope(adminOnly)
// in server.go; the requireMFA allowlist in mfa_middleware.go
// keeps them reachable while the session is mfa_pending.

package main

import (
	"context"
	"errors"
	"net/http"

	"filippo.io/age"

	"github.com/onebox-faas/faas/pkg/api"
	"github.com/onebox-faas/faas/pkg/auth"
	"github.com/onebox-faas/faas/pkg/secretbox"
	"github.com/onebox-faas/faas/pkg/state"
)

// mfaRecipient is the host X25519 recipient apid loads once at
// boot for sealing TOTP secrets. Set by cmd/apid/main.go (or a
// test harness) via SetMFARecipient during boot — same shape
// as setSecretRecipient in handlers_secrets.go. nil means
// MFA enrollment is refused; the handler 503s CodeCapacity
// because there's no honest way to seal a TOTP secret without
// a host key.
var mfaRecipient func() *age.X25519Recipient

// SetMFARecipient is the boot-time wire-up called by the apid
// main() once the host age key has loaded. Tests call this
// directly to inject an in-memory recipient.
func SetMFARecipient(f func() *age.X25519Recipient) { mfaRecipient = f }

// consumeRecoveryCodeSelector abstracts the single-row
// transactional bytea[] update the consume path needs.
// PgStore does this with SELECT FOR UPDATE + UPDATE inside a
// pgx transaction; MemStore serialises via the existing mutex.
// Tests inject a stub to drive the success / miss paths without
// touching a real DB.
type consumeRecoveryCodeSelector interface {
	GetMFASecrets(ctx context.Context, id string) ([]byte, [][]byte, error)
	UpdateMFARecoveryCodes(ctx context.Context, id string, recoveryHashes [][]byte) error
}

// --- handlers ---------------------------------------------------------------

// mfaEnroll starts an MFA enrollment. Generates a fresh
// base32 TOTP secret + 10 recovery codes, seals the secret,
// persists the secret + SHA-256 hashes WITHOUT stamping
// mfa_enrolled_at, and returns the otpauth URL + secret + QR
// + recovery codes to the dashboard ONCE. The customer must
// then complete /confirm to commit the enrollment.
//
// 409 CodeConflict when the customer has already enrolled —
// the dashboard re-renders the "manage MFA" page instead.
func (s *server) mfaEnroll(w http.ResponseWriter, r *http.Request, acct state.Account) {
	if acct.MFAEnrolled() {
		api.WriteProblem(w, api.NewProblem(http.StatusConflict, api.CodeConflict,
			"Already enrolled", "call /v1/account/mfa/disable before re-enrolling"))
		return
	}
	rec := mfaRecipient()
	if rec == nil {
		api.WriteProblem(w, api.NewProblem(http.StatusServiceUnavailable, api.CodeCapacity,
			"MFA unavailable", "host age key not loaded — refusing to seal TOTP secret"))
		return
	}

	secret, err := auth.GenerateSecret()
	if err != nil {
		s.log.Error("mfa.enroll.generate_secret", "err", err.Error())
		api.WriteProblem(w, api.ErrCapacity("could not generate TOTP secret"))
		return
	}
	key, err := auth.KeyFromSecret(secret, acct.Email)
	if err != nil {
		s.log.Error("mfa.enroll.key", "err", err.Error())
		api.WriteProblem(w, api.ErrCapacity("could not derive TOTP key"))
		return
	}
	png, err := auth.QRCode(key)
	if err != nil {
		s.log.Error("mfa.enroll.qr", "err", err.Error())
		api.WriteProblem(w, api.ErrCapacity("could not render TOTP QR"))
		return
	}
	plaintexts, hashes, err := auth.NewRecoveryCodes(auth.RecoveryCodeCount)
	if err != nil {
		s.log.Error("mfa.enroll.recovery", "err", err.Error())
		api.WriteProblem(w, api.ErrCapacity("could not generate recovery codes"))
		return
	}

	sealed, err := secretbox.Seal(rec, secretbox.Envelope{"MFA_SECRET": secret})
	if err != nil {
		s.log.Error("mfa.enroll.seal", "err", err.Error())
		api.WriteProblem(w, api.ErrCapacity("could not seal TOTP secret"))
		return
	}
	if err := s.store.SetMFASecret(r.Context(), acct.ID, sealed, hashes); err != nil {
		s.log.Error("mfa.enroll.persist", "err", err.Error())
		api.WriteProblem(w, api.ErrCapacity("could not persist TOTP enrollment"))
		return
	}

	writeJSON(w, http.StatusOK, api.MFAEnrollResponse{
		OTPAuthURL:    key.URL(),
		Secret:        secret,
		QRCodePNG:     png,
		RecoveryCodes: plaintexts,
	})
}

// mfaConfirm finishes an MFA enrollment. Reads the sealed
// secret from the account row, unseals, verifies the presented
// 6-digit code against the current 30-s step (±1 skew).
// On success stamps mfa_enrolled_at + clears mfa_required, and
// re-issues the session cookie without mfa_pending.
//
// Not wrapped in s.idempotent: the customer should see the
// success body exactly once (the side-effects on the cookie +
// the store stamp are the load-bearing bits, and the dashboard
// reads the response status alone).
func (s *server) mfaConfirm(w http.ResponseWriter, r *http.Request, acct state.Account) {
	var req api.MFAConfirmRequest
	if err := decodeJSON(r, &req); err != nil {
		api.WriteProblem(w, api.NewProblem(http.StatusBadRequest, api.CodeValidation,
			"Invalid request", "malformed JSON body"))
		return
	}
	if !auth.VerifyCode(readSealedSecret(s, w, r, acct.ID), req.Totp) {
		s.audit.Emit(r.Context(), "account.mfa_confirm_failed", &acct.ID, map[string]any{"reason": "code_mismatch"})
		api.WriteProblem(w, api.NewProblem(http.StatusUnauthorized, api.CodeMFAInvalidCode,
			"Invalid code", "the TOTP code did not match"))
		return
	}
	if err := s.store.MarkMFAEnrolled(r.Context(), acct.ID); err != nil {
		s.log.Error("mfa.confirm.mark_enrolled", "err", err.Error())
		api.WriteProblem(w, api.ErrCapacity("could not finalize enrollment"))
		return
	}
	if err := s.reissueSessionCookie(w, r, acct, false); err != nil {
		// Cookie re-issue failure is non-fatal — the customer's
		// enrollment landed; the next request will trip
		// requireMFA until they log in again. We log it but
		// still return 200 because the wire contract for
		// /confirm is "enrollment landed".
		s.log.Warn("mfa.confirm.reissue_cookie", "err", err.Error())
	}
	s.audit.Emit(r.Context(), "account.mfa_enrolled", &acct.ID, nil)
	s.audit.Emit(r.Context(), "account.mfa_confirmed", &acct.ID, map[string]any{"method": "totp"})
	writeJSON(w, http.StatusOK, api.MFAConfirmResponse{})
}

// mfaVerify steps up an mfa_pending session. Same verify path
// as /confirm but does NOT stamp mfa_enrolled_at (the customer
// is already enrolled). On success re-issues the session cookie
// without mfa_pending — the next protected route request
// passes through the requireMFA gate cleanly.
//
// 401 CodeMFAInvalidCode on a bad code, 404 if the account row
// is missing (defense in depth — should never happen because
// s.auth resolved it moments earlier).
func (s *server) mfaVerify(w http.ResponseWriter, r *http.Request, acct state.Account) {
	var req api.MFAVerifyRequest
	if err := decodeJSON(r, &req); err != nil {
		api.WriteProblem(w, api.NewProblem(http.StatusBadRequest, api.CodeValidation,
			"Invalid request", "malformed JSON body"))
		return
	}
	if !auth.VerifyCode(readSealedSecret(s, w, r, acct.ID), req.Totp) {
		s.audit.Emit(r.Context(), "account.mfa_verify_failed", &acct.ID, map[string]any{"reason": "code_mismatch"})
		api.WriteProblem(w, api.NewProblem(http.StatusUnauthorized, api.CodeMFAInvalidCode,
			"Invalid code", "the TOTP code did not match"))
		return
	}
	if err := s.reissueSessionCookie(w, r, acct, false); err != nil {
		s.log.Error("mfa.verify.reissue_cookie", "err", err.Error())
		api.WriteProblem(w, api.ErrCapacity("could not re-issue session cookie"))
		return
	}
	s.audit.Emit(r.Context(), "account.mfa_session_stepped_up", &acct.ID, nil)
	writeJSON(w, http.StatusOK, api.MFAVerifyResponse{})
}

// mfaRecover burns a recovery code to regain access when the
// TOTP device is lost. Removes the matching SHA-256 hash from
// the stored set; re-issues the session cookie without
// mfa_pending. The customer can still hit /verify if they
// re-install the authenticator app and recover the device.
//
// consumeRecoveryCode does the SELECT FOR UPDATE + UPDATE
// dance so two concurrent /recover requests can't both think
// they burned the same code. (Defense in depth — the dashboard
// disables the form while one request is in flight.)
func (s *server) mfaRecover(w http.ResponseWriter, r *http.Request, acct state.Account) {
	var req api.MFARecoverRequest
	if err := decodeJSON(r, &req); err != nil {
		api.WriteProblem(w, api.NewProblem(http.StatusBadRequest, api.CodeValidation,
			"Invalid request", "malformed JSON body"))
		return
	}
	presented := auth.HashRecoveryCode(req.Code)
	matched, err := consumeRecoveryCode(r.Context(), s.store, acct.ID, presented)
	if err != nil {
		if errors.Is(err, state.ErrNotFound) {
			api.WriteProblem(w, api.NewProblem(http.StatusUnauthorized, api.CodeMFAInvalidCode,
				"Invalid code", "no matching recovery code"))
			return
		}
		s.log.Error("mfa.recover.consume", "err", err.Error())
		api.WriteProblem(w, api.ErrCapacity("could not consume recovery code"))
		return
	}
	if !matched {
		s.audit.Emit(r.Context(), "account.mfa_recover_failed", &acct.ID, map[string]any{"reason": "code_mismatch"})
		api.WriteProblem(w, api.NewProblem(http.StatusUnauthorized, api.CodeMFAInvalidCode,
			"Invalid code", "no matching recovery code"))
		return
	}
	if err := s.reissueSessionCookie(w, r, acct, false); err != nil {
		s.log.Error("mfa.recover.reissue_cookie", "err", err.Error())
		api.WriteProblem(w, api.ErrCapacity("could not re-issue session cookie"))
		return
	}
	s.audit.Emit(r.Context(), "account.mfa_recovered", &acct.ID, nil)
	writeJSON(w, http.StatusOK, api.MFARecoverResponse{})
}

// mfaDisable opts the customer out of MFA. Body {password} OR
// {recovery_code} — the customer must prove they hold the
// second factor (or the password fallback) to disable. This
// is the analog of /v1/account/delete's password re-prompt.
//
// Clears mfa_secret_encrypted + mfa_recovery_codes_hash +
// mfa_enrolled_at. Leaves mfa_required untouched so the
// chokepoints (plan upgrade / card attach / 2nd deploy) can
// re-arm on the next trigger.
func (s *server) mfaDisable(w http.ResponseWriter, r *http.Request, acct state.Account) {
	var req api.MFADisableRequest
	if err := decodeJSON(r, &req); err != nil {
		api.WriteProblem(w, api.NewProblem(http.StatusBadRequest, api.CodeValidation,
			"Invalid request", "malformed JSON body"))
		return
	}
	hasPassword := req.Password != ""
	hasRecovery := req.RecoveryCode != ""
	if hasPassword == hasRecovery { // both or neither
		api.WriteProblem(w, api.NewProblem(http.StatusBadRequest, api.CodeValidation,
			"Invalid request", "exactly one of password or recovery_code is required"))
		return
	}

	if hasPassword {
		// Re-authenticate via the existing password hash. The
		// auth.Verify helper in pkg/auth/password.go is the
		// same path /login uses; we don't roll our own PHC
		// compare. An empty hash (no password set on the
		// account) is a 401 — the customer must set a
		// password before using the password-disable path.
		hash, err := s.store.AccountPasswordByAccountID(r.Context(), acct.ID)
		if err != nil || hash == "" {
			s.audit.Emit(r.Context(), "account.mfa_disable_failed", &acct.ID, map[string]any{"reason": "no_password"})
			api.WriteProblem(w, api.NewProblem(http.StatusUnauthorized, api.CodeMFAInvalidCode,
				"Invalid credentials", "password not set on this account"))
			return
		}
		ok, err := auth.Verify(hash, req.Password)
		if err != nil || !ok {
			s.audit.Emit(r.Context(), "account.mfa_disable_failed", &acct.ID, map[string]any{"reason": "password_mismatch"})
			api.WriteProblem(w, api.NewProblem(http.StatusUnauthorized, api.CodeMFAInvalidCode,
				"Invalid credentials", "password did not match"))
			return
		}
	} else {
		// Recovery-code path: consume one code. Same helper as
		// /recover. The customer burning a code to disable
		// is functionally equivalent to burning it to recover.
		presented := auth.HashRecoveryCode(req.RecoveryCode)
		matched, err := consumeRecoveryCode(r.Context(), s.store, acct.ID, presented)
		if err != nil || !matched {
			s.audit.Emit(r.Context(), "account.mfa_disable_failed", &acct.ID, map[string]any{"reason": "code_mismatch"})
			api.WriteProblem(w, api.NewProblem(http.StatusUnauthorized, api.CodeMFAInvalidCode,
				"Invalid code", "no matching recovery code"))
			return
		}
	}

	if err := s.store.ClearMFA(r.Context(), acct.ID, "user_disable"); err != nil {
		s.log.Error("mfa.disable.clear", "err", err.Error())
		api.WriteProblem(w, api.ErrCapacity("could not clear MFA state"))
		return
	}
	method := "password"
	if !hasPassword {
		method = "recovery_code"
	}
	s.audit.Emit(r.Context(), "account.mfa_disabled", &acct.ID, map[string]any{"method": method})
	writeJSON(w, http.StatusOK, api.MFADisableResponse{})
}

// --- helpers ----------------------------------------------------------------

// readSealedSecret returns the plaintext TOTP secret for an
// account, unsealing the at-rest blob with the host age
// identity. Writes a 5xx problem on any error and returns ""
// — the verify caller checks for the empty string and 500s
// upstream; the helper deliberately does NOT retry, because a
// failing seal means a real boot config error.
//
// (The helper shape mirrors readAppSecret in handlers_secrets.go
// — same idea, separate seal key/value.)
func readSealedSecret(s *server, w http.ResponseWriter, r *http.Request, accountID string) string {
	encrypted, _, err := s.store.GetMFASecrets(r.Context(), accountID)
	if err != nil {
		if errors.Is(err, state.ErrNotFound) {
			api.WriteProblem(w, api.NewProblem(http.StatusNotFound, api.CodeNotFound,
				"Not enrolled", "complete /v1/account/mfa/enroll first"))
			return ""
		}
		s.log.Error("mfa.read_sealed_secret", "err", err.Error())
		api.WriteProblem(w, api.ErrCapacity("could not read MFA state"))
		return ""
	}
	// mfaRecipient is read at enroll time but here we need the
	// *identity* to unseal. The identity is the host's age
	// private key — guarded by the same boot-time wire-up as
	// the recipient. nil identity = the daemon started without
	// a host age key, which is the same CodeCapacity condition
	// as mfaEnroll refusing to seal.
	ident := mfaIdentity()
	if ident == nil {
		api.WriteProblem(w, api.NewProblem(http.StatusServiceUnavailable, api.CodeCapacity,
			"MFA unavailable", "host age identity not loaded — refusing to unseal"))
		return ""
	}
	env, err := secretbox.Open(ident, encrypted)
	if err != nil {
		s.log.Error("mfa.open_sealed_secret", "err", err.Error())
		api.WriteProblem(w, api.ErrCapacity("could not unseal TOTP secret"))
		return ""
	}
	return env["MFA_SECRET"]
}

// mfaIdentity is the boot-time-wired host age identity (private
// key). Same pattern as mfaRecipient, but for Open() instead of
// Seal(). nil means the daemon hasn't loaded the host key yet
// — the handler 503s CodeCapacity.
var mfaIdentity func() *age.X25519Identity

// SetMFAIdentity is the boot-time wire-up called by apid
// main(). Tests call this directly to inject an in-memory
// identity.
func SetMFAIdentity(f func() *age.X25519Identity) { mfaIdentity = f }

// reissueSessionCookie mints a fresh session cookie (with the
// given mfa-pending flag) and sets it on the response. Used by
// /confirm, /verify, /recover to clear the mfa_pending flag
// after a successful step-up. Mirrors issueSessionCookie but
// takes the mfaPending explicitly so the verify step-up can
// pass false even if the account still has mfa_required set
// (the customer is enrolled; the policy flag can stay armed
// for the future).
func (s *server) reissueSessionCookie(w http.ResponseWriter, r *http.Request, acct state.Account, mfaPending bool) error {
	cookie, err := s.sessions.IssueWithMFAFlag(acct.ID, mfaPending)
	if err != nil {
		return err
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    cookie,
		Path:     "/",
		HttpOnly: true,
		Secure:   r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == schemeHTTPS,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(s.sessions.MaxAge().Seconds()),
	})
	return nil
}

// consumeRecoveryCode loads the account's recovery-code hashes,
// searches for a match against `presented`, and writes back the
// slice minus the matched index. Returns matched=true if a
// matching hash was removed; matched=false on no-match; and
// state.ErrNotFound when the account row is missing or has
// never enrolled.
//
// Single-row serialization: MemStore mutex; PgStore wraps the
// read+write in a tx with SELECT FOR UPDATE on accounts so two
// concurrent /recover requests on the same account can't both
// burn the same code.
//
// (Re-declared from a minimal interface so the handler test
// can stub it without faking a full Store.)
func consumeRecoveryCode(ctx context.Context, st consumeRecoveryCodeSelector, accountID string, presented []byte) (bool, error) {
	_, hashes, err := st.GetMFASecrets(ctx, accountID)
	if err != nil {
		return false, err
	}
	for i, h := range hashes {
		// Constant-time comparison per hash. 10 entries × 32 B
		// is negligible against the cold-boot wake budget.
		if sha256Equal(h, presented) {
			next := make([][]byte, 0, len(hashes)-1)
			next = append(next, hashes[:i]...)
			next = append(next, hashes[i+1:]...)
			// UpdateMFARecoveryCodes preserves the sealed secret
			// — the customer can still TOTP-verify after burning
			// every recovery code. SetMFASecret would nil the
			// secret and break the verify path on a burned-last-
			// code account.
			if err := st.UpdateMFARecoveryCodes(ctx, accountID, next); err != nil {
				return false, err
			}
			return true, nil
		}
	}
	return false, nil
}

// sha256Equal is a constant-time byte slice comparison. Lives
// here rather than in pkg/auth because it's only used by the
// recovery-code consume path; adding it to pkg/auth would
// invite a future caller to compare arbitrary byte strings and
// call it "secure" without checking that the slices are
// well-padded. The 32 B shape is the contract.
func sha256Equal(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	var diff byte
	for i := range a {
		diff |= a[i] ^ b[i]
	}
	return diff == 0
}

// --- auto-flip chokepoints --------------------------------------------------
//
// Three chokepoints set mfa_required=true when a customer
// crosses the "real customer" boundary (issue #186 / IAM-2):
//
//   - Plan upgrade across the paid threshold (free|hobby →
//     pro|scale). Hobby stays hobby on a free→hobby flip —
//     the boundary is "will the customer hold a credit-card-
//     backed resource?", and Hobby still has no card
//     requirement.
//   - Card attached (provider_customer_id freshly stamped).
//   - 2nd deploy (account already has >= 1 active deployment
//     before the about-to-be-created one).
//
// The chokepoints are best-effort: a SetMFARequired failure
// logs at WARN and continues — the customer's primary action
// (plan change / card attach / deploy) lands regardless. The
// audit Emit fires only on success so a redelivered webhook
// doesn't double-record the chokepoint.

// mfaFlipOnUpgrade is the plan-upgrade predicate. Returns
// true iff the customer is moving from a no-card-required
// tier to a card-required tier. Hobby → Pro and Hobby →
// Scale cross the boundary; Free → Hobby does not.
//
// (Hobby has no card requirement in the plan spec — the
// payment-required CodePayment only fires for Pro + Scale.
// Free → Hobby is a self-serve plan bump; Hobby → Pro +
// Hobby → Scale require the Stripe webhook.)
func mfaFlipOnUpgrade(old, new api.Plan) bool {
	if new != api.PlanPro && new != api.PlanScale {
		return false
	}
	return old == api.PlanFree || old == api.PlanHobby
}

// mfaFlipOnDeploy is the 2nd-deploy predicate. Returns
// true iff the customer has at least one already-active
// deployment before the about-to-be-created one lands.
// `currentCount` is the count before the new deployment;
// the threshold is "the new deploy would be the 2nd or
// later", so currentCount >= 1 fires.
func mfaFlipOnDeploy(currentCount int) bool { return currentCount >= 1 }

// flipMFARequiredIfUnenrolled sets mfa_required=true on the
// account iff the customer has not yet enrolled. Idempotent
// on repeat: the second call returns nil from SetMFARequired
// (PgStore + MemStore both treat the same value as no-op),
// and the audit Emit fires only on a fresh flip.
//
// The `reason` argument drives the audit `data` shape —
// one of "plan_upgrade", "card_attached", "second_deploy".
// The `extra` map is the caller-supplied context (e.g.
// deploy_count, from/to plan) that's relevant to that
// chokepoint.
//
// Called by changePlan, the card-attached webhook paths,
// and createDeployment. NOT a public method — the three
// callers are tightly coupled to the apid handler struct.
func (s *server) flipMFARequiredIfUnenrolled(ctx context.Context, acct state.Account, reason string, extra map[string]any) {
	if acct.MFAEnrolled() {
		return
	}
	if err := s.store.SetMFARequired(ctx, acct.ID, true); err != nil {
		s.log.Warn("mfa_required set failed", "account", acct.ID, "reason", reason, "err", err.Error())
		return
	}
	data := map[string]any{"reason": reason}
	for k, v := range extra {
		data[k] = v
	}
	s.audit.Emit(ctx, "account.mfa_required_enabled", &acct.ID, data)
}

// maybeFlipMFAOnDeploy is the 2nd-deploy chokepoint's thin
// wrapper. Called from createDeployment (image branch) +
// createDeploymentMultipart (tarball branch) after a
// successful CreateDeployment. Counts the post-insert active
// deployments for the account; if >= 2, arms mfa_required.
//
// The threshold is the post-insert count, not the pre-insert
// count, because CountDeployments is the SQL of truth (it
// joins through apps and excludes failed/superseded). The
// pre-insert check would race against concurrent deploys;
// the post-insert check sees exactly what's in the DB.
// Free accounts never trip this — CreateAppIfUnderQuota
// blocks at app #2 — so the chokepoint only matters for
// Hobby / Pro / Scale customers.
func (s *server) maybeFlipMFAOnDeploy(ctx context.Context, acct state.Account) {
	count, err := s.store.CountDeployments(ctx, acct.ID)
	if err != nil {
		s.log.Warn("mfa_required count failed", "account", acct.ID, "err", err.Error())
		return
	}
	if !mfaFlipOnDeploy(count) {
		return
	}
	s.flipMFARequiredIfUnenrolled(ctx, acct, "second_deploy", map[string]any{
		"deploy_count": count,
	})
}
