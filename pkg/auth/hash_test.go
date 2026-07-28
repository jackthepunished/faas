package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
)

// testHMACKey is the deterministic key the package-level HMAC secret
// is set to for the unit tests in this file. Picking a non-empty
// fixed key (instead of nil) means the test asserts the HMAC
// contract — the rainbow-table-resistance property — not just the
// SHA-256 lookup-stability property. The exact value doesn't matter
// (the tests below don't hard-code a specific hash), only that it's
// the same value for the lifetime of the package in the test binary.
var testHMACKey = []byte("test-hmac-key-do-not-use-in-prod")

// TestMain is the package-level init that wires the test secret.
// HashEmail reads the package-level hmacSecretV on every call, so
// the secret must be set before any HashEmail call. We do it via
// TestMain (not a package init()) so the test binary picks up the
// same value across all tests in the file.
//
// Note: if another test file in this package ALSO depends on
// HashEmail's output, it will pick up the testHMACKey secret too
// (the package-level state is shared). That's fine — the hash
// output is still deterministic for a given input + key, and the
// tests that need a different key can call SetHMACSecret with their
// own value.
func init() {
	SetHMACSecret(testHMACKey, nil)
}

// TestHashEmail_NormalisesCaseAndWhitespace asserts the
// normalisation contract — case + whitespace collapse BEFORE
// hashing, so the audit row join key is the same across form-posts
// that vary only in casing/whitespace.
func TestHashEmail_NormalisesCaseAndWhitespace(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{name: "lowercase", in: "alice@example.com", want: hmacHex("alice@example.com")},
		{name: "uppercase local", in: "Alice@Example.com", want: hmacHex("alice@example.com")},
		{name: "leading whitespace", in: "   alice@example.com", want: hmacHex("alice@example.com")},
		{name: "trailing whitespace", in: "alice@example.com\t", want: hmacHex("alice@example.com")},
		{name: "all whitespace collapsed", in: "  Alice@Example.com  \n", want: hmacHex("alice@example.com")},
		{name: "empty", in: "", want: hmacHex("")},
		{name: "whitespace only", in: "   \t\n", want: hmacHex("")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := HashEmail(tc.in)
			if got != tc.want {
				t.Errorf("HashEmail(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestHashEmail_DeterministicAcrossCalls asserts the same email
// hashes to the same value across calls so the audit row can join
// across subsystems (login failures from /login and /signup must
// share the same email_hash key).
func TestHashEmail_DeterministicAcrossCalls(t *testing.T) {
	a := HashEmail("victim@example.com")
	b := HashEmail("VICTIM@example.com")
	c := HashEmail(" victim@example.com ")
	if a != b || b != c {
		t.Errorf("HashEmail not deterministic: a=%q b=%q c=%q", a, b, c)
	}
}

// TestHashEmail_DoesNotReturnPlaintext is the SOC 2 evidence-chain
// belt-and-braces: confirm the audit row will not leak the literal
// email. This is the bare-minimum guarantee the GDPR-relevant
// audit storage contract relies on.
func TestHashEmail_DoesNotReturnPlaintext(t *testing.T) {
	in := "alice@example.com"
	got := HashEmail(in)
	if strings.Contains(got, "alice") || strings.Contains(got, "example") {
		t.Errorf("HashEmail leaked plaintext component: got=%q", got)
	}
}

// TestHashEmail_RainbowTableResistance is the load-bearing security
// property the HMAC switch introduces (issue #286 / CodeQL alert
// #121). Plain SHA-256 of an email is rainbow-table-reversible —
// `sha256("alice@example.com")` is a fixed value any adversary can
// precompute. With HMAC keyed by a per-daemon secret, the same
// input produces a different value on each box, so an off-box
// rainbow table built against a leaked `email_hash` column from
// box A does NOT translate to box B.
//
// The test asserts:
//  1. Two different secrets produce different hashes for the same
//     email (rainbow-table-portability broken).
//  2. The hash output is NOT sha256(lower(email)) — i.e. the
//     computation actually went through HMAC, not a regression to
//     the SHA-256 form.
//
// This test is the tripwire that catches a future regression to
// the SHA-256 form (which would re-open the CodeQL alert AND the
// rainbow-table attack).
func TestHashEmail_RainbowTableResistance(t *testing.T) {
	const email = "alice@example.com"
	const lower = "alice@example.com" // already lower-case; nothing to do

	// Save the package secret so we can restore it at end of test.
	originalKey := hmacSecret()
	t.Cleanup(func() { SetHMACSecret(originalKey, nil) })

	// Pin secret A.
	SetHMACSecret([]byte("secret-A"), nil)
	hashA := HashEmail(email)

	// Pin secret B.
	SetHMACSecret([]byte("secret-B"), nil)
	hashB := HashEmail(email)

	if hashA == hashB {
		t.Errorf("HMAC secret swap did not change the hash output; got hashA == hashB == %q. The implementation has regressed to plain SHA-256 (rainbow-table-reversible).", hashA)
	}

	// Confirm hashA != sha256(lower) — i.e. it's not just SHA-256
	// of the email.
	plainSum := sha256.Sum256([]byte(lower))
	plainHex := hex.EncodeToString(plainSum[:])
	if hashA == plainHex {
		t.Errorf("HashEmail returned plain SHA-256 output (%q); the HMAC switch has been undone. The CodeQL alert #121 would re-fire.", hashA)
	}
}

// TestHashEmail_EmptyKeyFallback confirms the package compiles and
// produces a stable output even when SetHMACSecret was never called
// (zero-key fallback). The output is NOT the same as
// sha256(lower(email)) — HMAC pads the empty key to the hash block
// size before use, so the "zero key" path produces an HMAC-SHA256
// output, not a plain SHA-256 digest. The output is still rainbow-
// table-portable across boxes that all run with the same zero-key
// fallback (== HMAC-SHA256(empty-key, msg)) — boxes running with
// a real per-box secret produce different output for the same
// email.
//
// The contract under test is "no panic, deterministic output,
// consistent across calls" — NOT "the output equals sha256(msg)".
// Production boxes MUST set a non-empty key via SetHMACSecret;
// this test pins the dev-mode fallback so a future nil-pointer
// panic in HashEmail doesn't slip in.
func TestHashEmail_EmptyKeyFallback(t *testing.T) {
	originalKey := hmacSecret()
	t.Cleanup(func() { SetHMACSecret(originalKey, nil) })

	SetHMACSecret(nil, nil) // explicit nil = fallback path

	const email = "alice@example.com"
	got1 := HashEmail(email)
	got2 := HashEmail(email)
	if got1 != got2 {
		t.Errorf("empty-key HashEmail is not deterministic: got1=%q got2=%q", got1, got2)
	}
	// And the empty-key output must NOT equal sha256(lower(email))
	// — if it did, the HMAC switch has been undone at the seam
	// (the very regression the tripwire in TestHashEmail_RainbowTableResistance
	// catches).
	plainSum := sha256.Sum256([]byte(email))
	plainHex := hex.EncodeToString(plainSum[:])
	if got1 == plainHex {
		t.Errorf("empty-key HashEmail == sha256(lower(email)) = %q; HMAC has been undone at the seam", plainHex)
	}
}

// TestGenerateHMACSecret_Returns32Bytes sanity-checks the
// secret-generation helper: the key is the right length for
// HMAC-SHA256 (32 bytes = 256 bits, matching the underlying hash
// output) and is non-zero (otherwise random.Read failed silently).
func TestGenerateHMACSecret_Returns32Bytes(t *testing.T) {
	key, err := GenerateHMACSecret()
	if err != nil {
		t.Fatalf("GenerateHMACSecret: %v", err)
	}
	if len(key) != 32 {
		t.Errorf("GenerateHMACSecret returned %d bytes; want 32", len(key))
	}
	allZero := true
	for _, b := range key {
		if b != 0 {
			allZero = false
			break
		}
	}
	if allZero {
		t.Error("GenerateHMACSecret returned all-zero key; crypto/rand failed silently")
	}
}

// TestSetHMACSecret_DefensiveCopy confirms SetHMACSecret copies the
// caller's key rather than aliasing the underlying slice. A future
// regression that dropped the copy would let the caller mutate the
// package-level secret via the slice they still hold — which would
// be a subtle, hard-to-test security bug.
func TestSetHMACSecret_DefensiveCopy(t *testing.T) {
	originalKey := hmacSecret()
	t.Cleanup(func() { SetHMACSecret(originalKey, nil) })

	callerKey := []byte("original-secret")
	SetHMACSecret(callerKey, nil)

	// Mutate the caller's slice. If SetHMACSecret took a reference
	// (not a copy), the package state would now be "mutated-secret"
	// and the test below would fail.
	callerKey[0] = 'M'

	// hmacSecret() returns a defensive copy too, so this read
	// shouldn't see the mutation either way. But the contract being
	// tested here is on the WRITE path.
	hashBefore := HashEmail("test@example.com")

	// Restore the package secret (via a fresh call) and confirm
	// the output is the same as the hash captured above, meaning
	// the prior mutation didn't leak through to the package state.
	SetHMACSecret([]byte("original-secret"), nil)
	hashAfter := HashEmail("test@example.com")

	if hashBefore != hashAfter {
		t.Errorf("caller's slice mutation leaked into package state:\n  hash-before: %q\n  hash-after:  %q", hashBefore, hashAfter)
	}
}

// hmacHex returns the HMAC-SHA256 hex digest of s under testHMACKey.
// Mirrors the old `sha256Hex` helper so the table-driven
// normalisation test reads the same way as before.
func hmacHex(s string) string {
	mac := hmac.New(sha256.New, testHMACKey)
	mac.Write([]byte(s))
	return hex.EncodeToString(mac.Sum(nil))
}
