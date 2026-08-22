// Whitebox tests for pkg/oidc.NewVerifier + Verify +
// subjectPatternAsClaim + OIDCBearerScopes. The handler tests in
// handler_test.go already cover the consumer surface via a fake
// Verifier; this file drives the production edgeJWKSVerifier
// against a real httptest JWKS endpoint + go-jose-signed JWTs so
// every branch of the production Verify path is exercised.
//
// Helpers (jwksServer, mintToken, rs256Fixture) are duplicated
// from pkg/edgejwks/verifier_test.go because the edgejwks test
// helpers are package-private (package edgejwks_test blackbox)
// and the OIDC tests live in package oidc.

package oidc

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/onebox-faas/faas/pkg/edgejwks"
)

// --- Helpers (copied from pkg/edgejwks/verifier_test.go) ----------------

func jwksServerURL(t *testing.T, pubJWK jose.JSONWebKey, hits *atomic.Int64) string {
	t.Helper()
	set := jose.JSONWebKeySet{Keys: []jose.JSONWebKey{pubJWK}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if hits != nil {
			hits.Add(1)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(&set)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func mintJWTToken(t *testing.T, priv *rsa.PrivateKey, kid string, std jwt.Claims, custom map[string]any) string {
	t.Helper()
	signer, err := jose.NewSigner(
		jose.SigningKey{Algorithm: jose.RS256, Key: priv},
		(&jose.SignerOptions{}).WithType("JWT").WithHeader(jose.HeaderKey("kid"), kid),
	)
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	b := jwt.Signed(signer).Claims(std)
	if len(custom) > 0 {
		b = b.Claims(custom)
	}
	out, err := b.Serialize()
	if err != nil {
		t.Fatalf("Serialize: %v", err)
	}
	return out
}

func rs256Key(t *testing.T, kid string) (*rsa.PrivateKey, jose.JSONWebKey) {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa.GenerateKey: %v", err)
	}
	pub := jose.JSONWebKey{
		Key:       &priv.PublicKey,
		KeyID:     kid,
		Algorithm: string(jose.RS256),
		Use:       "sig",
	}
	return priv, pub
}

// silentLogger returns a slog.Logger that discards output. The
// OnFetchErr hook calls Warn with the URL + err, but most
// happy-path tests don't fire it; the silent handler keeps test
// output clean.
func silentLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(&bytes.Buffer{}, &slog.HandlerOptions{Level: slog.LevelError}))
}

// --- OIDCBearerScopes ----------------------------------------------------

func TestOIDCBearerScopes_FreshSlicePerCall(t *testing.T) {
	a := OIDCBearerScopes()
	b := OIDCBearerScopes()
	if len(a) != 1 || a[0] != "deploy:write" {
		t.Errorf("OIDCBearerScopes = %v, want [deploy:write]", a)
	}
	if &a[0] == &b[0] {
		t.Error("OIDCBearerScopes must return a fresh slice per call (mirrors APIKey.Scopes)")
	}
}

// --- subjectPatternAsClaim ----------------------------------------------

func TestSubjectPatternAsClaim_EmptyReturnsNil(t *testing.T) {
	got := subjectPatternAsClaim("")
	if got != nil {
		t.Errorf("subjectPatternAsClaim(\"\") = %v, want nil", got)
	}
}

func TestSubjectPatternAsClaim_NonEmpty(t *testing.T) {
	got := subjectPatternAsClaim("^ci-job-[0-9]+$")
	if got == nil {
		t.Fatal("subjectPatternAsClaim(non-empty) = nil, want map")
	}
	if v, ok := got["sub"]; !ok || v != "^ci-job-[0-9]+$" {
		t.Errorf("subjectPatternAsClaim = %v, want map[sub:^ci-job-[0-9]+$]", got)
	}
}

// --- NewVerifier ---------------------------------------------------------

func TestNewVerifier_NilLogger(t *testing.T) {
	// Nil logger must not panic. The OnFetchErr hook checks
	// for nil so the path is safe; the log field stays nil
	// on the verifier struct (verified indirectly via Verify
	// hitting the cache fetch failure path).
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("NewVerifier(nil log) panicked: %v", r)
		}
	}()
	v := NewVerifier(nil)
	if v == nil {
		t.Fatal("NewVerifier returned nil")
	}
}

// --- Verify (production edgeJWKSVerifier) -------------------------------

func newTestVerifier(t *testing.T, log *slog.Logger) Verifier {
	t.Helper()
	return NewVerifier(log)
}

func TestVerify_NilPolicy(t *testing.T) {
	v := newTestVerifier(t, silentLogger())
	_, err := v.Verify(context.Background(), "any.token.value", nil)
	if err == nil {
		t.Fatal("expected error on nil policy")
	}
	if !strings.Contains(err.Error(), "nil trust policy") {
		t.Errorf("err = %v, want nil-policy message", err)
	}
}

func TestVerify_HappyPath_RegistersOnColdCache(t *testing.T) {
	priv, pub := rs256Key(t, "k1")
	var hits atomic.Int64
	jwksURL := jwksServerURL(t, pub, &hits)

	v := NewVerifier(silentLogger())
	policy := &OIDCTrustPolicy{
		IssuerURL:  "https://idp.example.test",
		JWKSURL:    jwksURL,
		Audience:   []string{"faas-deploy"},
		Algorithms: []string{"RS256"},
	}
	tok := mintJWTToken(t, priv, "k1", jwt.Claims{
		Issuer:   "https://idp.example.test",
		Audience: jwt.Audience{"faas-deploy"},
		Subject:  "ci-job-42",
		Expiry:   jwt.NewNumericDate(time.Now().Add(5 * time.Minute)),
	}, nil)

	claims, err := v.Verify(context.Background(), tok, policy)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if claims.Subject != "ci-job-42" {
		t.Errorf("Subject = %q, want ci-job-42", claims.Subject)
	}
	if claims.Issuer != "https://idp.example.test" {
		t.Errorf("Issuer = %q, want https://idp.example.test", claims.Issuer)
	}
	if len(claims.Aud) != 1 || claims.Aud[0] != "faas-deploy" {
		t.Errorf("Aud = %v, want [faas-deploy]", claims.Aud)
	}
	// Cold path registered; hits==1.
	if got := hits.Load(); got != 1 {
		t.Errorf("JWKS hits = %d, want 1 (cold-register fetch)", got)
	}
}

func TestVerify_HotPath_SkipsRegister(t *testing.T) {
	priv, pub := rs256Key(t, "k2")
	var hits atomic.Int64
	jwksURL := jwksServerURL(t, pub, &hits)

	v := NewVerifier(silentLogger())
	policy := &OIDCTrustPolicy{
		IssuerURL:  "https://idp.example.test",
		JWKSURL:    jwksURL,
		Audience:   []string{"faas-deploy"},
		Algorithms: []string{"RS256"},
	}
	tok := mintJWTToken(t, priv, "k2", jwt.Claims{
		Issuer:   "https://idp.example.test",
		Audience: jwt.Audience{"faas-deploy"},
		Subject:  "ci-job-43",
		Expiry:   jwt.NewNumericDate(time.Now().Add(5 * time.Minute)),
	}, nil)
	// First Verify warms the cache (hits=1).
	if _, err := v.Verify(context.Background(), tok, policy); err != nil {
		t.Fatalf("first Verify: %v", err)
	}
	// Second Verify uses the hot path (hits stays at 1).
	if _, err := v.Verify(context.Background(), tok, policy); err != nil {
		t.Fatalf("second Verify: %v", err)
	}
	if got := hits.Load(); got != 1 {
		t.Errorf("JWKS hits = %d, want 1 (hot path skips Register)", got)
	}
}

func TestVerify_BadSignature(t *testing.T) {
	priv1, pub1 := rs256Key(t, "k3")
	priv2, _ := rs256Key(t, "k3") // different key, same kid
	_ = pub1 // unused after this point
	jwksURL := jwksServerURL(t, jose.JSONWebKey{
		Key:       &priv1.PublicKey,
		KeyID:     "k3",
		Algorithm: string(jose.RS256),
		Use:       "sig",
	}, nil)

	v := NewVerifier(silentLogger())
	policy := &OIDCTrustPolicy{
		IssuerURL:  "https://idp.example.test",
		JWKSURL:    jwksURL,
		Audience:   []string{"faas-deploy"},
		Algorithms: []string{"RS256"},
	}
	tok := mintJWTToken(t, priv2, "k3", jwt.Claims{
		Issuer:   "https://idp.example.test",
		Audience: jwt.Audience{"faas-deploy"},
		Subject:  "ci-job-44",
		Expiry:   jwt.NewNumericDate(time.Now().Add(5 * time.Minute)),
	}, nil)

	_, err := v.Verify(context.Background(), tok, policy)
	if err == nil {
		t.Fatal("expected bad-signature error")
	}
	if !strings.Contains(err.Error(), "signature") {
		t.Errorf("err = %v, want 'signature' in message", err)
	}
}

func TestVerify_Expired(t *testing.T) {
	priv, pub := rs256Key(t, "k4")
	jwksURL := jwksServerURL(t, pub, nil)
	v := NewVerifier(silentLogger())
	policy := &OIDCTrustPolicy{
		IssuerURL:  "https://idp.example.test",
		JWKSURL:    jwksURL,
		Audience:   []string{"faas-deploy"},
		Algorithms: []string{"RS256"},
	}
	tok := mintJWTToken(t, priv, "k4", jwt.Claims{
		Issuer:   "https://idp.example.test",
		Audience: jwt.Audience{"faas-deploy"},
		Subject:  "ci-job-45",
		Expiry:   jwt.NewNumericDate(time.Now().Add(-5 * time.Minute)),
	}, nil)
	_, err := v.Verify(context.Background(), tok, policy)
	if err == nil {
		t.Fatal("expected expired error")
	}
	if !strings.Contains(err.Error(), "expired") {
		t.Errorf("err = %v, want 'expired' in message", err)
	}
}

func TestVerify_WrongIssuer(t *testing.T) {
	priv, pub := rs256Key(t, "k5")
	jwksURL := jwksServerURL(t, pub, nil)
	v := NewVerifier(silentLogger())
	policy := &OIDCTrustPolicy{
		IssuerURL:  "https://idp.example.test",
		JWKSURL:    jwksURL,
		Audience:   []string{"faas-deploy"},
		Algorithms: []string{"RS256"},
	}
	tok := mintJWTToken(t, priv, "k5", jwt.Claims{
		Issuer:   "https://attacker.example.com", // wrong
		Audience: jwt.Audience{"faas-deploy"},
		Subject:  "ci-job-46",
		Expiry:   jwt.NewNumericDate(time.Now().Add(5 * time.Minute)),
	}, nil)
	_, err := v.Verify(context.Background(), tok, policy)
	if err == nil {
		t.Fatal("expected wrong-issuer error")
	}
	if !strings.Contains(err.Error(), "issuer") {
		t.Errorf("err = %v, want 'issuer' in message", err)
	}
}

func TestVerify_WrongAudience(t *testing.T) {
	priv, pub := rs256Key(t, "k6")
	jwksURL := jwksServerURL(t, pub, nil)
	v := NewVerifier(silentLogger())
	policy := &OIDCTrustPolicy{
		IssuerURL:  "https://idp.example.test",
		JWKSURL:    jwksURL,
		Audience:   []string{"faas-deploy"},
		Algorithms: []string{"RS256"},
	}
	tok := mintJWTToken(t, priv, "k6", jwt.Claims{
		Issuer:   "https://idp.example.test",
		Audience: jwt.Audience{"some-other-audience"}, // wrong
		Subject:  "ci-job-47",
		Expiry:   jwt.NewNumericDate(time.Now().Add(5 * time.Minute)),
	}, nil)
	_, err := v.Verify(context.Background(), tok, policy)
	if err == nil {
		t.Fatal("expected wrong-audience error")
	}
	if !strings.Contains(err.Error(), "audience") {
		t.Errorf("err = %v, want 'audience' in message", err)
	}
}

func TestVerify_SubjectPatternMatch(t *testing.T) {
	priv, pub := rs256Key(t, "k7")
	jwksURL := jwksServerURL(t, pub, nil)
	v := NewVerifier(silentLogger())
	policy := &OIDCTrustPolicy{
		IssuerURL:      "https://idp.example.test",
		JWKSURL:        jwksURL,
		Audience:       []string{"faas-deploy"},
		Algorithms:     []string{"RS256"},
		SubjectPattern: "^ci-job-[0-9]+$",
	}
	tok := mintJWTToken(t, priv, "k7", jwt.Claims{
		Issuer:   "https://idp.example.test",
		Audience: jwt.Audience{"faas-deploy"},
		Subject:  "ci-job-100",
		Expiry:   jwt.NewNumericDate(time.Now().Add(5 * time.Minute)),
	}, nil)
	if _, err := v.Verify(context.Background(), tok, policy); err != nil {
		t.Fatalf("Verify(pattern match): %v", err)
	}
}

func TestVerify_SubjectPatternMiss(t *testing.T) {
	priv, pub := rs256Key(t, "k8")
	jwksURL := jwksServerURL(t, pub, nil)
	v := NewVerifier(silentLogger())
	policy := &OIDCTrustPolicy{
		IssuerURL:      "https://idp.example.test",
		JWKSURL:        jwksURL,
		Audience:       []string{"faas-deploy"},
		Algorithms:     []string{"RS256"},
		SubjectPattern: "^ci-job-[0-9]+$",
	}
	tok := mintJWTToken(t, priv, "k8", jwt.Claims{
		Issuer:   "https://idp.example.test",
		Audience: jwt.Audience{"faas-deploy"},
		Subject:  "deploy-bot", // doesn't match the pattern
		Expiry:   jwt.NewNumericDate(time.Now().Add(5 * time.Minute)),
	}, nil)
	_, err := v.Verify(context.Background(), tok, policy)
	if err == nil {
		t.Fatal("expected pattern-miss error")
	}
}

func TestVerify_RegisterFailure_NoJWKS(t *testing.T) {
	// Register against a URL that 500s on every fetch. The
	// Register call returns an error and Verify surfaces it.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	v := NewVerifier(silentLogger())
	policy := &OIDCTrustPolicy{
		IssuerURL: "https://idp.example.test",
		JWKSURL:   srv.URL,
		Audience:  []string{"faas-deploy"},
	}
	_, err := v.Verify(context.Background(), "any.token", policy)
	if err == nil {
		t.Fatal("expected register-failure error")
	}
}

func TestVerify_OnFetchErr_LoggedViaBuffer(t *testing.T) {
	// The OnFetchErr hook is called when the verifier's
	// cache.Get fires its internal fetch and the server
	// returns a non-OK status. Use a properly-signed token
	// (so the verifier's envelope parse succeeds and the
	// kid extraction reaches cache.Get) against a 500-returning
	// JWKS URL so the fetch fails and the hook fires.
	priv, _ := rs256Key(t, "k_hook")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	v := NewVerifier(log)
	policy := &OIDCTrustPolicy{
		IssuerURL:  "https://idp.example.test",
		JWKSURL:    srv.URL,
		Audience:   []string{"faas-deploy"},
		Algorithms: []string{"RS256"},
	}
	tok := mintJWTToken(t, priv, "k_hook", jwt.Claims{
		Issuer:   "https://idp.example.test",
		Audience: jwt.Audience{"faas-deploy"},
		Subject:  "ci-job-hook",
		Expiry:   jwt.NewNumericDate(time.Now().Add(5 * time.Minute)),
	}, nil)
	_, _ = v.Verify(context.Background(), tok, policy)
	// OnFetchErr is wired into edgejwks.Cache.Options; the
	// production hook calls log.Warn with "oidc: jwks fetch
	// failed" + the URL. Assert the buffer contains the
	// fixed sentinel so the log != nil branch is exercised.
	if !strings.Contains(buf.String(), "jwks fetch failed") {
		t.Errorf("OnFetchErr hook did not fire; buffer = %q", buf.String())
	}
}

// silence unused-import warning if edgejwks becomes unused
var _ = edgejwks.NewCache