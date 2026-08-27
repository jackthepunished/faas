// Test helpers for PR-C age seal/unseal round-trips.
//
// The RealService test suite needs to (1) generate a throwaway
// X25519 identity+recipient pair, and (2) seal a test "ghs_…"
// token under that recipient so the test can pre-seed a
// github_installations row. These helpers live here as test-only
// helpers (build is _test.go) so production binaries don't pull
// in the secretbox path for any caller beyond the loader in
// cmd/githubd.
//
// newTestAgeKeypair is the test analogue of secretbox.LoadHostKey:
// the production code (cmd/githubd/main.go:120) loads the host
// identity from disk with strict 0o400 perms. Tests don't need a
// fixture file — they mint a keypair in memory and pass the
// X25519Recipient + X25519Identity straight to RealService.
//
// sealForTest uses the canonical pkg/secretbox.SealOne path that
// RealService.ExchangeOAuthCode + ensureInstallToken use at
// runtime, so the produced ciphertext is byte-for-byte the same
// shape (ASCII-armoured age stanza header → ChaCha20-Poly1305
// body, wrapped in a JSON Envelope {key: value}). Tests
// asserting on the age magic prefix "age-encryption.org/v1" then
// exercise the same loader path the production reload does.
package githubd

import (
	"crypto/rand"
	"strings"
	"testing"

	"filippo.io/age"

	"github.com/onebox-faas/faas/pkg/secretbox"
)

// installTokenTestSealKey mirrors production's
// installTokenSealKey ("GITHUB_INSTALL_TOKEN") in realservice.go.
// Kept in this test file only so production's realservice.go can
// change the const without invalidating the test wire (the
// realservice test asserts on a successful RoundTrip — if the
// production constant drifts, ensureInstallToken would fail to
// find the key and the test would surface the drift through its
// error path, not here).
const installTokenTestSealKey = "GITHUB_INSTALL_TOKEN"

// newTestAgeKeypair generates a fresh X25519 identity+recipient
// pair in memory. Both halves MUST travel together through the
// test (the recipient seals, the identity unseals; cross-mixing
// the two in the same test would produce a confusing failure).
func newTestAgeKeypair(t *testing.T) (*age.X25519Identity, *age.X25519Recipient) {
	t.Helper()
	ident, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatalf("GenerateX25519Identity: %v", err)
	}
	return ident, ident.Recipient()
}

// sealForTest seals a test "ghs_…" token under recipient using
// the same pkg/secretbox.SealOne path production uses. The
// returned blob starts with the age ASCII magic
// ("age-encryption.org/v1") so RealService exchange-path tests
// can assert the bytea is age-armoured (PR-C spec section
// "End-state after PR-C", row "Install token at rest").
//
// maxValueBytes mirrors production's maxInstallTokenBytes.
func sealForTest(t *testing.T, recipient *age.X25519Recipient, token string) ([]byte, error) {
	t.Helper()
	if recipient == nil {
		t.Fatalf("sealForTest: nil recipient")
	}
	return secretbox.SealOne(recipient, installTokenTestSealKey, token, maxInstallTokenBytes)
}

func TestSealForTestAllowsProviderTokenHeadroom(t *testing.T) {
	_, recipient := newTestAgeKeypair(t)
	token := "ghs_" + strings.Repeat("x", 1024)
	if _, err := sealForTest(t, recipient, token); err != nil {
		t.Fatalf("seal provider-sized installation token: %v", err)
	}
}

// _ pins crypto/rand so gofmt/goimports doesn't drop it during a
// future refactor that switches to ed25519/age-only generation.
var _ = rand.Reader
