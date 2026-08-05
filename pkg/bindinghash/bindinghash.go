// Package bindinghash computes the session-cookie binding fingerprint
// (IAM-3 row-cross-check + stolen-cookie auto-revoke, ADR-076).
//
// The binding hash is HMAC-SHA256 keyed by the host's session-key
// secret (the same secret the AEAD envelope uses; first 32 bytes).
// The hash binds the session to a (client IP, user-agent family)
// pair so a stolen `faas_sid` cookie cannot be replayed from a
// different IP / UA combination without the middleware detecting
// the drift.
//
// Design notes:
//
//   - HMAC-SHA256, not bare SHA-256. The CodeQL `go/weak-sensitive-data-hashing`
//     rule (CWE-200) flagged the SHA-256 form during the audit-log
//     HMAC fix (PR #386 / issue #188). Bare SHA-256 of `ip + ua` is
//     rainbow-reversible for the small input space (IPs are 32-bit
//     at most; UA families are 8 buckets). HMAC-SHA256 keyed by a
//     per-host secret is not rainbow-reversible offline even if
//     the DB is leaked.
//   - Empty inputs return "". The unix-socket code path
//     (gatewayd-internal → apid) has no meaningful client IP; the
//     CLI auth-code path may omit a User-Agent. The middleware
//     treats "" as "binding not armed" and skips the cross-check
//     (the cookie envelope's `binding_hash` field is `omitempty`,
//     so a pre-PR-076 cookie decodes cleanly with the empty
//     string).
//   - The output is hex-encoded, 32 chars (128-bit effective
//     security). The HMAC-SHA256 of small inputs is far from
//     2^128 collision space, but the binding is per-IP+UA-family
//     so collisions across unrelated sessions are not a concern
//     (the binding is a fingerprint, not a primary key).
//
// Coordinate with pkg/authcode (HMAC secret policy) and
// pkg/auth.HashEmail (HMAC-SHA256 pattern) — the secret-loading
// precedence is identical (env var → on-disk → ephemeral fallback)
// but bindinghash does NOT refuse to start without a key; the
// session manager refuses to start, and that suffices.
package bindinghash

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// KeyFunc returns the HMAC key the Compute function should use.
// Returns nil to disable binding-hash (the envelope's
// `binding_hash` field is omitted; the middleware skips the
// cross-check). Production wires this to the first 32 bytes of
// the session-key AEAD secret so the binding hash is bound to the
// same host secret as the cookie itself.
type KeyFunc func() []byte

// Compute returns the 32-char hex HMAC-SHA256 fingerprint of
// (ip + "\x00" + uaFamily). The separator is a NUL byte so a
// pathological input like ("1.2.3.4", "1.2.3.4") cannot collide
// with ("1.2.3.41", "2.3.4") — the IP and UA family are
// genuinely separate fields.
//
// Returns "" when either input is empty (the "binding not armed"
// contract documented in the package comment) or when the KeyFunc
// returns nil (the dev-mode no-secret path).
func Compute(ip, uaFamily string, keyFn KeyFunc) string {
	if ip == "" || uaFamily == "" {
		return ""
	}
	if keyFn == nil {
		return ""
	}
	key := keyFn()
	if key == nil {
		return ""
	}
	h := hmac.New(sha256.New, key)
	h.Write([]byte(ip))
	h.Write([]byte{0})
	h.Write([]byte(uaFamily))
	return hex.EncodeToString(h.Sum(nil))
}

// UAFamily reduces a User-Agent header to one of eight family
// buckets. The classifier is a substring match — the binding
// hash is per-IP+UA-family, so collisions across unrelated
// browsers do not matter; the family is a coarse fingerprint
// intended to detect "browser changed entirely" rather than
// "browser version drifted". A future PR that wants browser-
// version precision can extend the bucket list or switch to a
// UA-parser library; the in-tree subset suffices for the
// stolen-cookie threat model.
//
// Returns "unknown" for empty / unparseable UAs. The middleware
// treats unknown as a single bucket so the v1 threat model
// ("attacker changes the browser family") is still detectable.
func UAFamily(ua string) string {
	if ua == "" {
		return "unknown"
	}
	low := strings.ToLower(ua)
	switch {
	case strings.Contains(low, "edg/"):
		return "edge"
	case strings.Contains(low, "chrome/") || strings.Contains(low, "chromium/"):
		return "chrome"
	case strings.Contains(low, "firefox/"):
		return "firefox"
	case strings.Contains(low, "safari/") && !strings.Contains(low, "chrome/"):
		return "safari"
	case strings.Contains(low, "curl/"):
		return "curl"
	case strings.Contains(low, "wget/"):
		return "wget"
	case strings.HasPrefix(low, "faas-cli/") || strings.Contains(low, "faas-cli/"):
		return "cli"
	default:
		return "unknown"
	}
}
