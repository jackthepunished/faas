// Server-side session wire shapes (IAM-3, ADR-036, issue #187 +
// #244 merged). The four endpoints under /v1/auth/* drive the
// dashboard's "Active sessions" panel:
//
//   - POST   /v1/auth/logout                  revoke the current
//                                              sid (this device).
//   - GET    /v1/auth/sessions                list every active
//                                              row for the calling
//                                              account; the row
//                                              whose id matches
//                                              the cookie's sid
//                                              is flagged
//                                              current_session:
//                                              true so the
//                                              dashboard can
//                                              render the
//                                              "this device" pill.
//   - DELETE /v1/auth/sessions/{id}            revoke a sibling
//                                              by id. Cross-
//                                              account deletes
//                                              return 404 (we
//                                              never confirm a
//                                              row exists in
//                                              another account).
//                                              Revoking the
//                                              current sid is
//                                              allowed and clears
//                                              the calling
//                                              cookie too (the
//                                              "log out this
//                                              device" path via
//                                              the list).
//   - POST   /v1/auth/sessions/revoke_all     revoke every
//                                              active row
//                                              except the
//                                              calling sid.
//                                              Returns
//                                              {revoked: N}.
//
// All four are CSRF-gated (CSRFToken absorbed in bodies; logout
// and revoke* actions use VerifyAuthenticated). Failure modes
// reuse the existing RFC 7807 codes:
// CodeCSRFInvalid (400, CSRF mismatch),
// CodeSessionExpired (401, mid-flight revoke of current),
// CodeNotFound (404, cross-account or unknown sid),
// CodeValidation (400, bad uuid on path).
package api

// SessionInfo is one row in GET /v1/auth/sessions. IssuedIP is
// the empty string when RemoteAddr was unparseable at login
// (mirrors pkg/state.Session.IssuedIP's "" sentinel — the SQL
// side stores NULL + coalesces to ""; clients don't need to
// distinguish). IssuedUA may also be empty when the browser
// never sent User-Agent. CurrentSession is true only on the
// row whose id matches the cookie's sid.
type SessionInfo struct {
	ID             string `json:"id"`
	IssuedIP       string `json:"issued_ip"`
	IssuedUA       string `json:"issued_ua"`
	IssuedAt       string `json:"issued_at"`         // RFC3339
	LastSeenAt     string `json:"last_seen_at"`      // RFC3339, "" if never
	CurrentSession bool   `json:"current_session"`
}

// SessionListResponse is the body of GET /v1/auth/sessions.
// Sessions are ordered newest-first (issued_at desc). Empty
// when the account has never logged in via the dashboard; CLI
// bearer-key logins do not create sessions.
type SessionListResponse struct {
	Sessions []SessionInfo `json:"sessions"`
}

// SessionsRevokeAllResponse is the body of POST
// /v1/auth/sessions/revoke_all. The number is the count of
// rows the server actually flipped (zero is a valid response
// if the caller was the only active session).
type SessionsRevokeAllResponse struct {
	Revoked int `json:"revoked"`
}

// SessionsRevokeRequest is the body for DELETE
// /v1/auth/sessions/{id}. The id arrives via the URL path; the
// body is empty for the simple shape. RevokeRequest keeps the
// CSRFToken seam in case the dashboard switches to a body-only
// POST transport later; today CSRF verification reads from the
// cookie + the bound action token, not from this field, but
// having it on the type keeps decodeJSON's DisallowUnknownFields
// happy and matches the MFA handlers' convention.
type SessionsRevokeRequest struct {
	CSRFToken string `json:"csrf_token,omitempty"`
}
