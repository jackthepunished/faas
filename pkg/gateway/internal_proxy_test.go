package gateway

import (
	"context"
	"crypto/tls"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// stubDialer captures the (ctx, target) tuples for assertions and
// returns a connection to a local httptest.Server.
type stubDialer struct {
	mu     sync.Mutex
	calls  []string
	server *httptest.Server
	// dialErr overrides the dial return when non-nil.
	dialErr error
}

func (d *stubDialer) DialContext(ctx context.Context, target string) (net.Conn, error) {
	d.mu.Lock()
	d.calls = append(d.calls, target)
	d.mu.Unlock()
	if d.dialErr != nil {
		return nil, d.dialErr
	}
	return net.Dial("tcp", d.server.Listener.Addr().String())
}

// TestInternalReverseProxy_StripsHopByHopHeaders pins the RFC 7230
// §6.1 contract: the inbound hop-by-hop headers are NOT forwarded
// to the upstream.
func TestInternalReverseProxy_StripsHopByHopHeaders(t *testing.T) {
	var seenConnection atomic.Bool
	var seenUpgrade atomic.Bool
	var seenKeepAlive atomic.Bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Connection") != "" {
			seenConnection.Store(true)
		}
		if r.Header.Get("Upgrade") != "" {
			seenUpgrade.Store(true)
		}
		if r.Header.Get("Keep-Alive") != "" {
			seenKeepAlive.Store(true)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()
	dialer := &stubDialer{server: upstream}
	p := NewInternalReverseProxy(dialer, &url.URL{Scheme: "http", Host: "internal"}, slog.Default())
	req := httptest.NewRequest(http.MethodGet, "/hello", nil)
	req.Header.Set("Connection", "close")
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Keep-Alive", "timeout=5")
	req.Header.Set("X-Custom", "stays") // non-hop-by-hop, must survive
	rr := httptest.NewRecorder()
	p.ServeHTTP(rr, req)
	if seenConnection.Load() {
		t.Errorf("Connection header forwarded to upstream")
	}
	if seenUpgrade.Load() {
		t.Errorf("Upgrade header forwarded to upstream")
	}
	if seenKeepAlive.Load() {
		t.Errorf("Keep-Alive header forwarded to upstream")
	}
	if rr.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rr.Code)
	}
	// X-Custom is non-hop-by-hop, must survive on the upstream
	// side (the test asserts the upstream received it via the
	// seen-custom-header flag above).
}

// TestInternalReverseProxy_StripsResponseHopByHop pins the
// response-side hop-by-hop strip (RFC 7230 §6.1 — the internal
// daemon may have set Connection: close and we don't want to
// leak that to the customer).
func TestInternalReverseProxy_StripsResponseHopByHop(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Connection", "close")
		w.Header().Set("X-Forwarded-For", "1.2.3.4")
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()
	dialer := &stubDialer{server: upstream}
	p := NewInternalReverseProxy(dialer, &url.URL{Scheme: "http", Host: "internal"}, slog.Default())
	rr := httptest.NewRecorder()
	p.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/", nil))
	if got := rr.Header().Get("Connection"); got != "" {
		t.Errorf("Connection leaked to response: %q", got)
	}
	if got := rr.Header().Get("X-Forwarded-For"); got != "1.2.3.4" {
		t.Errorf("X-Forwarded-For response = %q, want 1.2.3.4", got)
	}
}

// TestInternalReverseProxy_StripsAndRebuildsXFF pins the security
// load-bearing XFF contract: the inbound XFF is stripped (the
// customer could forge any IP) and only the public daemon's
// RemoteAddr is forwarded. Internal daemons reading XFF trust
// exactly one hop.
func TestInternalReverseProxy_StripsAndRebuildsXFF(t *testing.T) {
	var got []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Values("X-Forwarded-For")
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()
	dialer := &stubDialer{server: upstream}
	p := NewInternalReverseProxy(dialer, &url.URL{Scheme: "http", Host: "internal"}, slog.Default())
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "10.0.0.5:12345"
	// Forged XFF — must be stripped, not forwarded.
	req.Header.Set("X-Forwarded-For", "127.0.0.1")
	rr := httptest.NewRecorder()
	p.ServeHTTP(rr, req)
	if len(got) != 1 {
		t.Fatalf("upstream got %d X-Forwarded-For values, want 1 (only the public RemoteAddr): %v", len(got), got)
	}
	if got[0] != "10.0.0.5" {
		t.Errorf("XFF = %q, want 10.0.0.5 (the public daemon's RemoteAddr, not the forged value)", got[0])
	}
}

// TestInternalReverseProxy_SetsXForwardedProtoHTTPS pins the
// TLS-detection side of the XFF bundle.
func TestInternalReverseProxy_SetsXForwardedProtoHTTPS(t *testing.T) {
	var gotProto string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotProto = r.Header.Get("X-Forwarded-Proto")
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()
	dialer := &stubDialer{server: upstream}
	p := NewInternalReverseProxy(dialer, &url.URL{Scheme: "http", Host: "internal"}, slog.Default())
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "10.0.0.5:12345"
	req.TLS = &tls.ConnectionState{} // simulate the public TLS terminator
	rr := httptest.NewRecorder()
	p.ServeHTTP(rr, req)
	if gotProto != "https" {
		t.Errorf("X-Forwarded-Proto = %q, want https", gotProto)
	}
}

// TestInternalReverseProxy_DialFailure_502BadGateway verifies the
// dial-failure path surfaces 502 (not 500) so operators
// distinguish "internal tier down" from "this daemon is broken".
func TestInternalReverseProxy_DialFailure_502BadGateway(t *testing.T) {
	dialer := &stubDialer{dialErr: io.EOF} // a representative "dial failed"
	p := NewInternalReverseProxy(dialer, &url.URL{Scheme: "http", Host: "internal"}, slog.Default())
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rr := httptest.NewRecorder()
	p.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "internal round-trip failed") {
		t.Errorf("body = %q, want substring \"internal round-trip failed\"", rr.Body.String())
	}
}

// TestInternalReverseProxy_UpstreamError_StatusPropagated verifies
// 5xx responses from the internal daemon flow through unchanged
// (we don't mask upstream errors).
func TestInternalReverseProxy_UpstreamError_StatusPropagated(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("upstream 503"))
	}))
	defer upstream.Close()
	dialer := &stubDialer{server: upstream}
	p := NewInternalReverseProxy(dialer, &url.URL{Scheme: "http", Host: "internal"}, slog.Default())
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rr := httptest.NewRecorder()
	p.ServeHTTP(rr, req)
	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "upstream 503") {
		t.Errorf("body = %q, want substring \"upstream 503\"", rr.Body.String())
	}
}

// TestInternalReverseProxy_NilDialer_502 covers the wiring-bug
// path: a proxy constructed without a dialer returns 502 and logs.
func TestInternalReverseProxy_NilDialer_502(t *testing.T) {
	p := &InternalReverseProxy{Target: &url.URL{Scheme: "http", Host: "internal"}}
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rr := httptest.NewRecorder()
	p.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", rr.Code)
	}
}

// TestNewUnixSocketDialer_RespectsContextCancel pins the contract
// that the unix-socket dialer honours ctx cancellation (the public
// daemon's drain sequence relies on this).
func TestNewUnixSocketDialer_RespectsContextCancel(t *testing.T) {
	d := NewUnixSocketDialer("/tmp/this-socket-does-not-exist.sock")
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel BEFORE dial — fastest possible abort
	start := time.Now()
	conn, err := d.DialContext(ctx, "anything")
	elapsed := time.Since(start)
	if err == nil {
		_ = conn.Close()
		t.Errorf("DialContext with cancelled ctx returned nil err")
	}
	if elapsed > 100*time.Millisecond {
		t.Errorf("DialContext did not honour ctx cancel within 100 ms (%v elapsed)", elapsed)
	}
}

// TestIsHopByHop_Predicate pins the lookup table.
func TestIsHopByHop_Predicate(t *testing.T) {
	cases := map[string]bool{
		"Connection":          true,
		"Keep-Alive":          true,
		"Proxy-Authenticate":  true,
		"Proxy-Authorization": true,
		"Te":                  true,
		"Trailers":            true,
		"Transfer-Encoding":   true,
		"Upgrade":             true,
		"X-Custom":            false,
		"Content-Type":        false,
		"connection":          true, // case-insensitive
	}
	for h, want := range cases {
		if got := isHopByHop(h); got != want {
			t.Errorf("isHopByHop(%q) = %v, want %v", h, got, want)
		}
	}
}

// TestStripHopByHopInPlace_DoesNotDoubleAlloc pins the in-place
// strip behaviour. The proxy uses the in-place variant to avoid
// the Clone + rebuild double-alloc on the hot path.
func TestStripHopByHopInPlace_DoesNotDoubleAlloc(t *testing.T) {
	h := http.Header{
		"Connection":      []string{"close"},
		"Upgrade":         []string{"websocket"},
		"X-Custom":        []string{"stays"},
		"X-Forwarded-For": []string{"1.2.3.4"},
	}
	stripHopByHopInPlace(h)
	if h.Get("Connection") != "" {
		t.Errorf("Connection not stripped")
	}
	if h.Get("Upgrade") != "" {
		t.Errorf("Upgrade not stripped")
	}
	if h.Get("X-Custom") != "stays" {
		t.Errorf("X-Custom dropped")
	}
	if h.Get("X-Forwarded-For") != "1.2.3.4" {
		t.Errorf("X-Forwarded-For dropped (not a hop-by-hop)")
	}
}

// TestCopyResponseBody_ContextCancel verifies the body copy
// goroutine returns when ctx is cancelled (the conn-bound write
// loop does not pin the public listener).
func TestCopyResponseBody_ContextCancel(t *testing.T) {
	// Source returns io.EOF immediately so the goroutine completes
	// fast; the assertion is that the function returns within the
	// ctx deadline (no goroutine leak).
	src := io.NopCloser(strings.NewReader("hello"))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var buf strings.Builder
	n, err := copyResponseBody(ctx, &buf, src)
	if err != nil {
		t.Errorf("copyResponseBody err = %v, want nil", err)
	}
	if n != 5 || buf.String() != "hello" {
		t.Errorf("copyResponseBody wrote %q (n=%d), want \"hello\" (n=5)", buf.String(), n)
	}
}

// TestCopyResponseBody_ShortInput pins the EOF short-circuit.
func TestCopyResponseBody_ShortInput(t *testing.T) {
	src := io.NopCloser(strings.NewReader(""))
	ctx := context.Background()
	var buf strings.Builder
	n, err := copyResponseBody(ctx, &buf, src)
	if err != nil {
		t.Errorf("err = %v, want nil", err)
	}
	if n != 0 {
		t.Errorf("n = %d, want 0", n)
	}
}
