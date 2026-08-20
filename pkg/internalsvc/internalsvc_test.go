// Unit tests for pkg/internalsvc (ADR-119). No PG needed.
// Pins six contracts:
//
//   - Round-trip: Mint then Verify returns the same svcName.
//   - Wrong audience: token with aud != gregale.internal fails
//     with ErrAudienceMismatch.
//   - Expired: token with exp in the past fails with
//     ErrExpired.
//   - Unknown service: token whose sub is not in the
//     allowlist fails with ErrUnknownService (signature is
//     not verified in this case — the allowlist IS the
//     trust boundary).
//   - Tampered signature: flip a byte in the signature; fails
//     with ErrSignatureInvalid.
//   - Missing kid: Mint without kid still produces a working
//     token (the verifier keys off svcName, not kid).
//
// Drift guard: the package-level Audience constant must stay
// at "gregale.internal". A future contributor who renames it
// will break pkg/internalsvc_test.go's TestAudienceStable.

package internalsvc_test

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/onebox-faas/faas/pkg/internalsvc"
)

// TestAudienceStable pins the Audience constant. The drift
// guard at pkg/api/public_auth_constants_test.go catches the
// public_auth_mode enum drift; this test catches the audience
// string drift independently because the audience is the JWT
// trust boundary, not just an enum value.
func TestAudienceStable(t *testing.T) {
	if internalsvc.Audience != "gregale.internal" {
		t.Fatalf("Audience: got %q, want %q", internalsvc.Audience, "gregale.internal")
	}
}

// TestMintVerifyRoundTrip is the happy-path pin: mint a
// token, verify, get the same svcName back.
func TestMintVerifyRoundTrip(t *testing.T) {
	priv, pub, err := internalsvc.GenerateKeypair()
	if err != nil {
		t.Fatalf("GenerateKeypair: %v", err)
	}
	allowed := map[string]ed25519.PublicKey{
		"schedd": pub,
	}

	tok, err := internalsvc.Mint("schedd", 30*time.Second, nil, priv, "kid-test-001")
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	if tok == "" {
		t.Fatalf("Mint returned empty token")
	}

	gotSvc, err := internalsvc.Verify(tok, allowed)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if gotSvc != "schedd" {
		t.Errorf("Verify returned svcName %q, want %q", gotSvc, "schedd")
	}
}

// TestVerifyRejectsWrongAudience mints a token but with a
// non-Audience claim. The verify must fail with
// ErrAudienceMismatch. (Mint pins Audience at the package
// level, so to test this path we build the token manually
// using go-jose directly.)
func TestVerifyRejectsWrongAudience(t *testing.T) {
	priv, pub, err := internalsvc.GenerateKeypair()
	if err != nil {
		t.Fatalf("GenerateKeypair: %v", err)
	}
	allowed := map[string]ed25519.PublicKey{
		"schedd": pub,
	}

	// MintWithAudience is the test-only export added in
	// PR #1009 round-3 — the production Mint pins Audience
	// at gregale.internal, so the wrong-audience path needs
	// a parameterized surface. The substring that the
	// bridge returns verbatim is "aud claim does not match"
	// (internalsvc.go:77) — the substring-match table in
	// pkg/gateway/internal_svc_auth.go:300 keys off it.
	tok, err := internalsvc.MintWithAudience("schedd", 30*time.Second, nil, priv, "kid-test-aud", "foo")
	if err != nil {
		t.Fatalf("MintWithAudience: %v", err)
	}

	_, err = internalsvc.Verify(tok, allowed)
	if !errors.Is(err, internalsvc.ErrAudienceMismatch) {
		t.Errorf("Verify with wrong aud: got %v, want ErrAudienceMismatch", err)
	}
}

// TestVerifyRejectsExpired mints a token with negative TTL.
// Must fail with ErrExpired.
func TestVerifyRejectsExpired(t *testing.T) {
	priv, pub, err := internalsvc.GenerateKeypair()
	if err != nil {
		t.Fatalf("GenerateKeypair: %v", err)
	}
	allowed := map[string]ed25519.PublicKey{
		"schedd": pub,
	}

	// TTL=-1h puts exp one hour in the past, well past the
	// 30s VerifyLeeway.
	tok, err := internalsvc.Mint("schedd", -1*time.Hour, nil, priv, "kid-test-001")
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}

	_, err = internalsvc.Verify(tok, allowed)
	if !errors.Is(err, internalsvc.ErrExpired) {
		t.Errorf("Verify with expired token: got %v, want ErrExpired", err)
	}
}

// TestVerifyRejectsUnknownSvc mints a token for "schedd" but
// the allowlist contains only "meterd". Must fail with
// ErrUnknownService — the per-service allowlist contract.
func TestVerifyRejectsUnknownSvc(t *testing.T) {
	priv, pub, err := internalsvc.GenerateKeypair()
	if err != nil {
		t.Fatalf("GenerateKeypair: %v", err)
	}
	// Allowlist does NOT include schedd.
	allowed := map[string]ed25519.PublicKey{
		"meterd": pub,
	}

	tok, err := internalsvc.Mint("schedd", 30*time.Second, nil, priv, "kid-test-001")
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}

	_, err = internalsvc.Verify(tok, allowed)
	if !errors.Is(err, internalsvc.ErrUnknownService) {
		t.Errorf("Verify with unknown svcName: got %v, want ErrUnknownService", err)
	}
}

// TestVerifyRejectsTamperedSignature flips a byte in the
// signature segment. The verifier must reject with
// ErrSignatureInvalid.
func TestVerifyRejectsTamperedSignature(t *testing.T) {
	priv, pub, err := internalsvc.GenerateKeypair()
	if err != nil {
		t.Fatalf("GenerateKeypair: %v", err)
	}
	allowed := map[string]ed25519.PublicKey{
		"schedd": pub,
	}

	tok, err := internalsvc.Mint("schedd", 30*time.Second, nil, priv, "kid-test-001")
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}

	// Flip the last byte of the signature segment. JWT
	// format is header.payload.signature; split on '.' and
	// mutate the third segment.
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		t.Fatalf("token has %d segments, want 3", len(parts))
	}
	sig := []byte(parts[2])
	if sig[0] == 'A' {
		sig[0] = 'B'
	} else {
		sig[0] = 'A'
	}
	tampered := parts[0] + "." + parts[1] + "." + string(sig)

	_, err = internalsvc.Verify(tampered, allowed)
	if !errors.Is(err, internalsvc.ErrSignatureInvalid) {
		t.Errorf("Verify with tampered signature: got %v, want ErrSignatureInvalid", err)
	}
}

// TestVerifyRejectsWrongKey mints a token with key A, then
// verifies against key B (whose svcName IS in the allowlist).
// Must fail with ErrSignatureInvalid (signature mismatch).
func TestVerifyRejectsWrongKey(t *testing.T) {
	privA, _, err := internalsvc.GenerateKeypair()
	if err != nil {
		t.Fatalf("GenerateKeypair A: %v", err)
	}
	_, pubB, err := internalsvc.GenerateKeypair()
	if err != nil {
		t.Fatalf("GenerateKeypair B: %v", err)
	}
	allowed := map[string]ed25519.PublicKey{
		"schedd": pubB, // wrong key for the svcName
	}

	tok, err := internalsvc.Mint("schedd", 30*time.Second, nil, privA, "kid-test-001")
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}

	_, err = internalsvc.Verify(tok, allowed)
	if !errors.Is(err, internalsvc.ErrSignatureInvalid) {
		t.Errorf("Verify with wrong key: got %v, want ErrSignatureInvalid", err)
	}
}

// TestMintRejectsEmptySvcName asserts the call-site
// validation that prevents an operator from minting a token
// with no sub claim (which would make the allowlist
// meaningless).
func TestMintRejectsEmptySvcName(t *testing.T) {
	priv, _, err := internalsvc.GenerateKeypair()
	if err != nil {
		t.Fatalf("GenerateKeypair: %v", err)
	}

	_, err = internalsvc.Mint("", 30*time.Second, nil, priv, "kid-test-001")
	if err == nil {
		t.Fatalf("Mint with empty svcName: expected error, got nil")
	}
}

// TestKidFromPubStable pins the kid derivation format (round-3
// peer-review #7: kid format divergence). A drift here breaks
// diagnostic logs that key off kid to identify the minter —
// schedd's boot log would show a kid that no longer matches
// what the same pubkey produces inside pkg/internalsvc.
//
// The contract: KidFromPub returns base64url(sha256(pub)[:16]).
// Pin the length (22 chars), the alphabet (URL-safe), and the
// round-trip determinism (same pubkey → same kid). If any of
// these change, the test fails narratively so a future
// contributor is forced to acknowledge the kid-format change.
func TestKidFromPubStable(t *testing.T) {
	_, pub, err := internalsvc.GenerateKeypair()
	if err != nil {
		t.Fatalf("GenerateKeypair: %v", err)
	}
	kid := internalsvc.KidFromPub(pub)
	if len(kid) != 22 {
		t.Errorf("kid length = %d, want 22 (base64url of 16 bytes)", len(kid))
	}
	// base64.RawURLEncoding alphabet: A-Z a-z 0-9 _ -
	for _, r := range kid {
		if !((r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') ||
			(r >= '0' && r <= '9') || r == '_' || r == '-') {
			t.Errorf("kid contains non-URL-safe char %q (full=%q)", r, kid)
			break
		}
	}
	// Determinism — same pubkey must produce the same kid.
	if again := internalsvc.KidFromPub(pub); again != kid {
		t.Errorf("KidFromPub is non-deterministic: %q vs %q", kid, again)
	}
}

// TestMintAutoDerivesKidFromKidFromPub pins the connection
// between the package helper and the auto-derive path in
// Mint. If a future contributor changes Mint's auto-derive
// to a different shape, the test fires. Round-3 #7: this is
// the load-bearing drift guard for the kid-format
// unification.
func TestMintAutoDerivesKidFromKidFromPub(t *testing.T) {
	priv, pub, err := internalsvc.GenerateKeypair()
	if err != nil {
		t.Fatalf("GenerateKeypair: %v", err)
	}
	tok, err := internalsvc.Mint("schedd", 30*time.Second, nil, priv, "")
	if err != nil {
		t.Fatalf("Mint with empty kid: %v", err)
	}
	// Decode the JWT header and pull the kid claim. The
	// first segment is base64url-encoded JSON.
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		t.Fatalf("token has %d segments, want 3", len(parts))
	}
	headerJSON, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		t.Fatalf("decode header: %v", err)
	}
	var header struct {
		Kid string `json:"kid"`
	}
	if err := json.Unmarshal(headerJSON, &header); err != nil {
		t.Fatalf("unmarshal header: %v", err)
	}
	if header.Kid != internalsvc.KidFromPub(pub) {
		t.Errorf("auto-derived kid = %q, want KidFromPub = %q", header.Kid, internalsvc.KidFromPub(pub))
	}
}

// TestMintRejectsBadKeySize asserts that a non-Ed25519 key is
// rejected at the call site (defensive: catches a future
// contributor passing an HMAC key by mistake).
func TestMintRejectsBadKeySize(t *testing.T) {
	// 32 bytes is wrong for Ed25519.PrivateKey (should be 64).
	bad := make([]byte, 32)

	_, err := internalsvc.Mint("schedd", 30*time.Second, nil, bad, "kid-test-001")
	if err == nil {
		t.Fatalf("Mint with bad key size: expected error, got nil")
	}
}
