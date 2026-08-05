// Package session holds the dashboard session cookie envelope (M7.5,
// spec §11, gap G2 closure).
//
// Sessions are AES-GCM sealed JSON blobs: the cookie carries the
// nonce+ciphertext together. The 32-byte host secret lives at
// /etc/faas/secrets/session.key (root:root 0400, spec §11) and is
// loaded by apid on boot. Sessions hold an account_id + expiry;
// revocation is a server-side token lookup (IssuedToken is one-shot;
// the cookie just maps to the account_id).
//
// Failure mode: a missing or wrong-length key in production = boot
// failure. Dev (FAAS_DEV_TOKEN path) generates an ephemeral key with
// a warning, so the daemon still comes up for local testing.
package session

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"
)

// Manager owns the sealed-cookie lifecycle. Safe for concurrent use.
//
// Note: the 32-byte host secret is consumed in NewManager and is
// zeroed in the caller's slice — it never lives on the Manager's
// heap in the caller's slice. The AEAD wraps a copy inside the
// standard library's cipher package; Manager.bindingKey holds an
// INDEPENDENT copy used by BindingKey() to power the ADR-076
// binding-hash HMAC. Both copies are zeroed on Manager.Close.
type Manager struct {
	gcm        cipher.AEAD
	bindingKey []byte // 32-byte AEAD key copy for BindingKey()
	maxAge     time.Duration
	now        func() time.Time
	issuedAt   time.Time
}

// Envelope is the JSON payload sealed inside the cookie. Adding a
// field here is a non-breaking change (older cookies decode fine;
// a missing field unmarshals to the zero value); removing or
// renaming is breaking (older cookies fail Verify).
//
// MfaPending is the IAM-2 / issue #186 flag: when true, every
// session-cookie route 403s CodeMFARequired except the dashboard
// whoami + the /v1/account/mfa/* allowlist. The flag is false on
// every cookie issued by /login paths that the customer has
// already cleared MFA on; the /v1/account/mfa/verify handler
// re-issues the cookie with MfaPending=false after a successful
// TOTP check, so subsequent reads of the same cookie no longer
// trip the gate.
//
// IAM-3 (ADR-039): Sid carries the uuid of the corresponding
// sessions row. Every authenticated dashboard request re-validates
// the row via state.Store.GetSession(sid); the apid middleware
// treats an empty Sid as "rollout pre-IAM-3" and clears the
// cookie. omitempty + a pre-IAM-3 envelope means an existing cookie
// unmarshals cleanly with Sid == "" (no wire-incompat surprise).
type Envelope struct {
	AccountID  string    `json:"account_id"`
	IssuedAt   time.Time `json:"issued_at"`
	ExpiresAt  time.Time `json:"expires_at"`
	MfaPending bool      `json:"mfa_pending,omitempty"`
	// GithubLogin is the GitHub login the session was last stamped
	// with (PR-B §11 ownership proof). Empty when the session was
	// issued before PR-B or the user never went through
	// /v1/auth/github. omitempty so pre-PR-B cookies decode to
	// "" without a key-mismatch error.
	GithubLogin string `json:"github_login,omitempty"`
	// IAM-3 (ADR-039): Sid carries the uuid of the corresponding
	// sessions row. Every authenticated dashboard request re-validates
	// the row via state.Store.GetSession(sid); the apid middleware
	// treats an empty Sid as "rollout pre-IAM-3" and clears the
	// cookie. omitempty + a pre-IAM-3 envelope means an existing cookie
	// unmarshals cleanly with Sid == "" (no wire-incompat surprise).
	Sid string `json:"sid,omitempty"` // IAM-3
	// BindingHash is the IAM-hardening-mega-PR (logical change 5,
	// ADR-076) fingerprint of (client IP, UA-family). HMAC-SHA256
	// keyed by the host's session-key secret; empty when the
	// unix-socket / CLI-auth code path has no meaningful fingerprint
	// (cross-check is skipped). The RequireSession cookie branch
	// compares this against the sessions row's binding_hash column;
	// mismatch ⇒ auto-revoke + audit + 401.
	BindingHash string `json:"binding_hash,omitempty"`
	// StepUpAt is the IAM-hardening-mega-PR (logical change 6,
	// ADR-077) timestamp the customer last cleared a fresh TOTP
	// for step-up MFA. Zero value = "never stepped up"; the
	// RequireStepUp middleware reads this and rejects when
	// time.Since(env.StepUpAt) > ttl. The cookie omits this
	// field when zero (omitempty), preserving pre-PR-077 wire
	// compatibility.
	StepUpAt time.Time `json:"step_up_at,omitempty"`
}

// BindingKey returns a copy of the 32-byte AEAD key for offline
// HMAC use (ADR-076). The Manager retains its own internal copy
// in the AES cipher block (standard library contract); this method
// is a separate read-only export that powers pkg/bindinghash.Compute.
// The returned slice is freshly allocated; callers MAY zero it
// after use. nil when the Manager is uninitialised.
//
// Construction: the key is captured at NewManager time and held
// in a private field. The Manager's zero-out sweep at
// construction-time zeroes the CALLER's slice, not the internal
// copy — the copy is what powers BindingKey after the constructor
// returns. The cost is one 32-byte allocation per Manager lifetime.
//
// Use case: apid's main.go takes the loaded session key, hands it
// to session.NewManager, then asks for a BindingKey copy that the
// binding-hash HMAC function uses. The two are intentionally
// independent so a future operator-tooling code path that wants the
// AEAD key for offline-cookie-mint can reach the SAME key bytes
// without re-loading the file.
func (m *Manager) BindingKey() []byte {
	if m == nil || m.bindingKey == nil {
		return nil
	}
	out := make([]byte, 32)
	copy(out, m.bindingKey)
	return out
}

// NewManager builds a Manager from a 32-byte key + a session lifetime.
// An empty key in production is the caller's responsibility (boot
// fails). Callers should generate the key once at install time:
//
//	openssl rand -hex 32 > /etc/faas/secrets/session.key
//
// On success the caller's key slice is zeroed — the Manager keeps
// only the AEAD, which holds an internal copy inside the standard
// library cipher package. This is the minimum-viable defense
// against the secret landing in a heap dump or core file via the
// caller's own slice.
func NewManager(key []byte, maxAge time.Duration) (*Manager, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("session: key must be 32 bytes (got %d)", len(key))
	}
	if maxAge <= 0 {
		maxAge = 7 * 24 * time.Hour
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		// Wipe before returning — we never reached the AEAD wrap
		// step so the cipher key is still in the caller's slice.
		for i := range key {
			key[i] = 0
		}
		return nil, fmt.Errorf("session: aes: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		for i := range key {
			key[i] = 0
		}
		return nil, fmt.Errorf("session: gcm: %w", err)
	}
	// Capture the binding-key copy BEFORE we wipe the caller's
	// slice. ADR-076's pkg/bindinghash.Compute needs an independent
	// 32-byte key it can HMAC with; the AEAD holds its own copy
	// inside cipher/gcm that we never reach. Re-derive by
	// keeping a parallel local copy that the caller cannot mutate
	// after we wipe theirs.
	bindingCopy := make([]byte, 32)
	copy(bindingCopy, key)
	// AEAD ready — the caller's slice is no longer needed.
	for i := range key {
		key[i] = 0
	}
	return &Manager{
		gcm:        gcm,
		bindingKey: bindingCopy,
		maxAge:     maxAge,
		now:        time.Now,
		issuedAt:   time.Now(),
	}, nil
}

// NewEphemeralManager builds a Manager with a fresh random key. Used
// for dev (FAAS_DEV_TOKEN path) so the daemon still boots without
// /etc/faas/secrets/session.key. NOT for production — every restart
// invalidates every cookie. The caller logs a warning.
func NewEphemeralManager(maxAge time.Duration) (*Manager, error) {
	key := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, key); err != nil {
		return nil, fmt.Errorf("session: random key: %w", err)
	}
	return NewManager(key, maxAge)
}

// MaxAge returns the configured session lifetime.
func (m *Manager) MaxAge() time.Duration { return m.maxAge }

// SetClock overrides the wall clock for IssuedAt / Verify. Tests use
// this to fast-forward time without sleeping. Production leaves it
// alone.
func (m *Manager) SetClock(now func() time.Time) {
	if now == nil {
		now = time.Now
	}
	m.now = now
}

// Issue seals a cookie envelope for accountID. The opaque cookie
// value is base64(nonce || ciphertext); apid stores the same value
// in Set-Cookie verbatim. Equivalent to IssueWithMFAFlag(accountID,
// false) — use the explicit variant when the caller knows the
// account is mfa_required && !MfaEnrolled.
func (m *Manager) Issue(accountID string) (string, error) {
	return m.IssueWithMFAFlag(accountID, false)
}

// IssueWithMFAFlag is the explicit-mfa-pending seam. Callers that
// already know whether to set the flag use this; the legacy
// Issue(accountID) is a thin wrapper that calls
// IssueWithMFAFlag(accountID, false). The mfa_pending JSON tag is
// `omitempty`, so a false value produces a cookie byte-for-byte
// indistinguishable from a pre-IAM-2 cookie for every existing
// browser. A true value is the only new bit on the wire.
//
// IAM-3 (ADR-039): this delegates to IssueWithSession with an empty
// sid. Pre-IAM-3 callers continue to work — the AEAD-bound cookie
// just lacks a sid, which the apid middleware treats as revoked.
func (m *Manager) IssueWithMFAFlag(accountID string, mfaPending bool) (string, error) {
	return m.IssueWithSession("", accountID, mfaPending)
}

// IssueWithSession seals an envelope with sid + accountID +
// mfaPending. The sid is the primary key of the sessions row created
// by state.Store.CreateSession; the apid middleware cross-validates
// it on every request. An empty sid is permitted (pre-IAM-3 login
// paths that don't mint a row) — the middleware interprets empty
// as "rollout before IAM-3" and revokes the cookie.
//
// MfaVerify reuses the existing sid via this entry point — flipping
// the mfaPending bit on the same row, no second CreateSession call.
func (m *Manager) IssueWithSession(sid, accountID string, mfaPending bool) (string, error) {
	now := m.now()
	return m.Seal(Envelope{
		AccountID:  accountID,
		IssuedAt:   now,
		ExpiresAt:  now.Add(m.maxAge),
		MfaPending: mfaPending,
		Sid:        sid,
	})
}

// IssueWithSessionAndGithubLogin seals an envelope carrying sid (IAM-3
// / ADR-039), accountID, mfaPending (IAM-2), and githubLogin
// (PR-B §11 ownership proof) in a single AEAD round. The single-seal
// shape is the contract /v1/auth/github relies on — the dashboard
// chrome reads the github_login to render "signed in as <login>",
// and the /oauth/callback handler reads the same field to satisfy
// the §11 ownership invariant.
//
// MfaPending + Sid + GithubLogin all carry `omitempty`, so a missing
// flag/field produces a cookie byte-for-byte indistinguishable from
// the pre-merge wire form. Cookie-shape compatibility is unchanged.
//
// Empty githubLogin is permitted (the user landed here from a non-
// GitHub path) — the JSON tag is `omitempty` so the field is absent
// on the wire, identical to the pre-PR-B cookie.
func (m *Manager) IssueWithSessionAndGithubLogin(sid, accountID, githubLogin string, mfaPending bool) (string, error) {
	now := m.now()
	return m.Seal(Envelope{
		AccountID:   accountID,
		IssuedAt:    now,
		ExpiresAt:   now.Add(m.maxAge),
		MfaPending:  mfaPending,
		Sid:         sid,
		GithubLogin: githubLogin,
	})
}

// IssueWithSessionAndGithubLoginAndBindingHash seals an envelope
// carrying sid (IAM-3 / ADR-039), accountID, mfaPending (IAM-2),
// githubLogin (PR-B §11 ownership proof), and the binding-hash
// fingerprint (ADR-076) in a single AEAD round. The github-login
// path is the only code path that needs three session-binding
// fields stamped together (sid + binding_hash + github_login);
// combining them into one Seal avoids a double-seal round when
// both fields need to land in the cookie.
//
// Empty githubLogin + empty bindingHash are permitted (cross-check
// skips, omitempty keeps the wire form pre-PR-076-compatible).
func (m *Manager) IssueWithSessionAndGithubLoginAndBindingHash(sid, accountID, githubLogin, bindingHash string, mfaPending bool) (string, error) {
	now := m.now()
	return m.Seal(Envelope{
		AccountID:   accountID,
		IssuedAt:    now,
		ExpiresAt:   now.Add(m.maxAge),
		MfaPending:  mfaPending,
		Sid:         sid,
		GithubLogin: githubLogin,
		BindingHash: bindingHash,
	})
}

// IssueWithSessionAndBindingHashAndStepUp seals an envelope with
// sid + accountID + mfaPending + binding_hash + step_up_at in a
// single AEAD round. The step-up path uses this helper on every
// successful /v1/account/mfa/verify so the next step-up-gated
// request reads a fresh timestamp off the cookie envelope. The
// binding hash is the ADR-076 fingerprint; the step-up time is
// the ADR-077 timestamp. Both omitempty so a pre-PR-076 cookie
// (no binding) decodes unchanged AND a pre-PR-077 cookie (no
// step_up_at) decodes with StepUpAt zero.
func (m *Manager) IssueWithSessionAndBindingHashAndStepUp(sid, accountID, bindingHash string, stepUpAt time.Time, mfaPending bool) (string, error) {
	now := m.now()
	return m.Seal(Envelope{
		AccountID:   accountID,
		IssuedAt:    now,
		ExpiresAt:   now.Add(m.maxAge),
		MfaPending:  mfaPending,
		Sid:         sid,
		BindingHash: bindingHash,
		StepUpAt:    stepUpAt,
	})
}

// IssueWithSessionAndBindingHash seals an envelope carrying sid (IAM-3),
// accountID, mfaPending (IAM-2), and the binding-hash fingerprint
// (ADR-076) in a single AEAD round. The bindingHash is the
// HMAC-SHA256 fingerprint of (client IP, UA-family) keyed by the
// host's session-key secret; "" is the documented "binding not armed"
// marker (the unix-socket / CLI-auth code path). The RequireSession
// cookie branch compares the envelope's binding_hash against the
// sessions row's binding_hash column; mismatch ⇒ auto-revoke +
// audit + 401.
//
// All four session-binding fields carry `omitempty` so a pre-PR-076
// cookie decodes unchanged with BindingHash == "" (the cross-check
// skips). Wire compatibility is preserved.
func (m *Manager) IssueWithSessionAndBindingHash(sid, accountID, bindingHash string, mfaPending bool) (string, error) {
	now := m.now()
	return m.Seal(Envelope{
		AccountID:   accountID,
		IssuedAt:    now,
		ExpiresAt:   now.Add(m.maxAge),
		MfaPending:  mfaPending,
		Sid:         sid,
		BindingHash: bindingHash,
	})
}

// Seal is the primitive the issue helpers thin-wrap: marshal +
// AEAD-seal an envelope. Exported so tests (and any future
// operations tooling) can mint a cookie with a hand-crafted
// envelope — specifically, a test of the IAM-3
// (ADR-039) defensive branch where the AEAD-bound AccountID
// disagrees with the sessions row.
func (m *Manager) Seal(env Envelope) (string, error) {
	plaintext, err := json.Marshal(env)
	if err != nil {
		return "", fmt.Errorf("session: marshal envelope: %w", err)
	}
	nonce := make([]byte, m.gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", fmt.Errorf("session: random nonce: %w", err)
	}
	sealed := m.gcm.Seal(nonce, nonce, plaintext, nil)
	return base64.RawURLEncoding.EncodeToString(sealed), nil
}

// IsMFAPending is the predicate requireMFA calls. It's a free
// function (not a method) so the requireMFA middleware can read
// the verified Envelope without reaching into the Manager.
func IsMFAPending(env Envelope) bool { return env.MfaPending }

// SealGithubLogin mints a new cookie carrying the same (accountID,
// issued_at, expires_at) triple as a fresh Issue, but with the
// GithubLogin field populated. Used by /v1/auth/github after a
// successful login so the §11 ownership check on /oauth/callback
// succeeds for the same user that just authenticated.
//
// mfaPending is the IAM-2 flag — IssueWithMFAFlag's argument, kept
// here so the GitHub handler can seal a cookie that carries both
// the §11 login AND the mfa_pending bit in a single crypto round
// (no double-seal). The mfa_pending JSON tag is `omitempty`, so
// false produces a cookie byte-for-byte indistinguishable from a
// pre-IAM-2 envelope for every existing browser.
//
// We mint a NEW cookie rather than mutating the existing one
// because the AEAD's nonce must be unique per seal; reuse would
// also work (the user can hold two cookies simultaneously) but
// means the dashboard has to clear the old one. apid's handler
// does the Set-Cookie dance, so this method is the seal-only
// primitive the handler calls.
func (m *Manager) SealGithubLogin(accountID, githubLogin string, mfaPending bool) (string, error) {
	if accountID == "" {
		return "", fmt.Errorf("session: accountID required")
	}
	now := m.now()
	env := Envelope{
		AccountID:   accountID,
		IssuedAt:    now,
		ExpiresAt:   now.Add(m.maxAge),
		MfaPending:  mfaPending,
		GithubLogin: githubLogin,
	}
	plaintext, err := json.Marshal(env)
	if err != nil {
		return "", fmt.Errorf("session: marshal envelope: %w", err)
	}
	nonce := make([]byte, m.gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", fmt.Errorf("session: random nonce: %w", err)
	}
	sealed := m.gcm.Seal(nonce, nonce, plaintext, nil)
	return base64.RawURLEncoding.EncodeToString(sealed), nil
}

// ErrInvalid is returned by Verify when the cookie is malformed,
// forged, or expired. Callers should clear the cookie on this error.
var ErrInvalid = errors.New("session: invalid or expired")

// Verify opens a cookie envelope. Returns the Envelope on success,
// ErrInvalid otherwise. Never returns a partial Envelope — the
// AEAD guarantees all-or-nothing.
func (m *Manager) Verify(value string) (Envelope, error) {
	raw, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return Envelope{}, ErrInvalid
	}
	ns := m.gcm.NonceSize()
	if len(raw) < ns {
		return Envelope{}, ErrInvalid
	}
	nonce, sealed := raw[:ns], raw[ns:]
	plaintext, err := m.gcm.Open(nil, nonce, sealed, nil)
	if err != nil {
		return Envelope{}, ErrInvalid
	}
	var env Envelope
	if err := json.Unmarshal(plaintext, &env); err != nil {
		return Envelope{}, ErrInvalid
	}
	if env.ExpiresAt.Before(m.now()) {
		return Envelope{}, ErrInvalid
	}
	if env.AccountID == "" {
		return Envelope{}, ErrInvalid
	}
	return env, nil
}

// csrfDomainSep is the AAD byte the CSRF seal/open path uses. It
// separates the CSRF nonce space from the session-cookie nonce space:
// even if the same nonce bytes ever appeared on both sides, the AEAD
// tag would not verify because the AAD differs. Defence in depth on
// top of crypto/rand's nonce uniqueness.
var csrfDomainSep = []byte("faas-csrf-v1")

// SealForCSRF seals arbitrary plaintext into a base64url nonce||ct
// blob. Unlike Issue/Verify (which carry the structured Envelope
// JSON), this is a raw seal intended for the CSRF helper that needs
// to seal its own JSON envelope with action + subject + expires. The
// caller is responsible for choosing the plaintext shape and TTL;
// the Manager only owns the cryptography.
//
// Output: base64url(nonce || ciphertext). AEAD tag authenticates
// against the csrfDomainSep AAD, so a CSRF blob cannot be replayed
// as a session cookie even if a nonce were reused.
func (m *Manager) SealForCSRF(plaintext []byte) (string, error) {
	nonce := make([]byte, m.gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", fmt.Errorf("session: csrf random nonce: %w", err)
	}
	sealed := m.gcm.Seal(nonce, nonce, plaintext, csrfDomainSep)
	return base64.RawURLEncoding.EncodeToString(sealed), nil
}

// OpenForCSRF is the inverse of SealForCSRF. Returns ErrInvalid on
// any tag / AAD / shape mismatch so callers can wrap into their own
// sentinel.
func (m *Manager) OpenForCSRF(raw []byte) ([]byte, error) {
	ns := m.gcm.NonceSize()
	if len(raw) < ns {
		return nil, ErrInvalid
	}
	nonce, sealed := raw[:ns], raw[ns:]
	plaintext, err := m.gcm.Open(nil, nonce, sealed, csrfDomainSep)
	if err != nil {
		return nil, ErrInvalid
	}
	return plaintext, nil
}
