// Tests for the shared H2C-capable runner listener
// (ADR-126 / G19). The helper is the single opt-in site for
// HTTP/2 prior-knowledge across all runners; a regression here
// flips every guest back to H1, which breaks app_protocol ∈
// {http2, grpc} for the entire fleet.
//
// Tests are package-internal (`package internal`) so they can
// reach the unexported ListenAndServeLoopback test seam.

package internal

import (
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/http2"
)

// TestH2CListener_H2PriorKnowledge pins the load-bearing
// invariant for ADR-126 / G19: the listener accepts an inbound
// prior-knowledge HTTP/2 connection (no Upgrade dance, no TLS)
// and the H2 frame envelope reaches the handler. Without this,
// app_protocol ∈ {http2, grpc} is unreachable end-to-end.
func TestH2CListener_H2PriorKnowledge(t *testing.T) {
	gotReq := make(chan struct{}, 1)
	gotH2 := make(chan bool, 1)

	mux := http.NewServeMux()
	mux.HandleFunc("/echo", func(w http.ResponseWriter, r *http.Request) {
		gotReq <- struct{}{}
		gotH2 <- r.ProtoMajor == 2
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "h2 OK")
	})

	srv, ln, err := ListenAndServeLoopback(H2CListenerConfig{
		Addr:              "127.0.0.1:0",
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatalf("ListenAndServeLoopback: %v", err)
	}
	defer func() { _ = ln.Close() }()
	go func() { _ = srv.Serve(ln) }()

	// Drive an H2 prior-knowledge request via x/net/http2.Transport
	// pointed at the loopback listener.
	rt := &http2.Transport{
		AllowHTTP: true,
		DialTLSContext: func(ctx context.Context, _, _ string, _ *tls.Config) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "tcp", ln.Addr().String())
		},
		IdleConnTimeout: 1 * time.Second,
		ReadIdleTimeout: 1 * time.Second,
		PingTimeout:     100 * time.Millisecond,
	}
	t.Cleanup(func() { rt.CloseIdleConnections() })

	resp, err := rt.RoundTrip(&http.Request{
		Method: "POST",
		Header: http.Header{"Content-Type": {"text/plain"}},
		Body:   io.NopCloser(strings.NewReader("hello")),
		URL:    fmtURL(t, ln.Addr().String(), "/echo"),
	})
	if err != nil {
		t.Fatalf("H2 RoundTrip: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, _ := io.ReadAll(resp.Body)
	if string(body) != "h2 OK" {
		t.Errorf("body = %q, want %q", body, "h2 OK")
	}

	select {
	case <-gotReq:
	case <-time.After(2 * time.Second):
		t.Fatalf("handler never invoked")
	}
	select {
	case isH2 := <-gotH2:
		if !isH2 {
			t.Errorf("r.ProtoMajor = %d, want 2 (H2 prior-knowledge failed)", 1)
		}
	case <-time.After(1 * time.Second):
		t.Fatalf("h2-detection channel never signaled")
	}
}

// TestH2CListener_H1Fallback pins the backwards-compat invariant
// for app_protocol=http1 (default; today's behavior): an H1 client
// connects and the listener accepts the request — H2C is opt-in,
// not opt-out.
func TestH2CListener_H1Fallback(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/h1", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "h1 OK")
	})
	srv, ln, err := ListenAndServeLoopback(H2CListenerConfig{
		Addr:    "127.0.0.1:0",
		Handler: mux,
	})
	if err != nil {
		t.Fatalf("ListenAndServeLoopback: %v", err)
	}
	defer func() { _ = ln.Close() }()
	go func() { _ = srv.Serve(ln) }()

	// Wait briefly for srv.Serve to start accepting. The H2 prior-
	// knowledge test had the same race; it succeeded because the
	// H2 client retries on its own. The H1 client doesn't retry, so
	// we need an explicit dial-poll here.
	if !waitForListener(ln.Addr().String(), 1*time.Second) {
		t.Fatalf("listener never came up on %s", ln.Addr().String())
	}

	resp, err := http.Get(fmtURL(t, ln.Addr().String(), "/h1").String())
	if err != nil {
		t.Fatalf("H1 Get: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.ProtoMajor != 1 {
		t.Errorf("ProtoMajor = %d, want 1 (H1 fallback broken)", resp.ProtoMajor)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "h1 OK" {
		t.Errorf("body = %q, want %q", body, "h1 OK")
	}
}

// waitForListener dials the address until the connection
// succeeds or the deadline expires. Used to bridge the race
// between srv.Serve being launched in a goroutine and the
// test's first request arriving.
func waitForListener(addr string, deadline time.Duration) bool {
	end := time.Now().Add(deadline)
	for time.Now().Before(end) {
		c, err := net.DialTimeout("tcp", addr, 50*time.Millisecond)
		if err == nil {
			_ = c.Close()
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}

// TestH2CListener_DefaultPort pins the PORT-env fallback to 8080
// in DefaultH2CListenerConfig. This is the wire shape the rest
// of the platform assumes (liveness probes, customer handlers,
// the bridge's guest dial — all hit 8080 by default).
func TestH2CListener_DefaultPort(t *testing.T) {
	cfg := DefaultH2CListenerConfig(http.NewServeMux())
	if cfg.Addr != ":8080" {
		t.Errorf("DefaultH2CListenerConfig.Addr = %q, want %q", cfg.Addr, ":8080")
	}
	if cfg.ReadHeaderTimeout != 10*time.Second {
		t.Errorf("DefaultH2CListenerConfig.ReadHeaderTimeout = %v, want 10s", cfg.ReadHeaderTimeout)
	}
}

// TestH2CListener_PortEnvOverride pins the per-deployment PORT
// override (ADR-053 PR-C; guest/init stamps it on the exec'd
// env). The runner's listener must honor it.
func TestH2CListener_PortEnvOverride(t *testing.T) {
	t.Setenv("PORT", "9090")
	cfg := DefaultH2CListenerConfig(http.NewServeMux())
	if cfg.Addr != ":9090" {
		t.Errorf("DefaultH2CListenerConfig.Addr with PORT=9090 = %q, want %q", cfg.Addr, ":9090")
	}
}

// fmtURL builds an *http.URL for the loopback test. The H2
// transport requires an http URL; H1 uses it for the Host header.
func fmtURL(t *testing.T, addr, path string) *url.URL {
	t.Helper()
	u, err := url.Parse("http://" + addr + path)
	if err != nil {
		t.Fatalf("parse url: %v", err)
	}
	return u
}
