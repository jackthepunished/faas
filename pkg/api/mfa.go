// MFA wire shapes (IAM-2, issue #186).
//
// The MFA flow is five POST endpoints under /v1/account/mfa/*:
//
//   - /enroll  — start enrollment. Returns the otpauth URL +
//                secret + QR + recovery codes ONCE. The dashboard
//                renders the QR + recovery-code list; the
//                plaintext secret is also returned so the
//                customer's authenticator app can ingest the URL
//                by copy-paste.
//
//   - /confirm — finish enrollment. Body {totp}. Verifies the
//                code against the sealed secret, stamps
//                mfa_enrolled_at, clears mfa_required, and
//                re-issues the session cookie without
//                mfa_pending. Idempotent on retry.
//
//   - /verify  — step up an mfa_pending session. Body {totp}.
//                Same verify path as /confirm but does NOT
//                stamp mfa_enrolled_at (the customer is
//                already enrolled). Re-issues the cookie
//                without mfa_pending.
//
//   - /recover — burn a recovery code to regain access when
//                the TOTP device is lost. Body {code}.
//                Removes the matching hash from the stored
//                set; re-issues the cookie without mfa_pending.
//
//   - /disable — opt out. Body {password} OR {recovery_code}.
//                Re-authenticates the customer by proving the
//                password (the account_passwords hash still
//                applies) or by burning a recovery code.
//                Clears mfa_secret_encrypted +
//                mfa_recovery_codes_hash + mfa_enrolled_at;
//                leaves mfa_required untouched so the
//                chokepoints can re-arm on the next trigger.
//
// All five responses are 200 OK on success. Failure modes use
// the RFC 7807 problem shape: CodeMFAInvalidCode (401) for bad
// codes, CodeNotFound (404) when the account row is gone
// between verify and the database stamp, CodeConflict (409)
// when /enroll is called by an already-enrolled customer.

package api

// MFAEnrollRequest is empty — the customer brings only their
// session cookie. Kept as a struct (not omitted) so the JSON
// decoder accepts a bare {} body without a "no JSON object"
// parse error. (Go's encoding/json decodes a missing body into
// the zero value of the request type, but the dashboard
// always sends an empty object — matching this keeps both
// sides happy.)
type MFAEnrollRequest struct{}

// MFAEnrollResponse is the one-shot enrollment payload. The
// customer sees Secret + RecoveryCodes exactly once; the
// server-side record holds only the sealed secret + SHA-256
// hashes. Re-calling /enroll returns a fresh secret + codes
// (the previous set is overwritten).
type MFAEnrollResponse struct {
	OTPAuthURL    string   `json:"otpauth_url"`
	Secret        string   `json:"secret"`
	QRCodePNG     []byte   `json:"qr_code_png_base64"`
	RecoveryCodes []string `json:"recovery_codes"`
}

// MFAConfirmRequest is the body for /confirm. Totp is the
// 6-digit code from the customer's authenticator app.
type MFAConfirmRequest struct {
	Totp string `json:"totp"`
}

// MFAVerifyRequest is the body for /verify. Same shape as
// /confirm but a separate type so the OpenAPI doc lists them
// separately.
type MFAVerifyRequest struct {
	Totp string `json:"totp"`
}

// MFADisableRequest is the body for /disable. Exactly one of
// Password / RecoveryCode is required; the handler validates
// the constraint and returns CodeValidation if neither or
// both are set.
type MFADisableRequest struct {
	Password     string `json:"password,omitempty"`
	RecoveryCode string `json:"recovery_code,omitempty"`
}

// MFARecoverRequest is the body for /recover.
type MFARecoverRequest struct {
	Code string `json:"code"`
}

// MFAConfirmResponse / MFAVerifyResponse / MFADisableResponse
// / MFARecoverResponse are the success bodies. They are
// deliberately empty — the meaningful side effects (cookie
// re-issue, audit Emit, store stamp) are not on the JSON wire.
// The handler returns 200 OK + a zero-byte body so the
// dashboard's "XHR succeeded → refresh the prompt" path
// branches on status alone.
type (
	MFAConfirmResponse struct{}
	MFAVerifyResponse  struct{}
	MFADisableResponse struct{}
	MFARecoverResponse struct{}
)
