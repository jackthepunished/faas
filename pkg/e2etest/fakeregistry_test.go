// fakeregistry_test.go — pins the FakeRegistry.RequireBasicAuth
// extension (issue #461 / ADR-062). Mirrors the production seam:
//   - /v2/... without Bearer → 401 with a Bearer challenge that
//     drives pkg/oci.RegistryClient.fetchToken.
//   - /token with the wrong Basic Auth → 401.
//   - /token with the right Basic Auth → 200 + {"token": "..."}.
//   - /v2/... with that Bearer → 200 (manifest served).
//
// These tests don't boot imaged — the goal is to pin the FakeRegistry's
// behaviour so the metal e2e (which DOES boot imaged and exercises the
// full pkg/oci.RegistryClient.auth path) has a known-correct gateway to
// talk to.

package e2etest

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// TestFakeRegistry_RequireBasicAuth_PinsGate walks the public
// distribution-spec contract against the FakeRegistry gate.
func TestFakeRegistry_RequireBasicAuth_PinsGate(t *testing.T) {
	fr := NewFakeRegistry()
	defer fr.Close()
	img, _ := HelloImage("library/hello", "hello from fake")
	fr.AddImage("library/hello", img)
	fr.RequireBasicAuth("alice", "s3cret")

	// 1. Anonymous /v2/... → 401 with Bearer challenge.
	t.Run("anonymous manifest request is challenged", func(t *testing.T) {
		resp, err := http.Get(fr.URL() + "/v2/library/hello/manifests/latest")
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", resp.StatusCode)
		}
		chal := resp.Header.Get("Www-Authenticate")
		if !strings.HasPrefix(chal, "Bearer ") {
			t.Errorf("WWW-Authenticate = %q, want Bearer prefix", chal)
		}
		if !strings.Contains(chal, "/token") {
			t.Errorf("challenge missing /token realm: %q", chal)
		}
		if !strings.Contains(chal, `service="fake-registry"`) {
			t.Errorf("challenge missing fake-registry service: %q", chal)
		}
	})

	// 2. Wrong Basic Auth at /token → 401.
	t.Run("wrong creds at token endpoint rejected", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodGet, fr.URL()+"/token", nil)
		req.SetBasicAuth("alice", "WRONG")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("GET /token: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", resp.StatusCode)
		}
	})

	// 3. Right Basic Auth at /token → 200 + bearer.
	t.Run("right creds at token endpoint issue bearer", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodGet, fr.URL()+"/token", nil)
		req.SetBasicAuth("alice", "s3cret")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("GET /token: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
		var tok struct {
			Token string `json:"token"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&tok); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if tok.Token == "" {
			t.Fatalf("empty token: %+v", tok)
		}

		// 4. /v2/... with that Bearer → 200 manifest.
		mreq, _ := http.NewRequest(http.MethodGet,
			fr.URL()+"/v2/library/hello/manifests/latest", nil)
		mreq.Header.Set("Authorization", "Bearer "+tok.Token)
		mresp, err := http.DefaultClient.Do(mreq)
		if err != nil {
			t.Fatalf("GET manifest: %v", err)
		}
		defer func() { _ = mresp.Body.Close() }()
		if mresp.StatusCode != http.StatusOK {
			t.Fatalf("manifest status = %d, want 200", mresp.StatusCode)
		}
	})

	// 5. /token with no Basic Auth at all → 401 (sanity; mirrors
	// production distributions that probe the endpoint without
	// credentials first).
	t.Run("token endpoint requires basic auth header", func(t *testing.T) {
		resp, err := http.Get(fr.URL() + "/token")
		if err != nil {
			t.Fatalf("GET /token: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", resp.StatusCode)
		}
	})
}

// TestFakeRegistry_NoAuth_StaysAnonymous is the regression guard:
// when RequireBasicAuth was NOT called, /v2/... serves manifests
// anonymously (existing posture — covers the existing egress_metering,
// cpu_fairness, and quota e2es).
func TestFakeRegistry_NoAuth_StaysAnonymous(t *testing.T) {
	fr := NewFakeRegistry()
	defer fr.Close()
	img, _ := HelloImage("library/hello", "hello from fake")
	fr.AddImage("library/hello", img)

	resp, err := http.Get(fr.URL() + "/v2/library/hello/manifests/latest")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("anonymous status = %d, want 200", resp.StatusCode)
	}
}
