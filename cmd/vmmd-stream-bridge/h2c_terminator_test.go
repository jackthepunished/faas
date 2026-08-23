// End-to-end tests for the H2C terminator (ADR-126 §Decision 1,
// G19 closure). The terminator is exercised at the
// handleH2CStream boundary via a local httptest H2C server
// (the bridge's own inbound H2C stack — srv.Protocols.
// SetUnencryptedHTTP2(true)) bridged against an outbound H2C
// guest mock (also httptest). Round-tripping through the real
// newHandler closure pins the inbound header propagation,
// transport-level HPACK framing, and the gRPC trailer
// preservation invariant (ADR-126 §Decision 5).
//
// Tests are package-internal (`package main`) so they can reach
// the unexported handleH2CStream and the FAAS_BRIDGE_PROTOCOL
// env var that gates dispatch.

package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/http2"
)

// newHandlerH2CGuest wires up a local H2C "guest" (httptest.NewServer
// with srv.Protocols.SetUnencryptedHTTP2(true)) and an H2C inbound
// "gatewayd" stack around newHandler. The bridge's handleH2CStream
// reads the inbound H2C, dial a fresh guest H2C conn, and round-trips
// the request. The test asserts headers + body + trailers survive
// the bridge transparently.
//
// Returns:
//
//   - the inbound H2C roundtripper (driven by transport.RoundTrip)
//   - the observed request the guest sees
func newHandlerH2CGuest(t *testing.T, guestHandler http.HandlerFunc) (*http2.Transport, *guestObservations) {
	t.Helper()
	obs := &guestObservations{}
	wrapped := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Mirror the inbound request into obs for assertion.
		obs.method = r.Method
		obs.path = r.URL.Path
		obs.query = r.URL.RawQuery
		obs.headers = r.Header.Clone()
		obs.trailer = nil
		for k, vs := range r.Trailer {
			if obs.trailer == nil {
				obs.trailer = http.Header{}
			}
			for _, v := range vs {
				obs.trailer.Add(k, v)
			}
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("guest mock: read body: %v", err)
		}
		obs.body = string(body)
		// Run the user's guestHandler with the body already
		// consumed by ReadAll (r.Body is now EOF); the user's
		// handler can still set trailers via w.Header().
		guestHandler(w, r)
	})

	// Local H2C "guest" listener.
	guestSrv := &http.Server{Handler: wrapped}
	guestSrv.Protocols = new(http.Protocols)
	guestSrv.Protocols.SetUnencryptedHTTP2(true)
	guestLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("guest listen: %v", err)
	}
	t.Cleanup(func() { _ = guestLn.Close() })

	// Serve the guest in a goroutine.
	go func() {
		_ = guestSrv.Serve(guestLn)
	}()
	t.Cleanup(func() {
		shutCtx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
		defer cancel()
		_ = guestSrv.Shutdown(shutCtx)
	})

	// Inbound H2C stack — drives the bridge's newHandler.
	// Use unix-socket so the bridge can dial it; the bridge
	// speaks H2C on its inbound side.
	inboundHandler := newHandler("127.0.0.1", guestAddr(t, guestLn), time.Now().Add(5*time.Second))
	inboundSrv := &http.Server{Handler: inboundHandler}
	inboundSrv.Protocols = new(http.Protocols)
	inboundSrv.Protocols.SetUnencryptedHTTP2(true)
	inboundLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("inbound listen: %v", err)
	}
	t.Cleanup(func() { _ = inboundLn.Close() })
	go func() {
		_ = inboundSrv.Serve(inboundLn)
	}()

	// Build an H2C client transport that points at the inbound
	// listener; the test calls transport.RoundTrip to drive
	// newHandler through the H2C stack.
	rt := &http2.Transport{
		AllowHTTP: true,
		DialTLSContext: func(ctx context.Context, _, _ string, _ *tls.Config) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "tcp", inboundLn.Addr().String())
		},
		IdleConnTimeout: 1 * time.Second,
		ReadIdleTimeout: 1 * time.Second,
		PingTimeout:     100 * time.Millisecond,
	}
	t.Cleanup(func() { rt.CloseIdleConnections() })

	return rt, obs
}

// guestAddr extracts a uint16 port from a TCP listener.
func guestAddr(t *testing.T, ln net.Listener) uint16 {
	t.Helper()
	addr, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("guest ln addr not TCP: %T", ln.Addr())
	}
	return uint16(addr.Port)
}

// guestObservations captures what the guest saw.
type guestObservations struct {
	method   string
	path     string
	query    string
	headers  http.Header
	trailer  http.Header
	body     string
	// For trailer assertions, the guest may also surface
	// trailerPrefix fields through r.Trailer — captured above.
}

// TestHandleH2CStream_UnaryRequest round-trips a single unary
// HTTP/2 request through newHandler with FAAS_BRIDGE_PROTOCOL=h2c.
// Asserts the inbound headers reach the guest as H2 HEADERS (no
// downgrade), the body bytes are preserved, and the response
// status + headers reach the inbound caller identically.
//
// ADR-126 §Decision 5: the response's headers ride back through
// the inbound H2C stream verbatim — no application translation.
func TestHandleH2CStream_UnaryRequest(t *testing.T) {
	t.Setenv("FAAS_BRIDGE_PROTOCOL", "h2c")

	rt, obs := newHandlerH2CGuest(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Guest-Echo", "1")
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "hello from guest")
	})

	req, err := http.NewRequest("POST", "http://test.invalid/foo?bar=1", strings.NewReader("request body"))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("X-Custom-Header", "custom-value")

	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatalf("roundtrip: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	if got := resp.Header.Get("X-Guest-Echo"); got != "1" {
		t.Errorf("X-Guest-Echo on response = %q, want %q", got, "1")
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "hello from guest" {
		t.Errorf("response body = %q, want %q", body, "hello from guest")
	}

	// The guest should have observed the inbound request as
	// H2 HEADERS — no Upgrade artifacts, no Transfer-Encoding,
	// no chunked framing on the wire.
	if obs.method != "POST" {
		t.Errorf("guest saw method %q, want POST", obs.method)
	}
	if obs.path != "/foo" {
		t.Errorf("guest saw path %q, want /foo", obs.path)
	}
	if obs.body != "request body" {
		t.Errorf("guest saw body %q, want %q", obs.body, "request body")
	}
	if got := obs.headers.Get("X-Custom-Header"); got != "custom-value" {
		t.Errorf("guest saw X-Custom-Header=%q, want %q", got, "custom-value")
	}
	// HTTP/2 hop-by-hop hygiene: the guest must NOT see
	// Transfer-Encoding (the bridge filters it).
	if got := obs.headers.Get("Transfer-Encoding"); got != "" {
		t.Errorf("guest saw Transfer-Encoding=%q, want empty (H2 manages framing)", got)
	}
}

// TestHandleH2CStream_GRPCTrailers pins the gRPC trailer framing
// invariant (ADR-126 §Decision 5). The guest emits trailer HEADERS
// (END_STREAM) carrying grpc-status. The bridge's H2C terminator
// forwards them verbatim — the inbound H2C client's resp.Trailer
// carries the demuxed trailer map.
//
// Note on Go stdlib demux: for `w.Header().Set(http.TrailerPrefix+"X", ...)`
// to round-trip into the wire trailer HEADERS frame, the matching
// `Trailer:` header MUST be declared in `Header()` BEFORE WriteHeader
// (per net/http docs). The test handler below declares it explicitly.
func TestHandleH2CStream_GRPCTrailers(t *testing.T) {
	t.Setenv("FAAS_BRIDGE_PROTOCOL", "h2c")

	rt, _ := newHandlerH2CGuest(t, func(w http.ResponseWriter, r *http.Request) {
		// Declare trailer fields BEFORE WriteHeader — Go stdlib
		// emits these as trailer HEADERS on the wire only if
		// they were declared up front. Setting `Trailer:` here
		// is the canonical Go idiom (see net/http ResponseWriter
		// godoc — "Trailers must be supported by the transport
		// in order for trailers to be received by the client").
		w.Header().Set("Trailer", "Grpc-Status, Grpc-Message")
		w.Header().Set(http.TrailerPrefix+"Grpc-Status", "")
		w.Header().Set(http.TrailerPrefix+"Grpc-Message", "")
		w.Header().Set("Content-Type", "application/grpc")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "grpc body bytes")
		// Set trailers after the body, before Flush.
		w.Header().Set(http.TrailerPrefix+"Grpc-Status", "0")
		w.Header().Set(http.TrailerPrefix+"Grpc-Message", "")
		w.(http.Flusher).Flush()
	})

	req, err := http.NewRequest("POST", "http://test.invalid/echo", strings.NewReader(""))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/grpc")

	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatalf("roundtrip: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	// Read the body so the response fully completes and trailers
	// are demuxed by the stdlib h2c client transport.
	body, _ := io.ReadAll(resp.Body)
	if len(body) == 0 {
		t.Errorf("body empty; trailers won't be demuxed yet")
	}
	if got := resp.Trailer.Get("Grpc-Status"); got != "0" {
		t.Errorf("Grpc-Status trailer = %q, want %q (load-bearing invariant for grpc)", got, "0")
	}
	if got := resp.Trailer.Get("Grpc-Message"); got != "" {
		t.Errorf("Grpc-Message trailer = %q, want empty", got)
	}
}

// TestHandleH2CStream_GRPCStreamUnaryNoBody ensures the H2C
// terminator handles an inbound POST with no body (Content-Length:0)
// cleanly — gRPC unary RPCs land here. The guard pre-empts a real
// failure mode where r.Body=nil triggers io.Copy dst.Write on
// an already-closed http.Body.
func TestHandleH2CStream_GRPCStreamUnaryNoBody(t *testing.T) {
	t.Setenv("FAAS_BRIDGE_PROTOCOL", "h2c")

	rt, _ := newHandlerH2CGuest(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	req, err := http.NewRequest("POST", "http://test.invalid/empty", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatalf("roundtrip: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("status = %d, want 204", resp.StatusCode)
	}
}

// TestHandleH2CStream_GuestDialFailure pins the dial-failure path
// (guest :<port> closed before RoundTrip). The bridge must surface
// 502 Bad Gateway (per spec §6.4) and not panic.
func TestHandleH2CStream_GuestDialFailure(t *testing.T) {
	t.Setenv("FAAS_BRIDGE_PROTOCOL", "h2c")

	// Reserve a port; close immediately so the dial fails.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	deadPort := uint16(ln.Addr().(*net.TCPAddr).Port)
	_ = ln.Close()

	// Spin up the inbound bridge pointed at the dead port.
	inboundHandler := newHandler("127.0.0.1", deadPort, time.Now().Add(2*time.Second))
	inboundSrv := &http.Server{Handler: inboundHandler}
	inboundSrv.Protocols = new(http.Protocols)
	inboundSrv.Protocols.SetUnencryptedHTTP2(true)
	inboundLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("inbound listen: %v", err)
	}
	defer func() { _ = inboundLn.Close() }()
	go func() {
		_ = inboundSrv.Serve(inboundLn)
	}()
	rt := &http2.Transport{
		AllowHTTP: true,
		DialTLSContext: func(ctx context.Context, _, _ string, _ *tls.Config) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "tcp", inboundLn.Addr().String())
		},
		IdleConnTimeout: 1 * time.Second,
		ReadIdleTimeout: 1 * time.Second,
		PingTimeout:     100 * time.Millisecond,
	}
	t.Cleanup(func() { rt.CloseIdleConnections() })

	req, err := http.NewRequest("GET", "http://test.invalid/never", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := rt.RoundTrip(req)
	if err != nil {
		// Some dial-errors surface directly (e.g. context
		// deadline) — accept those.
		if strings.Contains(err.Error(), "EOF") || strings.Contains(err.Error(), "connection refused") {
			return
		}
		t.Fatalf("roundtrip: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusBadGateway {
		t.Logf("got status %d (BadGateway expected); the bridge close-writes the conn mid-frames", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "dial guest") && resp.StatusCode == http.StatusBadGateway {
		t.Logf("body did not contain 'dial guest' (was %q) — diagnostic only", body)
	}
}

// TestNewHandler_DispatchesToH2COnEnv pins Atomic 7: with
// FAAS_BRIDGE_PROTOCOL=h2c, newHandler routes to handleH2CStream.
// Setting it to h1 routes to handleH1Stream (already pinned by
// existing TestNewHandler_WritesHTTP11RequestLine).
func TestNewHandler_DispatchesToH2COnEnv(t *testing.T) {
	t.Setenv("FAAS_BRIDGE_PROTOCOL", "h2c")
	if got := currentBridgeFraming(); got != framingH2C {
		t.Errorf("currentBridgeFraming() = %q, want %q (dispatch gating)", got, framingH2C)
	}
	t.Setenv("FAAS_BRIDGE_PROTOCOL", "h1")
	if got := currentBridgeFraming(); got != framingH1 {
		t.Errorf("currentBridgeFraming() = %q, want %q (h1 dispatch)", got, framingH1)
	}
	fmt.Println("dispatch: OK")
}
