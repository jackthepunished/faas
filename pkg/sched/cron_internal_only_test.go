package sched

// cron_internal_only_test.go — ADR-119 tests for the
// cron-fired Authorization: Bearer JWT attachment path. The
// production code in loop.go::httpGatewaySynth.SynthesizeRequest
// consults the per-app public_auth_mode and attaches a minted
// JWT when the mode is 'internal_only'. These tests stand up
// a real HTTP test server (no unix socket) + an
// httpGatewaySynth wired with the mode lookup + minter, then
// assert the inbound request shape on the receiving side.

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/onebox-faas/faas/pkg/internalsvc"
	"github.com/onebox-faas/faas/pkg/state"
)

// capturedRequest is the recorded inbound request the test
// server sees. The Authorization header is the load-bearing
// assertion target — every test below scans it for the
// expected substring.
type capturedRequest struct {
	Method string
	Path   string
	Header http.Header
	Body   []byte
}

func (c capturedRequest) authHeader() string {
	return c.Header.Get("Authorization")
}

// captureServer is the test-side HTTP handler that captures
// every inbound request so tests can assert on it. Returns
// 200 OK to keep the SynthesizeRequest call path happy.
func captureServer(captured *atomic.Value) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		cp := capturedRequest{
			Method: r.Method,
			Path:   r.URL.Path,
			Header: r.Header.Clone(),
			Body:   body,
		}
		captured.Store(cp)
		w.WriteHeader(http.StatusOK)
	})
}

// newTestHTTPGatewaySynth builds a real *httpGatewaySynth
// targeting the test server, with the given mode-lookup +
// minter closures. Returns the synth + the captured-request
// sink the test server writes into.
func newTestHTTPGatewaySynth(t *testing.T, srvURL string, modeLookup PublicAuthModeLookupFunc, minter InternalSvcMintFunc) (*httpGatewaySynth, *atomic.Value) {
	t.Helper()
	captured := &atomic.Value{}
	mux := http.NewServeMux()
	mux.Handle("/v1/synthesize", captureServer(captured))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	// Build the client manually so we can dial the test
	// server (http://...) — production DialGatewaySynth uses
	// a unix-socket dialer.
	tr := &http.Transport{}
	c := &http.Client{Transport: tr, Timeout: 5 * time.Second}
	hg := &httpGatewaySynth{
		client:     c,
		basePrefix: srv.URL,
		log:        slog.New(slog.NewJSONHandler(io.Discard, nil)),
	}
	if modeLookup != nil {
		hg.WithAppPublicAuthModeLookup(modeLookup)
	}
	if minter != nil {
		hg.WithMintInternalSvcToken(minter)
	}
	return hg, captured
}

// loadCaptured reads the captured-request atomic. Returns the
// zero-value when nothing was captured yet.
func loadCaptured(c *atomic.Value) capturedRequest {
	v := c.Load()
	if v == nil {
		return capturedRequest{}
	}
	return v.(capturedRequest)
}

// mintFn builds an InternalSvcMintFunc closure over the given
// keypair + svcName. Returns a token whose signature the
// receiver can verify (the test allowlist mirrors this
// keypair).
func mintFn(t *testing.T, pub ed25519.PublicKey, priv ed25519.PrivateKey, svcName string) InternalSvcMintFunc {
	t.Helper()
	sum := sha256.Sum256(pub)
	kid := base64.RawURLEncoding.EncodeToString(sum[:16])
	return func(appID string) (string, error) {
		return internalsvc.Mint(svcName, 30*time.Second, map[string]any{"app_id": appID}, priv, kid)
	}
}

// TestSynthesizeRequest_InternalOnlyAttachesJWT covers the
// load-bearing assertion: when the app's mode is
// 'internal_only', the outbound /v1/synthesize request MUST
// carry Authorization: Bearer <JWT>. Without this attachment,
// the synth-side gate (synth.go) would 403 and the cron fire
// would fail silently.
func TestSynthesizeRequest_InternalOnlyAttachesJWT(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("genkey: %v", err)
	}
	modeLookup := func(appID string) string {
		if appID == "app-internal" {
			return "internal_only"
		}
		return "open"
	}
	hg, captured := newTestHTTPGatewaySynth(t, "http://example.invalid", modeLookup, mintFn(t, pub, priv, "schedd"))

	if err := hg.SynthesizeRequest(context.Background(), "app-internal", http.MethodPost, "/"); err != nil {
		t.Fatalf("SynthesizeRequest: %v", err)
	}
	cap := loadCaptured(captured)
	if cap.Method != http.MethodPost {
		t.Errorf("method = %s, want POST", cap.Method)
	}
	if cap.Path != "/v1/synthesize" {
		t.Errorf("path = %s, want /v1/synthesize", cap.Path)
	}
	auth := cap.authHeader()
	if !strings.HasPrefix(auth, "Bearer ") {
		t.Fatalf("missing Authorization header; got %q", auth)
	}
	// Verify the token is well-formed: parses via
	// internalsvc.Verify against the matching allowlist.
	tok := strings.TrimPrefix(auth, "Bearer ")
	_, verr := internalsvc.Verify(tok, map[string]ed25519.PublicKey{"schedd": pub})
	if verr != nil {
		t.Errorf("token failed Verify: %v", verr)
	}
}

// TestSynthesizeRequest_OpenModeNoToken covers the negative
// case: mode != 'internal_only' → no Authorization header.
// This is the default posture for the existing tests + dev
// boxes; adding a header for non-internal_only apps would be
// a security regression (token leak on every wake).
func TestSynthesizeRequest_OpenModeNoToken(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("genkey: %v", err)
	}
	modeLookup := func(appID string) string { return "open" }
	hg, captured := newTestHTTPGatewaySynth(t, "http://example.invalid", modeLookup, mintFn(t, pub, priv, "schedd"))

	if err := hg.SynthesizeRequest(context.Background(), "app-open", http.MethodPost, "/"); err != nil {
		t.Fatalf("SynthesizeRequest: %v", err)
	}
	if got := loadCaptured(captured).authHeader(); got != "" {
		t.Errorf("Authorization header on open-mode request = %q, want empty", got)
	}
}

// TestSynthesizeRequest_NilMinterIsLoud covers the operator-
// misconfig posture: app is in internal_only mode but no
// minter is wired. The current implementation logs a WARN
// + returns no error (the gate 403s on the receiving end).
// Future hardening could turn this into a hard error; for
// PR-A it's loud-but-non-fatal.
func TestSynthesizeRequest_NilMinterIsLoud(t *testing.T) {
	modeLookup := func(appID string) string { return "internal_only" }
	// minter = nil
	hg, _ := newTestHTTPGatewaySynth(t, "http://example.invalid", modeLookup, nil)

	// No assertion on log output (slog to io.Discard); the
	// contract is "doesn't panic, doesn't error". The gate
	// on the receiving side is what 403s.
	if err := hg.SynthesizeRequest(context.Background(), "app-internal", http.MethodPost, "/"); err != nil {
		t.Errorf("SynthesizeRequest errored on nil-minter; want non-fatal: %v", err)
	}
}

// TestPublicAuthModeFromStore covers the cmd-side closure
// factory. Reads from a fake store; returns "" on miss.
func TestPublicAuthModeFromStore(t *testing.T) {
	lookup := PublicAuthModeFromStore(func(_ context.Context, appID string) (state.App, error) {
		if appID == "app-internal" {
			return state.App{PublicAuthMode: "internal_only"}, nil
		}
		return state.App{}, nil // pretend "not found"
	})
	if got := lookup("app-internal"); got != "internal_only" {
		t.Errorf("mode = %q, want internal_only", got)
	}
	if got := lookup("app-other"); got != "" {
		t.Errorf("mode = %q, want \"\" (not found)", got)
	}
}

// TestConfigureInternalSvcAuth covers the cmd-side wiring
// helper. Sets a mode lookup + minter; asserts the underlying
// httpGatewaySynth retains both (round-trip via a synthetic
// request).
func TestConfigureInternalSvcAuth(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("genkey: %v", err)
	}
	var gotMode string
	modeLookup := func(appID string) string { gotMode = appID; return "open" }
	mint := mintFn(t, pub, priv, "schedd")
	gw, captured := newTestHTTPGatewaySynth(t, "http://example.invalid", nil, nil)

	if ok := ConfigureInternalSvcAuth(gw, modeLookup, mint); !ok {
		t.Errorf("ConfigureInternalSvcAuth returned false")
	}
	if err := gw.SynthesizeRequest(context.Background(), "app-x", http.MethodPost, "/"); err != nil {
		t.Fatalf("SynthesizeRequest: %v", err)
	}
	if gotMode != "app-x" {
		t.Errorf("mode lookup not consulted; got appID = %q, want \"app-x\"", gotMode)
	}
	if loadCaptured(captured).authHeader() != "" {
		t.Errorf("Authorization header attached for open mode")
	}
}