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
	"testing"
	"time"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
)

// TestInternalProxy_NegotiatesH2C pins the issue #675 contract:
// when useH2C is true, the proxy negotiates HTTP/2 prior knowledge
// against an h2c.NewHandler-wrapped backend. Asserts the response
// protocol observed by the client side is HTTP/2.0 (r.Proto == "HTTP/2.0"
// on the inbound server-side request, resp.ProtoMajor == 2 on the
// outbound client response).
func TestInternalProxy_NegotiatesH2C(t *testing.T) {
	var (
		mu        sync.Mutex
		gotProto  string
		gotMethod string
		gotPath   string
	)
	backend := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotProto = r.Proto
		gotMethod = r.Method
		gotPath = r.URL.Path
		mu.Unlock()
		w.Header().Set("X-Backend", "h2c")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok")
	})
	// h2c.NewHandler wraps the backend with HTTP/2 cleartext prior
	// knowledge negotiation. Listen on a TCP loopback port so the
	// client transport can dial it.
	srv := &http.Server{Handler: h2c.NewHandler(backend, &http2.Server{})}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	go func() { _ = srv.Serve(ln) }()
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	}()

	// Build an InternalReverseProxy whose dialer returns a real
	// net.Conn pointing at the loopback listener. http2.Transport
	// calls DialTLS synchronously during RoundTrip, so we don't need
	// a background goroutine to accept — the call is on the hot path.
	dialer := &loopbackDialer{addr: ln.Addr().String()}
	target := &url.URL{Scheme: "http", Host: "internal"}
	proxy := NewInternalReverseProxy(dialer, target, slog.Default(), true)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/synthesize", nil)
	proxy.ServeHTTP(rec, req)

	resp := rec.Result()
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: %d (body=%q)", resp.StatusCode, rec.Body.String())
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "ok") {
		t.Fatalf("body: %q", string(body))
	}
	if resp.Header.Get("X-Backend") != "h2c" {
		t.Fatalf("missing X-Backend header — proxy did not reach backend through H2C transport")
	}
	mu.Lock()
	defer mu.Unlock()
	if gotProto != "HTTP/2.0" {
		t.Fatalf("backend saw r.Proto=%q; want \"HTTP/2.0\"", gotProto)
	}
	if gotMethod != http.MethodGet {
		t.Fatalf("backend saw method=%q; want GET", gotMethod)
	}
	if gotPath != "/v1/synthesize" {
		t.Fatalf("backend saw path=%q; want /v1/synthesize", gotPath)
	}
}

// TestInternalProxy_HTTP11Fallback asserts that when useH2C is false,
// the proxy uses the legacy HTTP/1.1 transport against an H1-only
// backend. Acts as a regression guard for the FAAS_INTERNAL_H2C=false
// rollback path.
func TestInternalProxy_HTTP11Fallback(t *testing.T) {
	backend := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Refuse H2 by writing a non-h2 response — the legacy
		// transport must negotiate H1 and succeed.
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "h1")
	})
	srv := httptest.NewServer(backend)
	defer srv.Close()

	dialer := &loopbackDialer{addr: strings.TrimPrefix(srv.URL, "http://")}
	target := &url.URL{Scheme: "http", Host: "internal"}
	proxy := NewInternalReverseProxy(dialer, target, slog.Default(), false)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/synthesize", nil)
	proxy.ServeHTTP(rec, req)

	resp := rec.Result()
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "h1") {
		t.Fatalf("body: %q", string(body))
	}
}

// loopbackDialer dials a TCP loopback address using the stdlib
// dialer. Used by the H2C and H1-fallback tests.
type loopbackDialer struct{ addr string }

func (d *loopbackDialer) DialContext(ctx context.Context, _ string) (net.Conn, error) {
	var nd net.Dialer
	return nd.DialContext(ctx, "tcp", d.addr)
}
