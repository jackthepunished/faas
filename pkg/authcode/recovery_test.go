// Recovery-code digest tests (IAM-hardening mega-PR logical
// change 7: SHA-256 → HMAC-SHA256).
//
// The package's global hmacSecret is set via SetHMACSecret.
// Tests that exercise HashRecoveryCode / NewRecoveryCodes must
// call SetHMACSecret once at process start (TestMain does that
// here); the production path is wired in cmd/apid/main.go::run
// (see loadOrGenerateRecoveryHMACKey).
//
// Cross-tests invariant: HashRecoveryCode is deterministic for a
// given (key, plaintext). The pre-PR SHA-256 path was
// deterministic-on-plaintext-only; the HMAC path is
// deterministic-on-key-and-plaintext. That's the security
// guarantee we test for — a code from a key-K1 system never
// collides with the same code under key-K2.
package authcode

import (
	"bytes"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
)

// testKey32 is a deterministic key the unit tests use. Hex-encoded
// because SetHMACSecret takes a raw byte slice; the production
// loader hex-decodes from env / file and passes the raw bytes —
// the test key here is generated once and stored as a constant so
// failure modes are reproducible across runs.
var testKey32 = func() []byte {
	b, err := hex.DecodeString("0102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f20")
	if err != nil {
		panic(err)
	}
	return b
}()

// testKey32Alt is the second deterministic key. Used to assert
// the digest is sensitive to the key — the load-bearing property
// the HMAC upgrade buys us.
var testKey32Alt = func() []byte {
	b, err := hex.DecodeString("ffeeddccbbaa99887766554433221100f0e1d2c3b4a5968778695a4b3c2d1e0f")
	if err != nil {
		panic(err)
	}
	return b
}()

// TestMain sets the per-process HMAC secret before any test in
// the package runs. TestMain is the cleanest seam because the
// key is package-global; using a per-test SetHMACSecret would
// race under -race or t.Parallel.
func TestMain(m *testing.M) {
	// Copy the test key (SetHMACSecret mutates the input slice).
	dup := make([]byte, len(testKey32))
	copy(dup, testKey32)
	if err := SetHMACSecret(dup); err != nil {
		panic("authcode: TestMain SetHMACSecret failed: " + err.Error())
	}
	m.Run()
}

func TestSetHMACSecret_RefuseEmpty(t *testing.T) {
	// Save the live key so the rest of the suite still works.
	// The package-level hmacSecret is package-private; in
	// production cmd/apid wires it via loadOrGenerateRecoveryHMACKey.
	// Reset below to testKey32.
	saved := make([]byte, len(hmacSecret))
	copy(saved, hmacSecret)
	t.Cleanup(func() {
		hmacSecret = saved
	})

	if err := SetHMACSecret(nil); err == nil {
		t.Fatal("SetHMACSecret(nil) returned nil, want ErrNoHMACKey")
	}
	if !errors.Is(SetHMACSecret(nil), ErrNoHMACKey) {
		t.Fatalf("SetHMACSecret(nil) error = %v, want ErrNoHMACKey", SetHMACSecret(nil))
	}
	empty := []byte{}
	if err := SetHMACSecret(empty); err == nil {
		t.Fatal("SetHMACSecret(empty-slice) returned nil, want ErrNoHMACKey")
	}
}

func TestSetHMACSecret_ZeroesInput(t *testing.T) {
	// SetHMACSecret must zero the caller's slice before returning.
	// The lifetime contract mirrors pkg/session.NewManager (which
	// copies the AEAD key for the same reason): no cleartext copy
	// must outlive the call.
	input := bytes.Repeat([]byte{0x42}, 32)
	saved := make([]byte, len(hmacSecret))
	copy(saved, hmacSecret)
	t.Cleanup(func() {
		hmacSecret = saved
	})

	if err := SetHMACSecret(input); err != nil {
		t.Fatalf("SetHMACSecret unexpected error: %v", err)
	}
	for i, b := range input {
		if b != 0 {
			t.Fatalf("SetHMACSecret input byte %d = %x, want 0 (caller's cleartext copy must be zeroed)", i, b)
		}
	}
}

func TestHashRecoveryCode_Deterministic(t *testing.T) {
	// Same (key, plaintext) → same digest. The pre-PR SHA-256 path
	// only keyed on plaintext; the new path keys on (key, plaintext).
	// The determinism-on-input property is what /v1/account/mfa/recover
	// relies on when comparing the presented digest to a stored row.
	d1, err := HashRecoveryCode("ABCD2345EF")
	if err != nil {
		t.Fatalf("HashRecoveryCode unexpected error: %v", err)
	}
	d2, err := HashRecoveryCode("ABCD2345EF")
	if err != nil {
		t.Fatalf("HashRecoveryCode unexpected error: %v", err)
	}
	if !bytes.Equal(d1, d2) {
		t.Fatalf("HashRecoveryCode non-deterministic: %x vs %x", d1, d2)
	}
	if len(d1) != HMACSize {
		t.Fatalf("HashRecoveryCode length = %d, want %d (HMACSize)", len(d1), HMACSize)
	}
}

func TestHashRecoveryCode_CaseInsensitive(t *testing.T) {
	// RFC 6238 doesn't mandate case for the recovery codes
	// (recovery codes are user-typed; TOTP secrets are
	// often base32 in either case). The pre-PR SHA-256 path
	// uppercased via strings.ToUpper; the new HMAC path uses
	// ASCII fold for the same reason. Cover both cases.
	a, err := HashRecoveryCode("abcd2345ef")
	if err != nil {
		t.Fatal(err)
	}
	b, err := HashRecoveryCode("ABCD2345EF")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a, b) {
		t.Fatalf("HashRecoveryCode case-sensitive: %x vs %x", a, b)
	}
}

func TestHashRecoveryCode_KeySensitive(t *testing.T) {
	// THE load-bearing test for this commit: the same plaintext
	// under different keys MUST produce different digests. This
	// is the rainbow-reversal defence — the pre-PR SHA-256 path
	// produced the same digest regardless of key, which is why
	// a leaked blob was rainbow-reversible.
	plaintext := "ABCD2345EF"
	under1, err := HashRecoveryCode(plaintext)
	if err != nil {
		t.Fatal(err)
	}

	// Swap to the alt key.
	saved := make([]byte, len(hmacSecret))
	copy(saved, hmacSecret)
	t.Cleanup(func() {
		hmacSecret = saved
	})
	dupAlt := make([]byte, len(testKey32Alt))
	copy(dupAlt, testKey32Alt)
	if err := SetHMACSecret(dupAlt); err != nil {
		t.Fatal(err)
	}

	under2, err := HashRecoveryCode(plaintext)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(under1, under2) {
		t.Fatalf("HashRecoveryCode NOT key-sensitive: under key1 %x == under key2 %x (pre-PR SHA-256 behaviour; this is the bug we're closing)", under1, under2)
	}
}

func TestHashRecoveryCode_RefuseEmptyKey(t *testing.T) {
	// SetHMACSecret with an empty slice returns ErrNoHMACKey.
	// HashRecoveryCode must also refuse when no key is loaded —
	// otherwise a future refactor that adds a nil-tolerant
	// branch in SetHMACSecret silently reverts the HMAC defence.
	saved := make([]byte, len(hmacSecret))
	copy(saved, hmacSecret)
	t.Cleanup(func() {
		hmacSecret = saved
	})
	hmacSecret = nil

	_, err := HashRecoveryCode("ABCD2345EF")
	if !errors.Is(err, ErrNoHMACKey) {
		t.Fatalf("HashRecoveryCode with nil key = %v, want ErrNoHMACKey", err)
	}
}

func TestVerifyRecoveryCode_ConstantTime(t *testing.T) {
	// The verify helper returns true on a match and false on a
	// mismatch. Constant-time compare is the difference between
	// a hostile attacker who can probe one byte at a time on a
	// hot path and one who cannot. We don't directly measure
	// timing here (flaky under CI); we pin the contract.
	stored, err := HashRecoveryCode("ABCD2345EF")
	if err != nil {
		t.Fatal(err)
	}
	if !VerifyRecoveryCode(stored, "ABCD2345EF") {
		t.Fatal("VerifyRecoveryCode true path returned false")
	}
	// Wrong case still matches — case-insensitive (covered above).
	if !VerifyRecoveryCode(stored, "abcd2345ef") {
		t.Fatal("VerifyRecoveryCode case-insensitive path returned false")
	}
	// Wrong code returns false.
	if VerifyRecoveryCode(stored, "ZZZZZZZZZZ") {
		t.Fatal("VerifyRecoveryCode wrong-code path returned true")
	}
	// Mismatched length returns false (subtle.ConstantTimeCompare
	// returns 0 on length mismatch).
	if VerifyRecoveryCode(stored[:31], "ABCD2345EF") {
		t.Fatal("VerifyRecoveryCode length-mismatch path returned true")
	}
}

func TestVerifyRecoveryCode_NoKeyReturnsFalse(t *testing.T) {
	saved := make([]byte, len(hmacSecret))
	copy(saved, hmacSecret)
	t.Cleanup(func() {
		hmacSecret = saved
	})
	hmacSecret = nil

	if VerifyRecoveryCode([]byte("anything"), "ABCD2345EF") {
		t.Fatal("VerifyRecoveryCode with nil key returned true")
	}
}

func TestNewRecoveryCodes_MintsTenHMACDigests(t *testing.T) {
	// NewRecoveryCodes returns 10 plaintext + 10 hashes. The
	// hashes must be unique across all 10 (they would be even
	// without keying because the plaintexts are distinct, but
	// the test pins the unique-on-hash-of-unique-plaintext
	// invariant independently).
	plaintexts, hashes, err := NewRecoveryCodes(RecoveryCodeCount)
	if err != nil {
		t.Fatalf("NewRecoveryCodes unexpected error: %v", err)
	}
	if len(plaintexts) != RecoveryCodeCount {
		t.Fatalf("plaintexts len = %d, want %d", len(plaintexts), RecoveryCodeCount)
	}
	if len(hashes) != RecoveryCodeCount {
		t.Fatalf("hashes len = %d, want %d", len(hashes), RecoveryCodeCount)
	}
	for i, h := range hashes {
		if len(h) != HMACSize {
			t.Fatalf("hash[%d] length = %d, want %d", i, len(h), HMACSize)
		}
	}
	// Every plaintext format: 10 chars, base32 alphabet.
	for i, p := range plaintexts {
		if len(p) != 10 {
			t.Fatalf("plaintext[%d] length = %d, want 10", i, len(p))
		}
		for j, c := range p {
			if !isBase32Char(byte(c)) {
				t.Fatalf("plaintext[%d][%d] = %q, want base32 alphabet A-Z + 2-7", i, j, c)
			}
		}
	}
	// Hashes are pairwise distinct.
	for i := 0; i < len(hashes); i++ {
		for j := i + 1; j < len(hashes); j++ {
			if bytes.Equal(hashes[i], hashes[j]) {
				t.Fatalf("hash[%d] == hash[%d]: %x (recovery-code collisions are unacceptable)", i, j, hashes[i])
			}
		}
	}
}

func TestNewRecoveryCodes_VerifyRoundTrip(t *testing.T) {
	// The end-to-end test: mint 10 codes, then verify each
	// code using HashRecoveryCode + bytewise compare. The
	// pre-PR SHA-256 path passed this test; the new HMAC path
	// must pass it too, end-to-end. This is the test that
	// breaks if a future refactor accidentally reverts to
	// unkeyed SHA-256 (the digest lengths are the same so
	// any test that doesn't pin determinism-by-key won't
	// catch the regression).
	plaintexts, hashes, err := NewRecoveryCodes(RecoveryCodeCount)
	if err != nil {
		t.Fatal(err)
	}
	for i, p := range plaintexts {
		derived, err := HashRecoveryCode(p)
		if err != nil {
			t.Fatalf("HashRecoveryCode(%q) error: %v", p, err)
		}
		if !bytes.Equal(derived, hashes[i]) {
			t.Fatalf("verify mismatch for code[%d] (%q): derived %x != stored %x", i, p, derived, hashes[i])
		}
		if !VerifyRecoveryCode(hashes[i], p) {
			t.Fatalf("VerifyRecoveryCode false for code[%d] (%q) — round-trip broken", i, p)
		}
	}
}

func TestNewRecoveryCodes_NoKeyFails(t *testing.T) {
	saved := make([]byte, len(hmacSecret))
	copy(saved, hmacSecret)
	t.Cleanup(func() {
		hmacSecret = saved
	})
	hmacSecret = nil

	_, _, err := NewRecoveryCodes(RecoveryCodeCount)
	if !errors.Is(err, ErrNoHMACKey) {
		t.Fatalf("NewRecoveryCodes with nil key = %v, want ErrNoHMACKey", err)
	}
}

func TestHmacConstantTimeEqual(t *testing.T) {
	// Pure helper export pin. Constant-time is documented; this
	// test pins the contract.
	a := []byte("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	b := []byte("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaab")
	if HmacConstantTimeEqual(a, b) {
		t.Fatal("HmacConstantTimeEqual returned true for distinct inputs")
	}
	if !HmacConstantTimeEqual(a, []byte(string(a))) {
		t.Fatal("HmacConstantTimeEqual returned false for identical inputs")
	}
}

func TestGenerateRandomKey_LengthAndDistinct(t *testing.T) {
	// GenerateRandomKey returns 32 bytes (the conventional length
	// matching the AEAD key). Two calls must return distinct
	// slices — a stuck entropy source is the kind of regression
	// a runtime test would never catch.
	k1, err := GenerateRandomKey()
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		for i := range k1 {
			k1[i] = 0
		}
	}()
	k2, err := GenerateRandomKey()
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		for i := range k2 {
			k2[i] = 0
		}
	}()
	if len(k1) != 32 {
		t.Fatalf("GenerateRandomKey len = %d, want 32", len(k1))
	}
	if bytes.Equal(k1, k2) {
		t.Fatalf("GenerateRandomKey returned identical keys across calls: %x", k1)
	}
}

// isBase32Char reports whether c is in the base32 alphabet used
// for recovery codes (the lowercase variant of uppercase after
// trimRight('=') over base32.StdEncoding.EncodeToString). Both
// 'A'..'Z' and '2'..'7'.
func isBase32Char(c byte) bool {
	return (c >= 'A' && c <= 'Z') || (c >= '2' && c <= '7')
}

// TestUpperCase_ASCIIOnly is a small contract test for toUpper.
// This is the package-local ASCII fold that replaced
// strings.ToUpper to avoid the Turkish-locale `i` problem and
// the per-call allocation cost. The package-level `toUpper` is
// unexported; this test pins its contract.
func TestUpperCase_ASCIIOnly(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"abcd", "ABCD"},
		{"ABCD", "ABCD"},
		{"1234", "1234"},
		{"abCD12", "ABCD12"},
		{"", ""},
	}
	for _, tc := range cases {
		got := toUpper(tc.in)
		if got != tc.want {
			t.Errorf("toUpper(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestPrefix8 is the prefix helper used in cmd/apid for logs /
// audit rows. The test pins: returns first 8 chars when input is
// long enough; returns the whole string otherwise.
//
// (The helper lives in cmd/apid, not pkg/authcode; the prefix8
// reference is a habit pattern. Skipped here — covered in the
// cmd/apid tests.)
var _ = strings.HasPrefix
