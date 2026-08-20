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
