package oci

// registry_auth_test pins the AuthPuller seam and the scrub helper
// for issue #461 / ADR-062 (per-app private-registry Basic Auth).
//
// The fakeRegistry in registry_test.go doesn't model the realm
// endpoint's Basic Auth request — that's the responsibility of
// auth_test.go's TestFetchToken_BasicAuth. Here we extend the
// surface to cover:
//   - scrubAuthFromError strips the username/password + base64
//     composite from any error string.
//   - RegistryClient satisfies AuthPuller (compile-time interface
//     check) and DefaultPuller does too.
//   - PullDigestWithAuth / PullImageConfigWithAuth / PullLayersWithAuth
//     end-to-end: when the registry challenges, the realm endpoint
//     receives the Basic Auth header; subsequent manifest + blob
//     fetches carry the bearer token.

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestScrubAuthFromError_ScrubsAllForms pins the defence-in-depth
// scrubber: the username, the password, and the base64("user:pass")
// composite are all stripped from the error string, and the literal
// "Authorization: Basic <...>" / "Authorization: Bearer <...>"
// shape is collapsed to "Authorization: REDACTED".
func TestScrubAuthFromError_ScrubsAllForms(t *testing.T) {
	auth := &BasicAuth{Username: "alice", Password: "s3cret-MARKER"}
	b64 := base64.StdEncoding.EncodeToString([]byte("alice:s3cret-MARKER"))
	err := fmt.Errorf("oci: realm returned 401: server saw Authorization: Basic %s for user alice with password s3cret-MARKER", b64)

	scrubbed := scrubAuthFromError(err, auth).Error()
	for _, leak := range []string{"alice", "s3cret-MARKER", b64} {
		if strings.Contains(scrubbed, leak) {
			t.Errorf("scrubbed error leaks %q: %s", leak, scrubbed)
		}
	}
	if !strings.Contains(scrubbed, "REDACTED") {
		t.Errorf("scrubbed error missing REDACTED marker: %s", scrubbed)
	}
}

// TestScrubAuthFromError_NoAuthStillScrubsHeader pins that even
// without an auth to scrub, any "Authorization: <type> <token>"
// substring is collapsed — a registry-returned body that echoes the
// challenge header still gets sanitized.
func TestScrubAuthFromError_NoAuthStillScrubsHeader(t *testing.T) {
	err := errors.New("oci: 401 with header Authorization: Bearer eyJhbGciOi... and another Authorization: Basic YWxpY2U6c2VjcmV0")
	scrubbed := scrubAuthFromError(err, nil).Error()
	if strings.Contains(scrubbed, "eyJhbGciOi") || strings.Contains(scrubbed, "YWxpY2U6c2VjcmV0") {
		t.Errorf("scrubbed error leaks bearer/basic tokens: %s", scrubbed)
	}
	if !strings.Contains(scrubbed, "Authorization: REDACTED") {
		t.Errorf("scrubbed error missing Authorization: REDACTED marker: %s", scrubbed)
	}
}

// TestScrubAuthFromError_NilInput pins idempotence: nil in, nil out.
func TestScrubAuthFromError_NilInput(t *testing.T) {
	if got := scrubAuthFromError(nil, nil); got != nil {
		t.Errorf("nil in = %v, want nil", got)
	}
}

// TestScrubAuthFromError_Idempotent pins that scrubbing an already-
// scrubbed error is a no-op (a second call returns the same string).
func TestScrubAuthFromError_Idempotent(t *testing.T) {
	auth := &BasicAuth{Username: "alice", Password: "s3cret"}
	err := errors.New("alice sent s3cret")
	first := scrubAuthFromError(err, auth).Error()
	second := scrubAuthFromError(errors.New(first), auth).Error()
	if first != second {
		t.Errorf("idempotence broken: first=%q second=%q", first, second)
	}
}

// TestRegistryClient_SatisfiesAuthPuller pins the interface claim
// at compile time.
func TestRegistryClient_SatisfiesAuthPuller(t *testing.T) {
	var _ AuthPuller = (*RegistryClient)(nil)
	var _ AuthPuller = DefaultPuller{}
	// AuthManifestPuller is the M6 two-drive seam (issue #461 / ADR-062);
	// production RegistryClient must satisfy it. DefaultPuller
	// intentionally implements only Puller (offline default), not
	// ManifestPuller / AuthManifestPuller — the auth path runs only
	// when production wires RegistryClient. fakes that DO implement
	// ManifestPuller (e.g. cmd/e2e fakes) must additionally implement
	// AuthManifestPuller for the auth path to engage.
	var _ AuthManifestPuller = (*RegistryClient)(nil)
}

// TestPullDigestWithAuth_SendsBasicAuthToRealm drives the realm
// endpoint with a fake that requires Basic Auth and asserts the
// header carries the expected `Basic <base64>` value. Mirrors
// TestFetchToken_BasicAuth but at the PullDigest layer.
func TestPullDigestWithAuth_SendsBasicAuthToRealm(t *testing.T) {
	const (
		user = "alice"
		pass = "s3cret-REGISTRY-AUTH-MARKER"
	)
	var realmSawAuth string
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		realmSawAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"token":"tok-basic"}`)
	}))
	defer tokenSrv.Close()

	regSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/token"):
			// Already handled by tokenSrv — unreachable.
		case strings.Contains(r.URL.Path, "/manifests/"):
			if r.Header.Get("Authorization") != "Bearer tok-basic" {
				w.Header().Set("Www-Authenticate",
					fmt.Sprintf(`Bearer realm=%q,service="registry"`, tokenSrv.URL))
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
			w.Header().Set("Docker-Content-Digest", "sha256:"+strings.Repeat("d", 64))
			_, _ = w.Write([]byte(`{"schemaVersion":2}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer regSrv.Close()

	host := strings.TrimPrefix(regSrv.URL, "http://")
	c := NewRegistryClient(WithEndpoint("http", host))

	auth := &BasicAuth{Username: user, Password: pass}
	got, err := c.PullDigestWithAuth(context.Background(), "example.com/org/app:main", auth)
	if err != nil {
		t.Fatalf("PullDigestWithAuth: %v", err)
	}
	if got != "sha256:"+strings.Repeat("d", 64) {
		t.Errorf("digest = %q", got)
	}
	wantBasic := "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+pass))
	if realmSawAuth != wantBasic {
		t.Errorf("realm Authorization = %q, want %q", realmSawAuth, wantBasic)
	}
}

// TestPullDigestWithAuth_AnonymousDoesNotSendBasicAuth pins that
// the WithAuth(nil) path is byte-identical to PullDigest: no
// Authorization header on the realm endpoint. Same as the legacy
// behaviour; the AddAuth seam doesn't widen the existing path.
func TestPullDigestWithAuth_AnonymousDoesNotSendBasicAuth(t *testing.T) {
	var realmSawAuth string
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		realmSawAuth = r.Header.Get("Authorization")
		_, _ = fmt.Fprintf(w, `{"token":"tok-anon"}`)
	}))
	defer tokenSrv.Close()

	regSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/manifests/") {
			if r.Header.Get("Authorization") != "Bearer tok-anon" {
				w.Header().Set("Www-Authenticate",
					fmt.Sprintf(`Bearer realm=%q`, tokenSrv.URL))
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
			w.Header().Set("Docker-Content-Digest", "sha256:"+strings.Repeat("e", 64))
			_, _ = w.Write([]byte(`{"schemaVersion":2}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer regSrv.Close()

	host := strings.TrimPrefix(regSrv.URL, "http://")
	c := NewRegistryClient(WithEndpoint("http", host))

	got, err := c.PullDigestWithAuth(context.Background(), "example.com/org/app:main", nil)
	if err != nil {
		t.Fatalf("PullDigestWithAuth: %v", err)
	}
	if got != "sha256:"+strings.Repeat("e", 64) {
		t.Errorf("digest = %q", got)
	}
	if realmSawAuth != "" {
		t.Errorf("realm Authorization = %q, want empty (anonymous path)", realmSawAuth)
	}
}

// TestPullDigestWithAuth_ErrorScrubsPassword pins the scrubber
// integration: when the realm 401s after seeing the password, the
// returned error does NOT contain the password value.
func TestPullDigestWithAuth_ErrorScrubsPassword(t *testing.T) {
	const pass = "s3cret-SCRUB-ME-NOW"
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Echo the password back in the error body. A misbehaving
		// proxy or logger might surface this in the error string.
		http.Error(w, "forbidden: Authorization: Basic "+base64.StdEncoding.EncodeToString([]byte("alice:"+pass)), http.StatusForbidden)
	}))
	defer tokenSrv.Close()

	regSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Www-Authenticate",
			fmt.Sprintf(`Bearer realm=%q`, tokenSrv.URL))
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer regSrv.Close()

	host := strings.TrimPrefix(regSrv.URL, "http://")
	c := NewRegistryClient(WithEndpoint("http", host))

	auth := &BasicAuth{Username: "alice", Password: pass}
	_, err := c.PullDigestWithAuth(context.Background(), "example.com/org/app:main", auth)
	if err == nil {
		t.Fatal("expected error from forbidden realm")
	}
	if strings.Contains(err.Error(), pass) {
		t.Errorf("error leaks password: %s", err.Error())
	}
}
