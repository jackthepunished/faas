package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"strings"
	"sync"
)

// HashEmail returns a stable, lookup-safe HMAC-SHA256 hex digest of
// the supplied email, normalised to lower-case + trimmed whitespace
// before hashing. Used by the failed-login audit row (issue #286,
// SOC 2 CC7.2) so the audit table doesn't carry plaintext PII while
// remaining joinable across subsystems (the same email submitted
// from /login and /signup hashes to the same value).
//
// Why HMAC-SHA256 and not plain SHA-256:
//
//   - Plain SHA-256 of an email is reversible via rainbow tables for
//     any adversary who has built a corpus of `email -> sha256(email)`
//     for common addresses. The audit row is observable to the
//     customer via GET /v1/audit-events, and to operators via the
//     events table. A rainbow-table attack on a customer's own email
//     is trivial; an attack on a leaked operator-side dump is the
//     same problem. SHA-256 is safe for casual operators (they see
//     "looks like a hash, not a real email") but is NOT safe against
//     a determined forensic adversary.
//   - HMAC-SHA256 keyed by a server-side secret is collision- and
//     pre-image-resistant in the same way SHA-256 is, but the rainbow
//     table attack requires the secret to be present. A box-level
//     secret loaded from host.age (PR #237 precedent) at startup,
//     never written to the events table, never logged, gives us the
//     lookup-stability we need for the audit-row join key WITHOUT
//     the rainbow-table weakness.
//   - CodeQL go/weak-sensitive-data-hashing (alert #121) flagged the
//     SHA-256 form. The HMAC form is the right fix; dismissing the
//     alert was incorrect.
//
// Why a per-daemon secret (not a per-customer salt): the audit row
// is a *join key across handlers*, not a credential verification.
// A per-customer salt would (a) require looking up the customer's
// salt before emitting the audit row, defeating the async-batched
// flusher path (the audit emit must NOT do an extra DB lookup), and
// (b) make the audit row joinable only via the customer lookup
// table, not via a self-contained value. The per-daemon HMAC key
// preserves both the no-extra-DB-lookup and the self-contained-join
// contracts.
//
// Why normalise before hashing: case-only differences in the inbound
// email (`Alice@Example.com` vs `alice@example.com`) must collapse
// to the same audit row, otherwise the same physical login attempt
// would mint two distinct audit rows and the per-IP cardinality cap
// would inflate. Trim handles leading/trailing whitespace from form
// posts. Lower-case is the canonical form per RFC 5321 §2.3.11 (the
// local part is technically case-sensitive but every real provider
// normalises).
func HashEmail(email string) string {
	normalised := strings.ToLower(strings.TrimSpace(email))
	key := hmacSecret()
	// The zero-key fallback is documented as a misconfiguration
	// below; here we just compute HMAC over whatever key is wired
	// in. A nil/empty key produces HMAC(key=[], msg=normalised),
	// which still meets the "lookup-stable join key" contract — it
	// just doesn't add rainbow-table resistance. The SetHMACSecret
	// doc-comment explains the trade-off.
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(normalised))
	return hex.EncodeToString(mac.Sum(nil))
}

// hmacSecretMu guards hmacSecret. The package-level secret is set
// once at daemon startup (cmd/apid/main.go) via SetHMACSecret and
// read on every HashEmail call. Mirrors the SetMFARecipient pattern
// at cmd/apid/main.go (issue #186 / PR #318): the secret is bound
// late so the package can be imported in unit tests without
// requiring a secret to be available.
var (
	hmacSecretMu sync.RWMutex
	hmacSecretV  []byte
)

// SetHMACSecret wires the per-daemon HMAC key into the package.
// Called from cmd/apid/main.go at startup, AFTER the host.age
// identity has been loaded and a domain-separated audit-HMAC key
// has been derived (HKDF over the identity, info="audit-email-hmac"
// to keep this domain distinct from any other HMAC use).
//
// A nil or empty key logs a Warn and leaves HashEmail running on a
// zero-key fallback. The zero-key fallback is functionally
// equivalent to the old SHA-256 form: stable join key, but rainbow-
// table-reversible. Production boxes MUST set a non-empty key.
// Tests that don't care about rainbow-table resistance can pass
// nil to skip the SetHMACSecret dance (or call SetHMACSecret with
// a deterministic test key).
//
// SetHMACSecret is safe to call concurrently with HashEmail
// (RWMutex); the new key takes effect for all subsequent calls.
func SetHMACSecret(key []byte, log *slog.Logger) {
	hmacSecretMu.Lock()
	defer hmacSecretMu.Unlock()
	if len(key) == 0 {
		if log != nil {
			log.Warn("auth: SetHMACSecret called with empty key; HashEmail will run on a zero-key fallback (rainbow-table-reversible). Production must wire a host.age-derived key.")
		}
		hmacSecretV = nil
		return
	}
	// Defensive copy: the caller may reuse the underlying slice for
	// other HMAC purposes (the host.age identity is one buffer; we
	// don't want our long-lived global to alias it).
	cp := make([]byte, len(key))
	copy(cp, key)
	hmacSecretV = cp
}

// hmacSecret returns the current secret under read-lock. Returning
// a copy (not the underlying slice) keeps the caller from mutating
// the package state via the byte slice returned to the crypto/hmac
// internals — hmac.New copies the key on construction so this is
// belt-and-braces, but the read-side copy is cheap and closes the
// door.
func hmacSecret() []byte {
	hmacSecretMu.RLock()
	defer hmacSecretMu.RUnlock()
	if len(hmacSecretV) == 0 {
		return nil
	}
	cp := make([]byte, len(hmacSecretV))
	copy(cp, hmacSecretV)
	return cp
}

// GenerateHMACSecret returns a cryptographically random 32-byte key
// suitable for SetHMACSecret. Used by:
//   - The dev-mode fallback in cmd/apid/main.go when host.age is
//     not loaded (writes the key to /var/lib/faas/audit-hmac.key
//     with 0o600 perms so it survives daemon restart).
//   - Tests that need a fresh key per case.
func GenerateHMACSecret() ([]byte, error) {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	return key, nil
}
