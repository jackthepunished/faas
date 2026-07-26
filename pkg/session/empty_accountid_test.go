// White-box test file for the one Manager guard that cannot be
// exercised through the public API: Verify's `env.AccountID == ""`
// branch (manager.go:255-257). All other tests in this package live
// in `package session_test` (manager_test.go) and reach the Manager
// through its public surface; this file uses `package session`
// because forging a session-style cookie — same AAD, same envelope
// shape — requires a direct call to the unexported `gcm.Seal`. We
// keep the white-box exception narrowly scoped to one test rather
// than a global testability seam.
package session

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"testing"
	"time"
)

// sealForTest produces a base64url(nonce || ciphertext) cookie
// shaped exactly like the one Issue() emits, but with caller-
// controlled Envelope fields. Used to feed crafted envelopes
// into Verify so we can test the downstream guards (AccountID
// empty, ExpiresAt past) without standing up a third-party seal
// helper. Mirrors IssueWithMFAFlag's nonce generation byte-for-byte.
func sealForTest(t *testing.T, m *Manager, env Envelope) string {
	t.Helper()
	plaintext, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("sealForTest: marshal: %v", err)
	}
	nonce := make([]byte, m.gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		t.Fatalf("sealForTest: read nonce: %v", err)
	}
	sealed := m.gcm.Seal(nonce, nonce, plaintext, nil)
	return base64.RawURLEncoding.EncodeToString(sealed)
}

// TestVerify_RejectsEmptyAccountID covers the `env.AccountID == ""`
// branch in Verify (manager.go:250-252). The Manager is the only
// place that can seal a session-shaped cookie, so this test uses
// the package-local sealForTest helper to mint one. Without this
// guard, a bug that accepted a zero-account envelope would let an
// attacker (or a misconfigured handler) produce a cookie that
// passes Verify but resolves to "no account" downstream — a
// silent auth bypass.
//
// The clock is frozen at the envelope's IssuedAt so the upstream
// ExpiresAt check (manager.go:247-249) doesn't short-circuit and
// return ErrInvalid before the AccountID check runs.
func TestVerify_RejectsEmptyAccountID(t *testing.T) {
	m, err := NewManager(testKey(t), time.Hour)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	issued := time.Unix(1_700_000_000, 0)
	m.SetClock(func() time.Time { return issued })
	cookie := sealForTest(t, m, Envelope{
		AccountID: "",
		IssuedAt:  issued,
		ExpiresAt: issued.Add(time.Hour),
	})
	if _, err := m.Verify(cookie); !errors.Is(err, ErrInvalid) {
		t.Errorf("Verify(empty account) = %v, want ErrInvalid", err)
	}
}

// testKey returns a deterministic 32-byte key for the white-box
// tests in this file. Mirrors the `key(t)` helper in
// manager_test.go but lives next to the caller as a deliberate
// style choice: every white-box test file in the project is
// self-contained, so a future reader scanning this file knows
// they don't need to grep the rest of the package to understand
// it. The duplication is one byte of crypto noise (a 32-byte
// zero-pattern key), not a real test surface.
func testKey(t *testing.T) []byte {
	t.Helper()
	k := make([]byte, 32)
	for i := range k {
		k[i] = byte(i)
	}
	return k
}

// TestVerify_RejectsMalformedCiphertext covers the `json.Unmarshal`
// error branch in Verify (manager.go:243-246). The AEAD opens
// successfully but the plaintext is not valid JSON — for example,
// ciphertext we sealed from a non-JSON byte slice. Verify must
// refuse rather than return a zero-value Envelope the handler
// might treat as authenticated.
func TestVerify_RejectsMalformedCiphertext(t *testing.T) {
	m, err := NewManager(testKey(t), time.Hour)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	cookie := sealRawForTest(t, m, []byte("not-json-at-all"))
	if _, err := m.Verify(cookie); !errors.Is(err, ErrInvalid) {
		t.Errorf("Verify(garbage plaintext) = %v, want ErrInvalid", err)
	}
}

// sealRawForTest seals arbitrary raw bytes (no JSON marshalling)
// the same way Issue() does internally — same nonce layout, same
// no-AAD seal — and returns the base64 cookie. Lets the test feed
// non-JSON plaintexts into Verify to exercise the unmarshal guard.
func sealRawForTest(t *testing.T, m *Manager, plaintext []byte) string {
	t.Helper()
	nonce := make([]byte, m.gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		t.Fatalf("sealRawForTest: read nonce: %v", err)
	}
	sealed := m.gcm.Seal(nonce, nonce, plaintext, nil)
	return base64.RawURLEncoding.EncodeToString(sealed)
}
