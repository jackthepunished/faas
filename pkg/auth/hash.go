package auth

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// HashEmail returns a breach-resistant SHA-256 hex digest of the
// supplied email, normalised to lower-case + trimmed whitespace
// before hashing. Used by the failed-login audit row (issue #286)
// so the audit table doesn't carry plaintext PII while remaining
// joinable across subsystems (the same email submitted from /login
// and /signup hashes to the same value).
//
// Threat model:
//   - The audit row is observable to the customer via
//     GET /v1/audit-events and to operators via the events table.
//     Plaintext email would be PII under GDPR + the SOC 2 evidence
//     chain — this helper is the seam that removes it.
//   - The hash is **breach-resistant, not strongly anonymous.** SHA-256
//     of a known email is reversible via rainbow tables for any
//     adversary who has built a corpus of `email -> sha256(email)` for
//     common addresses. The hash is safe for casual operators (they
//     see "looks like a hash, not a real email") and safe for the
//     audit-row storage contract. It is NOT safe against a determined
//     forensic adversary who can rainbow-table the value back to a
//     plain address.
//   - A follow-up ADR can swap this for HMAC-SHA256 with a server-side
//     secret loaded from host.age (PR #237 precedent) if the threat
//     model gets stronger. The function signature is stable so the
//     swap is local.
//
// Why normalise before hashing: case-only differences in the inbound
// email (`Alice@Example.com` vs `alice@example.com`) must collapse to
// the same audit row, otherwise the same physical login attempt would
// mint two distinct audit rows and the per-IP cardinality cap would
// inflate. Trim handles leading/trailing whitespace from form posts.
// Lower-case is the canonical form per RFC 5321 §2.3.11 (the local
// part is technically case-sensitive but every real provider
// normalises).
//
// Why SHA-256 and not argon2id: this is a *lookup* hash, not a
// credential hash. The aim is one-to-one mapping with a stable
// output length, not brute-force resistance — the inbound email
// already failed to authenticate, so anti-brute-force is off-topic.
// argon2id here would buy nothing and would inflate the per-failed-
// login CPU cost by 50 ms.
func HashEmail(email string) string {
	normalised := strings.ToLower(strings.TrimSpace(email))
	sum := sha256.Sum256([]byte(normalised))
	return hex.EncodeToString(sum[:])
}
