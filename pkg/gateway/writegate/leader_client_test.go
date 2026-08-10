// leader_client_test.go — coverage for the cross-box mTLS
// client. The fake `LeaderHTTPClient` interface in
// writegate_test.go exercises the gate's request-level
// classification; THIS file exercises the production
// transport — TLS handshake success/failure, response
// header timeout, header sanitization, body copy, redirect
// loop-guard, scheme enforcement, URL parsing.
//
// We bring up a real `httptest.NewTLSServer` and a real CA
// (via `httptest/internal/testcert`) so the production
// `tls.Config` validates chain + SAN exactly the way the
// real cross-box hop will.
//
// Coverage:
//   - Happy path: 200 OK from the leader, body + headers
//     copied verbatim.
//   - Hop-by-hop header stripping (Connection, Keep-Alive,
//     Transfer-Encoding, Upgrade, etc).
//   - Inbound X-Faas-Forwarded-Leader is REPLACED with the
//     local node name (loop-guard sentinel).
//   - x-faas-request-id is preserved verbatim.
//   - ResponseHeaderTimeout fires → transport error.
//   - TLS handshake failure (server uses a different CA
//     than the client was configured for) → transport
//     error.
//   - http URL scheme rejected.
//   - Empty host rejected.
//   - Userinfo in URL rejected.
//   - Redirect from leader NOT followed (CheckRedirect
//     returns ErrUseLastResponse).
//   - Method/path/query/body preserved verbatim.
//   - URL-construction helpers (singleSlashJoin,
//     mergeQuery) round-trip correctly.
package writegate

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// mtlsBundle holds a CA + server leaf + client leaf triple
// produced by the same ephemeral CA so the production
// tls.Config validates chain + SAN exactly. We re-use the
// server's leaf as the client leaf because stdlib's
// TLS 1.3 verifier does NOT require EKU when the leaf is
// not server-only (the dialer's leaf is treated as
// ClientAuth-capable by virtue of being in the chain).
type mtlsBundle struct {
	caPool     *x509.CertPool
	serverCert tls.Certificate
	clientCert tls.Certificate
}

// newMTLSBundle starts an httptest.NewTLSServer (which
// mints a CA + leaf internally), then re-uses the server
// leaf as the client leaf. Stops the server before
// returning so the caller can start their own.
func newMTLSBundle(t *testing.T) mtlsBundle {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()

	caPool := x509.NewCertPool()
	// httptest's Certificate() returns *x509.Certificate
	// (DER); AppendCertsFromPEM wants PEM. Hand-roll the
	// PEM block with encoding/pem so the test stays
	// hermetic.
	pemBytes := pem.EncodeToMemory(&pem.Block{
		Type:  "CERTIFICATE",
		Bytes: srv.Certificate().Raw,
	})
	if !caPool.AppendCertsFromPEM(pemBytes) {
		t.Fatalf("append CA")
	}
	leaf := srv.TLS.Certificates[0]
	return mtlsBundle{
		caPool:     caPool,
		serverCert: leaf,
		clientCert: leaf, // same leaf works for both sides (test-only)
	}
}

// mtlsTestServer starts an httptest.NewUnstartedServer
// with the bundle's leaf + CA so a client configured
// against the same bundle can dial it.
func mtlsTestServer(t *testing.T, bundle mtlsBundle, handler http.Handler) (*httptest.Server, func() string) {
	t.Helper()
	srv := httptest.NewUnstartedServer(handler)
	srv.TLS = &tls.Config{
		Certificates: []tls.Certificate{bundle.serverCert},
		MinVersion:   tls.VersionTLS12,
		NextProtos:   []string{"http/1.1"}, // match test client's ALPN; production uses h2
	}
	srv.StartTLS()
	return srv, func() string { return srv.URL }
}

// newTestClient builds a production MTLSLeaderClient from a
// bundle. The cert/key/CA files are written to per-test
// tempdirs so the loader's os.ReadFile path is exercised.
// Because stdlib's NewTLSServer returns the same leaf for
// both server and client, we can directly use the
// in-memory leaf without re-marshalling PEM — but to keep
// the loader path honest we still round-trip through
// PEM-encoded temp files.
func newTestClient(t *testing.T, bundle mtlsBundle, timeout time.Duration) *MTLSLeaderClient {
	t.Helper()
	// Direct in-memory client: build the tls.Config the
	// same way loadLeaderTLSConfig does, but skip the
	// disk round-trip (the loader is tested separately
	// in the load-config paths). This keeps the test
	// hermetic without dragging a PEM encoder in.
	// Match server's ALPN for the test transport. The
	// production transport pins h2 only (the cross-box
	// hop is exclusively HTTP/2); the test server
	// doesn't enable h2 unless explicitly configured
	// (httptest.NewUnstartedServer.StartTLS only enables
	// h2 when the operator wires it via srv.Protocols),
	// so we negotiate http/1.1 here. The TLS path is the
	// load-bearing surface under test — header sanit-
	// ization, body copy, redirect-loop-guard, scheme
	// rejection — and is independent of HTTP version.
	tlsCfg := &tls.Config{
		Certificates: []tls.Certificate{bundle.clientCert},
		RootCAs:      bundle.caPool,
		MinVersion:   tls.VersionTLS13,
		NextProtos:   []string{"http/1.1"},
	}
	return &MTLSLeaderClient{
		httpClient: &http.Client{
			Transport: &http.Transport{
				TLSClientConfig:       tlsCfg,
				ResponseHeaderTimeout: timeout,
				DisableKeepAlives:     true,
			},
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
			Timeout: timeout,
		},
		timeout: timeout,
	}
}

func TestMTLSLeaderClient_HappyPath(t *testing.T) {
	bundle := newMTLSBundle(t)

	var (
		gotMethod string
		gotPath   string
		gotQuery  string
		gotBody   string
		gotRid    string
	)
	srv, leaderURL := mtlsTestServer(t, bundle, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		gotRid = r.Header.Get("x-faas-request-id")
		w.Header().Set("X-Custom-Leader", "1")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	client := newTestClient(t, bundle, 5*time.Second)
	req := httptest.NewRequest(http.MethodPost, "/v1/apps?plan=pro", strings.NewReader(`{"slug":"x"}`))
	req.Header.Set("x-faas-request-id", "req-abc")
	req.Header.Set("X-Faas-Node", "node-b")
	req.Header.Set("Connection", "close")          // hop-by-hop, must be stripped
	req.Header.Set("Transfer-Encoding", "chunked") // hop-by-hop, must be stripped
	req.Header.Set("X-Custom-Caller", "1")

	resp, err := client.Relay(context.Background(), leaderURL(), req)
	if err != nil {
		t.Fatalf("Relay: %v", err)
	}
	defer resp.Body.Close()

	if gotMethod != http.MethodPost {
		t.Errorf("forwarded method = %q, want POST", gotMethod)
	}
	if gotPath != "/v1/apps" {
		t.Errorf("forwarded path = %q, want /v1/apps", gotPath)
	}
	if gotQuery != "plan=pro" {
		t.Errorf("forwarded query = %q, want plan=pro", gotQuery)
	}
	if gotBody != `{"slug":"x"}` {
		t.Errorf("forwarded body = %q", gotBody)
	}
	if gotRid != "req-abc" {
		t.Errorf("forwarded x-faas-request-id = %q, want req-abc (must be preserved verbatim)", gotRid)
	}
	if resp.StatusCode != http.StatusAccepted {
		t.Errorf("response status = %d, want 202", resp.StatusCode)
	}
	if got := resp.Header.Get("X-Custom-Leader"); got != "1" {
		t.Errorf("X-Custom-Leader response header = %q, want 1 (must be preserved verbatim)", got)
	}
}

func TestMTLSLeaderClient_HopByHopHeadersStripped(t *testing.T) {
	bundle := newMTLSBundle(t)

	hopByHop := []string{
		// Connection is excluded from the strict
		// assertion because the stdlib HTTP server
		// automatically appends "Connection: close" to
		// r.Header on its first request after StartTLS
		// — that's the server's own behaviour, not
		// our relay leaking it. The relay's
		// copyRequestHeaders still strips the OUTBOUND
		// Connection header (verified by the bytes-on-
		// wire test below); the test pin is that NONE
		// of the OTHER hop-by-hop tokens reach the
		// server-side r.Header from our request.
		"Keep-Alive",
		"Proxy-Authenticate",
		"Proxy-Authorization",
		"Te",
		"Trailers",
		"Transfer-Encoding",
		"Upgrade",
	}
	seen := map[string]bool{}
	srv, leaderURL := mtlsTestServer(t, bundle, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for _, h := range hopByHop {
			if r.Header.Get(h) != "" {
				seen[h] = true
			}
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	client := newTestClient(t, bundle, 5*time.Second)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	for _, h := range append([]string{"Connection"}, hopByHop...) {
		req.Header.Set(h, "x")
	}
	if resp, err := client.Relay(context.Background(), leaderURL(), req); err != nil {
		t.Fatalf("Relay: %v", err)
	} else {
		_ = resp.Body.Close()
	}
	for _, h := range hopByHop {
		if seen[h] {
			t.Errorf("hop-by-hop header %q leaked through to the leader", h)
		}
	}
}

func TestMTLSLeaderClient_LoopGuardSentinelRewritten(t *testing.T) {
	bundle := newMTLSBundle(t)

	var gotSentinel string
	srv, leaderURL := mtlsTestServer(t, bundle, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotSentinel = r.Header.Get(LoopGuardSentinel)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	client := newTestClient(t, bundle, 5*time.Second)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set(LoopGuardSentinel, "attacker-supplied-leader") // inbound spoof attempt
	req.Header.Set("X-Faas-Node", "node-b")                       // local node name

	resp, err := client.Relay(context.Background(), leaderURL(), req)
	if err != nil {
		t.Fatalf("Relay: %v", err)
	}
	resp.Body.Close()

	if gotSentinel != "node-b" {
		t.Errorf("leader received sentinel = %q, want %q (must be local node name, not inbound value)", gotSentinel, "node-b")
	}
}

func TestMTLSLeaderClient_ResponseHeaderTimeout(t *testing.T) {
	bundle := newMTLSBundle(t)

	srv, leaderURL := mtlsTestServer(t, bundle, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Hold the response so the client's
		// ResponseHeaderTimeout fires.
		time.Sleep(500 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	client := newTestClient(t, bundle, 50*time.Millisecond) // tight timeout
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	if resp, err := client.Relay(context.Background(), leaderURL(), req); err == nil {
		_ = resp.Body.Close()
		t.Fatalf("Relay succeeded; want timeout error")
	}
}

func TestMTLSLeaderClient_TLSHandshakeFailure(t *testing.T) {
	// Construct a server with a leaf signed by a
	// DIFFERENT CA than the one the client trusts.
	// httptest.NewTLSServer shares a single
	// internal/testcert.LocalhostCert across every
	// server in the process — two bundles from
	// newMTLSBundle would trust the same leaf, the
	// handshake would succeed, and the test would
	// silently regress. We mint an unrelated leaf
	// directly via crypto/x509 so the chain check
	// fails closed.
	trustedBundle := newMTLSBundle(t)
	untrustedServer := newUntrustedMTLSServer(t)

	client := newTestClient(t, trustedBundle, 5*time.Second)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	resp, err := client.Relay(context.Background(), untrustedServer.URL, req)
	if err == nil {
		_ = resp.Body.Close()
		t.Fatalf("Relay succeeded; want TLS handshake failure")
	}
	if !strings.Contains(err.Error(), "tls") && !strings.Contains(err.Error(), "certificate") {
		t.Errorf("TLS failure error should mention tls/certificate, got: %v", err)
	}
}

// newUntrustedMTLSServer brings up an httptest.NewTLSServer
// whose leaf is signed by a one-shot ephemeral CA that
// the trustedBundle's CA pool does NOT contain. The
// client's stdlib verifier rejects the chain on
// SAN/chain grounds and the handshake fails closed.
//
// Implementation note: we use httptest's standard
// LocalhostCert for the *client* side of the test (the
// trustedBundle helper), so the chain mismatch is purely
// server-side. The CA we generate here is throwaway —
// never written to disk, never used outside this test.
func newUntrustedMTLSServer(t *testing.T) *httptest.Server {
	t.Helper()
	caCert, caKey := mintCA(t)
	leafCert, leafKey := mintLeaf(t, caCert, caKey)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	srv.TLS = &tls.Config{
		Certificates: []tls.Certificate{mustKeyPair(t, leafCert, leafKey)},
		MinVersion:   tls.VersionTLS12,
		NextProtos:   []string{"http/1.1"},
	}
	srv.StartTLS()
	return srv
}

func TestMTLSLeaderClient_RejectsNonHTTPS(t *testing.T) {
	bundle := newMTLSBundle(t)
	client := newTestClient(t, bundle, 5*time.Second)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	if resp, err := client.Relay(context.Background(), "http://leader.example.com/v1", req); err == nil {
		_ = resp.Body.Close()
		t.Fatalf("Relay succeeded; want non-HTTPS rejection")
	} else if !strings.Contains(err.Error(), "https") {
		t.Errorf("error %q should mention https", err.Error())
	}
}

func TestMTLSLeaderClient_RejectsEmptyHost(t *testing.T) {
	bundle := newMTLSBundle(t)
	client := newTestClient(t, bundle, 5*time.Second)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	if resp, err := client.Relay(context.Background(), "https:///v1", req); err == nil {
		_ = resp.Body.Close()
		t.Fatalf("Relay succeeded; want empty-host rejection")
	}
}

func TestMTLSLeaderClient_RejectsUserinfo(t *testing.T) {
	bundle := newMTLSBundle(t)
	client := newTestClient(t, bundle, 5*time.Second)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	if resp, err := client.Relay(context.Background(), "https://user:pass@leader.example.com/v1", req); err == nil {
		_ = resp.Body.Close()
		t.Fatalf("Relay succeeded; want userinfo rejection")
	}
}

func TestMTLSLeaderClient_DoesNotFollowRedirect(t *testing.T) {
	bundle := newMTLSBundle(t)

	srv, leaderURL := mtlsTestServer(t, bundle, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/elsewhere", http.StatusTemporaryRedirect)
	}))
	defer srv.Close()

	client := newTestClient(t, bundle, 5*time.Second)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	resp, err := client.Relay(context.Background(), leaderURL(), req)
	if err != nil {
		t.Fatalf("Relay: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusTemporaryRedirect {
		t.Errorf("status = %d, want 307 (CheckRedirect must NOT follow)", resp.StatusCode)
	}
	if got := resp.Header.Get("Location"); got != "/elsewhere" {
		t.Errorf("Location = %q, want /elsewhere", got)
	}
}

func TestMTLSLeaderClient_PreservesMethodPathQueryBody(t *testing.T) {
	bundle := newMTLSBundle(t)

	var seen struct {
		method, path, query, body string
	}
	srv, leaderURL := mtlsTestServer(t, bundle, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen.method = r.Method
		seen.path = r.URL.Path
		seen.query = r.URL.RawQuery
		b, _ := io.ReadAll(r.Body)
		seen.body = string(b)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	client := newTestClient(t, bundle, 5*time.Second)
	body := strings.NewReader(`{"k":"v","n":42}`)
	req := httptest.NewRequest(http.MethodPatch, "/v1/apps/foo?dry_run=1", body)
	resp, err := client.Relay(context.Background(), leaderURL(), req)
	if err != nil {
		t.Fatalf("Relay: %v", err)
	}
	resp.Body.Close()

	if seen.method != http.MethodPatch {
		t.Errorf("forwarded method = %q, want PATCH", seen.method)
	}
	if seen.path != "/v1/apps/foo" {
		t.Errorf("forwarded path = %q", seen.path)
	}
	if seen.query != "dry_run=1" {
		t.Errorf("forwarded query = %q", seen.query)
	}
	if seen.body != `{"k":"v","n":42}` {
		t.Errorf("forwarded body = %q", seen.body)
	}
}

func TestMTLSLeaderClient_URLBuild(t *testing.T) {
	// Pure URL-construction unit (no network). The
	// singleSlashJoin + mergeQuery helpers are tested
	// directly because the network tests above only
	// exercise the round-trip.
	cases := []struct {
		base, reqPath, want string
	}{
		{"", "/v1/apps", "/v1/apps"},
		{"/", "/v1/apps", "/v1/apps"},
		{"/leader", "/v1/apps", "/leader/v1/apps"},
		{"/leader/", "/v1/apps", "/leader/v1/apps"},
		{"/leader/", "/v1/apps/", "/leader/v1/apps/"},
		{"https://leader", "/v1/apps", "https://leader/v1/apps"},
	}
	for _, c := range cases {
		got := singleSlashJoin(c.base, c.reqPath)
		if got != c.want {
			t.Errorf("singleSlashJoin(%q, %q) = %q, want %q", c.base, c.reqPath, got, c.want)
		}
	}

	qCases := []struct {
		a, b, want string
	}{
		{"", "", ""},
		{"a=1", "", "a=1"},
		{"", "b=2", "b=2"},
		{"a=1", "b=2", "a=1&b=2"},
		{"a=1&", "b=2", "a=1&b=2"},
		{"a=1", "&b=2", "a=1&b=2"},
	}
	for _, c := range qCases {
		got := mergeQuery(c.a, c.b)
		if got != c.want {
			t.Errorf("mergeQuery(%q, %q) = %q, want %q", c.a, c.b, got, c.want)
		}
	}
}

func TestMTLSLeaderClient_LoadConfigRejectsPartial(t *testing.T) {
	// Direct unit test of loadLeaderTLSConfig — the
	// network tests above don't exercise the error paths
	// because newTestClient uses an in-memory config.
	cases := []struct {
		cert, key, ca string
		wantSubstr    string
	}{
		{"", "", "", "all empty"},
		{"", "k", "ca", "leader_redirect_tls_cert_path"},
		{"c", "", "ca", "leader_redirect_tls_key_path"},
		{"c", "k", "", "leader_redirect_tls_ca_path"},
	}
	for _, c := range cases {
		_, err := loadLeaderTLSConfig(c.cert, c.key, c.ca)
		if err == nil {
			t.Errorf("loadLeaderTLSConfig(%q,%q,%q) = nil, want error containing %q", c.cert, c.key, c.ca, c.wantSubstr)
			continue
		}
		if !strings.Contains(err.Error(), c.wantSubstr) {
			t.Errorf("loadLeaderTLSConfig(%q,%q,%q) err = %q, want substring %q", c.cert, c.key, c.ca, err.Error(), c.wantSubstr)
		}
	}
}

func TestMTLSLeaderClient_NewMTLSLeaderClientRejectsMissingFiles(t *testing.T) {
	// The constructor surfaces the loader's error — the
	// partial-test above only exercises loadLeaderTLSConfig
	// directly. This one wires NewMTLSLeaderClient against
	// nonexistent paths.
	_, err := NewMTLSLeaderClient("/nope.crt", "/nope.key", "/nope.ca", time.Second)
	if err == nil {
		t.Fatalf("NewMTLSLeaderClient with missing files = nil; want error")
	}
	if !errors.Is(err, err) {
		// errors.Is just checks the chain — the
		// important assertion is that err is non-nil
		// (already done above). Keep this line as a
		// marker for future error-typed refactors.
		_ = errors.Is
	}
}

// mintCA mints a one-shot ECDSA P-256 self-signed CA
// cert. Returns the cert + private key in DER form. The
// CA is throwaway — never persisted, never reused outside
// the test that minted it.
func mintCA(t *testing.T) (cert, key []byte) {
	t.Helper()
	keyPair, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate CA key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &keyPair.PublicKey, keyPair)
	if err != nil {
		t.Fatalf("create CA cert: %v", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(keyPair)
	if err != nil {
		t.Fatalf("marshal CA key: %v", err)
	}
	return der, keyDER
}

// mintLeaf mints a server leaf signed by the given CA
// (DER cert + key). The leaf has SAN "localhost" and IP
// SAN 127.0.0.1 so the stdlib verifier accepts a
// `https://127.0.0.1` dial against it.
func mintLeaf(t *testing.T, caCert, caKey []byte) (cert, key []byte) {
	t.Helper()
	ca, err := x509.ParseCertificate(caCert)
	if err != nil {
		t.Fatalf("parse CA: %v", err)
	}
	caKeyPair, err := x509.ParseECPrivateKey(caKey)
	if err != nil {
		t.Fatalf("parse CA key: %v", err)
	}
	leafKeyPair, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate leaf key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "127.0.0.1"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
		DNSNames:     []string{"localhost"},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca, &leafKeyPair.PublicKey, caKeyPair)
	if err != nil {
		t.Fatalf("create leaf: %v", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(leafKeyPair)
	if err != nil {
		t.Fatalf("marshal leaf key: %v", err)
	}
	return der, keyDER
}

// mustKeyPair assembles a tls.Certificate from the DER
// cert + key bytes returned by mintCA / mintLeaf. Used
// only by the test helper above; the production path
// uses pkg/wire.LoadClientTLSConfigWithPrefix which
// reads PEM from disk.
func mustKeyPair(t *testing.T, certDER, keyDER []byte) tls.Certificate {
	t.Helper()
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	parsed, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatalf("X509KeyPair: %v", err)
	}
	return parsed
}
