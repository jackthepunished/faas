// Package authcode's HMAC secret infrastructure (IAM-hardening
// mega-PR logical change 7).
//
// Recovery codes are the IAM-2 fallback when the customer's TOTP
// device is lost. The recovery_code hash used to be bare SHA-256
// of the uppercased 10-char base32 plaintext — a 50-bit value
// (the customer's recovery code is 10 chars × 5 bits/char in
// the base32 alphabet A-Z + 2-7). On a leaked PG blob, an
// attacker can pre-compute every common SHA-256 hash and
// rainbow-reverse the column in O(1) per code.
//
// The fix is a standard HMAC-SHA256 keyed by a per-process
// secret. The secret lives at /var/lib/faas/recovery-hmac.key
// (auto-generated 0o600 on first boot) and is loaded once at
// apid startup. The audit-hmac.key precedent (ADR-035 §"Failed-
// login emission" / issue #386) uses a Warn-and-continue zero-key
// fallback; the recovery-hmac key MUST refuse to start without
// a real key. A leaked PG blob + a zero-key HMAC is the same
// threat as a leaked PG blob + bare SHA-256 (an attacker who
// has the blob can compute hashes offline). The boot-time
// refusal is the difference between "no defence" and "no
// service" — and the latter is the correct tradeoff for the
// recovery fall-back code path.
//
// Wire compatibility: the migration is a forced re-enrollment
// of every MFA-enrolled account. The boot path sets the key
// unconditionally; the verify path will rotate customer codes
// at next enrollment.
package authcode

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
)

// ErrNoHMACKey is returned by HashRecoveryCode when SetHMACSecret
// was never called or was called with a zero-length key. The
// audit-hmac.key precedent uses a Warn-and-continue policy; the
// recovery-hmac path is stricter — every call must succeed or
// the boot must fail. The strictness is intentional because the
// recovery code is the only fallback when the customer's TOTP
// device is lost: a zero-key HMAC is identical to bare SHA-256
// in threat surface.
var ErrNoHMACKey = errors.New("authcode: recovery HMAC secret not configured — refuse to start")

// hmacSecret is the per-process secret HashRecoveryCode keys
// its HMAC-SHA256 with. Set once at startup via SetHMACSecret;
// read on every hash call. The shape is a byte slice so future
// operators can rotate to a 64-byte key without an API change.
//
// Single-process: pkg/authcode is owned by cmd/apid's
// setRecoveryHMACSecret at startup. gatewayd-internal does not
// mint or verify recovery codes (no customer-facing MFA flow),
// so this package is only ever loaded into one daemon.
//
// The contract is "set once at boot, read everywhere". There is
// no RWMutex here because pkg/authcode is called on the request
// hot path — the read is bounded constant-time, the set is
// startup-bound. A sync.RWMutex would add per-hash cost on the
// hot path. The "set twice" case is a programming error caught
// by the boot-time test `TestSetHMACSecret_RefuseEmpty`.
var hmacSecret []byte

// SetHMACSecret stores the recovery-code HMAC secret for the
// lifetime of the process. Callers MUST zero the input slice
// after this call returns; the function copies the bytes into
// a fresh internal slice (the same lifetime contract that
// pkg/session.Manager.NewManager keeps for the AEAD key).
//
// Accepts a key of any length ≥ 1; the HMAC-SHA256 spec
// requires only a non-empty key. The conventional choice is
// 32 bytes (256 bits) — the same entropy as the AEAD key — so
// a leaked PG blob and a leaked recovery-hmac secret don't
// have a discrepancy in either direction.
//
// Returns an error when the key is empty so the boot-time
// caller can refuse to start. The refusal mirrors the spec
// §11 "no key, no service" rule for the session-AEAD key.
func SetHMACSecret(key []byte) error {
	if len(key) == 0 {
		return ErrNoHMACKey
	}
	dup := make([]byte, len(key))
	copy(dup, key)
	// Zero the caller's slice before returning. The internal
	// copy is the lifetime owner.
	for i := range key {
		key[i] = 0
	}
	hmacSecret = dup
	return nil
}

// HmacConstantTimeEqual compares two hashes in constant time.
// The recovery-code hash column is bytea; the new HMAC digest
// is 32 bytes; the legacy SHA-256 digest is also 32 bytes
// (same algorithm, different keying). The byte-length match
// means a single helper covers both code paths.
//
// Exported so verification helpers outside this package can
// use the testify-style constant-time compare — the existing
// SQL MatchRecoveryCode path uses pgx's memcmp, which is
// functionally identical. The helper here is the in-memory
// analog for the test seam.
func HmacConstantTimeEqual(a, b []byte) bool {
	return subtle.ConstantTimeCompare(a, b) == 1
}

// HMACSize is the digest length of HMAC-SHA256, copied from
// crypto/sha256.Size so callers can size their storage without
// importing crypto/sha256 themselves.
const HMACSize = sha256.Size

// HashRecoveryCode is the canonical HMAC-keyed digest form.
// The caller uppercases the presented code before passing it
// in (the recovery-code generator emits uppercase, and
// case-insensitive matching is the customer's expectation).
//
// Returns (digest, nil) on success; (nil, ErrNoHMACKey) when
// no secret is configured. The error path is the load-bearing
// guard: a caller that swallows the error re-introduces the
// leaf-bare-SHA-256 threat (the audit-hmac.key pattern).
//
// Pattern mirrors pkg/auth.HashEmail at pkg/auth/hash.go —
// key the same way (HMAC-SHA256), call the constructor the
// same way, and zero the input the same way. Mirroring keeps
// the CodeQL `go/weak-sensitive-data-hashing` precedent
// applicable to both call sites.
func HashRecoveryCode(code string) ([]byte, error) {
	if len(hmacSecret) == 0 {
		return nil, ErrNoHMACKey
	}
	mac := hmac.New(sha256.New, hmacSecret)
	// toUpper (the ASCII-locale-safe one) — recovery codes
	// are base32-no-padding (A-Z + 2-7), so an in-place ASCII
	// fold is correct. strings.ToUpper would reallocate and
	// add locale dependency for no gain.
	mac.Write([]byte(toUpper(code)))
	return mac.Sum(nil), nil
}

// VerifyRecoveryCode is the constant-time compare helper for
// the verify path. The caller provides the stored digest and
// the presented plaintext; the function derives the presented
// digest via HashRecoveryCode and compares.
//
// The constant-time compare is the difference between a
// timing-attack-vulnerable equality check and a hostile
// attacker who can probe one byte at a time on a hot path.
// pgx's memcmp in the SQL path (MatchRecoveryCode) covers
// the SQL verify; this helper covers the in-memory verify
// path the handler tests use.
func VerifyRecoveryCode(stored []byte, presented string) bool {
	derived, err := HashRecoveryCode(presented)
	if err != nil {
		return false
	}
	return subtle.ConstantTimeCompare(stored, derived) == 1
}

// toUpper is the package-local ASCII uppercase helper. Kept
// here (not strings.ToUpper) because the recovery character
// set is base32 (A-Z + 2-7), which is ASCII-only by
// construction; the locale-aware strings.ToUpper would
// reallocate on Turkish-locale `i` etc. and waste cycles on
// ASCII input.
func toUpper(s string) string {
	out := make([]byte, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'a' && c <= 'z' {
			c -= 'a' - 'A'
		}
		out[i] = c
	}
	return string(out)
}

// GenerateRandomKey is the boot-time helper that auto-mints
// a 32-byte HMAC secret when /var/lib/faas/recovery-hmac.key
// doesn't exist. The byte slice is freshly allocated; the
// caller (cmd/apid/main.go) writes it to disk with 0o600
// permissions and then calls SetHMACSecret.
func GenerateRandomKey() ([]byte, error) {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("authcode: random key: %w", err)
	}
	return key, nil
}
