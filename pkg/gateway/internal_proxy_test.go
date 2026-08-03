package gateway

import (
	"context"
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
	// Dial the test server's listener directly (loopback TCP — the
	// unix-socket shape is exercised by NewUnixSocketDialer's own
	// test below).
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
}

// TestInternalReverseProxy_AppendsXForwardedFor verifies XFF is
// appended (not replaced) so the chain survives multi-hop.
func TestInternalReverseProxy_AppendsXForwardedFor(t *testing.T) {
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
	// Pre-existing XFF — must be appended-to, not replaced.
	req.Header.Set("X-Forwarded-For", "192.0.2.1")
	rr := httptest.NewRecorder()
	p.ServeHTTP(rr, req)
	if len(got) != 2 {
		t.Fatalf("upstream got %d X-Forwarded-For values, want 2: %v", len(got), got)
	}
	// Order: pre-existing first, then the appended (per RFC 7230
	// convention).
	if got[0] != "192.0.2.1" || got[1] != "10.0.0.5" {
		t.Errorf("XFF order = %v, want [192.0.2.1, 10.0.0.5]", got)
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
	if !strings.Contains(rr.Body.String(), "internal dial failed") {
		t.Errorf("body = %q, want substring \"internal dial failed\"", rr.Body.String())
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
	// Path that does not exist — the dial will block on connect
	// (no listener at the path). Cancellling the ctx must abort the
	// dial quickly.
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
