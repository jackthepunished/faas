// Package gateway — internal_proxy.go is the public→internal reverse
// proxy that lives at the centre of the Tier A7 edge split (ADR-070).
//
// gatewayd-public forwards every customer request to gatewayd-internal
// over the same-box unix socket at /run/faas/gatewayd-internal.sock
// (ADR-015/018). We use http.Transport.RoundTrip with a custom
// DialContext that dials the unix socket — the standard transport
// gives us body handling, idle-conn pooling, request timeouts, and
// the canonical "client.Do" semantics for free. The unix-socket
// dialer is a single line (`net.Dialer.DialContext(ctx, "unix", path)`).
//
// Same-box only in v1.0; cross-box mTLS is Gate-B. The dialer is
// injected via InternalDialer so Gate-B can swap in a TLS dialer
// without touching the proxy.
//
// Trust model (security):
//
// gatewayd-public is the only hop the customer can reach. The
// internal daemon reads X-Forwarded-For to enforce per-IP rate
// limits (ADR-070). If we APPENDED the customer's XFF header
// we would trust every customer to write their own per-IP key — a
// trivial bypass. We therefore STRIP the inbound XFF and re-add
// only the public daemon's own RemoteAddr. The chain is
// `customer-=public-remote-addr` going in; the internal daemon
// trusts exactly one hop. X-Forwarded-Proto is preserved (the
// public hop is the only TLS terminator; downstream hops are
// plaintext unix).
//
// Hop-by-hop headers (RFC 7230 §6.1) are stripped on both request
// and response — match the forwardproxy.go list so the test suite
// that asserts equality stays green.
package gateway

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/net/http2"
)

// InternalDialer is the seam the public daemon wires to reach a
// gatewayd-internal replica. The default unix-socket wiring is
// NewUnixSocketDialer("/run/faas/gatewayd-internal.sock").
//
// Note: the dialer may pool connections internally; the returned
// net.Conn is owned by the proxy for the duration of the request
// and closed by the transport when the body read completes.
type InternalDialer interface {
	DialContext(ctx context.Context, target string) (net.Conn, error)
}

// NewUnixSocketDialer returns an InternalDialer that dials a unix
// socket at `path`. The dialer ignores the target string the caller
// passes — every request routes to the same socket in the v1.0
// same-box shape. Future Gate-B work passes a target-encoded replica
// selector and picks a different socket per replica.
//
// ACL: the daemon runs as faas:faas (per the systemd unit). The
// internal daemon chmods the socket 0660 on bind, so the dialer
// only needs to be in the `faas` group — there's no per-process
// umask dance.
func NewUnixSocketDialer(path string) InternalDialer {
	return &unixSocketDialer{path: path}
}

type unixSocketDialer struct {
	path string
}

func (d *unixSocketDialer) DialContext(ctx context.Context, _ string) (net.Conn, error) {
	var dnet net.Dialer
	return dnet.DialContext(ctx, "unix", d.path)
}

// InternalReverseProxy is the public→internal forwarder. It wraps
// http.Transport with a DialContext that dials the unix socket, and
// applies the load-bearing header rewrites (XFF strip-and-rebuild,
// hop-by-hop strip, X-Forwarded-Proto) before the request hits the
// wire.
//
// The zero value is unusable; construct via NewInternalReverseProxy.
type InternalReverseProxy struct {
	Dialer      InternalDialer
	Target      *url.URL
	Transport   http.RoundTripper
	Logger      *slog.Logger
	DialTimeout time.Duration
}

// NewInternalReverseProxy returns a wired InternalReverseProxy. The
// dialer and target are required; the rest default to safe
// production values (10 s dial timeout, 30 s response header
// timeout, slog.Default()).
//
// useH2C switches the upstream transport to H2C (HTTP/2 cleartext)
// for the in-process unix-socket hop. Issue #675: default is true so
// the Tier A7 production path negotiates H2 end-to-end with the
// gatewayd-internal h2c.NewHandler wrapper. Set to false to fall
// back to the legacy HTTP/1.1 transport (e.g. for rollback via the
// FAAS_INTERNAL_H2C env knob in cmd/gatewayd-public/main.go).
//
// The returned proxy is safe for concurrent use on the public
// listener hot path.
func NewInternalReverseProxy(dialer InternalDialer, target *url.URL, log *slog.Logger, useH2C bool) *InternalReverseProxy {
	if log == nil {
		log = slog.Default()
	}
	var transport http.RoundTripper
	if useH2C {
		transport = newInternalProxyH2CTransport(dialer)
	} else {
		transport = newInternalProxyTransport(dialer)
	}
	return &InternalReverseProxy{
		Dialer:      dialer,
		Target:      target,
		Transport:   transport,
		Logger:      log,
		DialTimeout: 10 * time.Second,
	}
}

// newInternalProxyTransport returns an *http.Transport whose
// DialContext is the unix-socket dialer. The transport is the
// single seam the proxy uses; standard *http.Client semantics
// cover request body, hop-by-hop connection close, idle-conn
// reuse, and response timeout.
func newInternalProxyTransport(dialer InternalDialer) *http.Transport {
	return &http.Transport{
		// Disable the env-driven HTTP proxy; this is a local hop.
		Proxy: nil,
		// DialContext is the load-bearing override — every request
		// dials via the unix socket (or whatever InternalDialer
		// returns). The transport's idle pool keys by the dialer's
		// identity (the unix socket path) so the same in-flight
		// conn is reused across requests.
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return dialer.DialContext(ctx, "")
		},
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   32,
		IdleConnTimeout:       90 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		// Disable HTTP/2 — the unix-socket hop is HTTP/1.1 only.
		// Issue #675: this is the legacy HTTP/1.1 path; for the
		// production Tier A7 hop use newInternalProxyH2CTransport
		// instead. Kept here so the legacy e2e harness path (which
		// expects HTTP/1.1) keeps working without any knob.
		ForceAttemptHTTP2: false,
	}
}

// newInternalProxyH2CTransport returns an http2.Transport that
// dials the unix socket via the supplied InternalDialer. H2C
// (HTTP/2 cleartext) is the only way to negotiate HTTP/2 over the
// plaintext unix socket — the stdlib http.Transport refuses to do
// H2 without TLS even when ForceAttemptHTTP2 is true.
//
// The server side of this hop (gatewayd-internal) sets
// srv.Protocols.SetUnencryptedHTTP2(true) in cmd/gatewayd-internal/run.go
// so the negotiation is symmetric. Together they carry H2 frames in
// both directions on the same in-process socket that used to be
// HTTP/1.1.
//
// Streaming responses (app.StreamingEnabled=true) keep working
// because the customer handler writes its own chunked framing
// downstream of this hop — H2C is transparent to the streaming
// path. The handler-to-guest leg stays HTTP/1.1 plaintext per the
// issue #675 decision to keep streaming on HTTP/1.1.
//
// Connection lifetime mirrors the HTTP/1.1 transport above so an
// operator toggling between the two transports via FAAS_INTERNAL_H2C
// doesn't see different idle-conn behaviour. ReadIdleTimeout +
// PingTimeout are H2-specific keepalive parameters that have no
// HTTP/1.1 equivalent.
func newInternalProxyH2CTransport(dialer InternalDialer) http.RoundTripper {
	t := &http2.Transport{
		// AllowHTTP is the load-bearing flag — without it http2
		// refuses to dial plain HTTP and falls back to TLS-or-error.
		AllowHTTP: true,
		// DialTLS is the seam the http2 library uses to acquire a
		// net.Conn. We ignore network/addr/cfg and route everything
		// through the unix-socket InternalDialer. The context passed
		// here is the request context (caller's deadline) — we
		// propagate it to the dialer so cancellation works. Without
		// this contextcheck flagged the closure (golangci-lint v2.4
		// contextcheck rule).
		DialTLSContext: func(ctx context.Context, _ string, _ string, _ *tls.Config) (net.Conn, error) {
			return dialer.DialContext(ctx, "")
		},
		ReadIdleTimeout: 30 * time.Second,
		PingTimeout:     15 * time.Second,
	}
	return t
}

// ServeHTTP implements http.Handler. It rewrites the inbound
// request to the target URL, dials via Transport (which dials the
// unix socket), and pipes the response back to the inbound writer.
//
// On dial failure: 502 Bad Gateway. On upstream error: propagated
// unchanged.
func (p *InternalReverseProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if p.Dialer == nil || p.Target == nil {
		// Wiring bug — log at ERROR because the customer sees the
		// failure on the public listener.
		p.logger().Error("internal proxy not configured",
			"has_dialer", p.Dialer != nil,
			"has_target", p.Target != nil)
		http.Error(w, "internal proxy not configured", http.StatusBadGateway)
		return
	}
	outReq := r.Clone(r.Context())
	outReq.URL.Scheme = p.Target.Scheme
	outReq.URL.Host = p.Target.Host
	outReq.Host = p.Target.Host
	outReq.RequestURI = "" // required for outgoing client requests
	// Strip hop-by-hop in place (no second map alloc). The strip
	// is correct for plain HTTP (RFC 7230 §6.1) — but it would
	// destroy the Connection: Upgrade + Upgrade: <token>
	// handshake for inbound WebSocket / h2c / MQTT-over-WS
	// requests before gatewayd-internal's Upgrade detector ever
	// sees them (issue #676 / ADR-080). Skip the strip when the
	// request is an upgrade; the detector lives in pkg/gateway
	// (upgrade.go) and is shared with Handler.ServeHTTP so the
	// two sides of the public→internal hop agree on the
	// case-insensitive RFC 7230 §3.2 parse.
	if !isUpgradeRequest(r) {
		stripHopByHopInPlace(outReq.Header)
	}
	// XFF trust: strip the inbound chain (the customer could
	// forge any IP) and re-add only the public daemon's RemoteAddr.
	// The internal daemon sees exactly one trusted hop.
	outReq.Header.Del("X-Forwarded-For")
	if clientIP, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		outReq.Header.Set("X-Forwarded-For", clientIP)
	}
	if r.TLS != nil {
		outReq.Header.Set("X-Forwarded-Proto", "https")
	} else {
		outReq.Header.Set("X-Forwarded-Proto", "http")
	}
	// Bind the dial timeout via ctx so the transport honours it.
	ctx := r.Context()
	if p.DialTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, p.DialTimeout)
		defer cancel()
	}
	resp, err := p.Transport.RoundTrip(outReq)
	if err != nil {
		// Distinguish "upstream unreachable" (502) from "client
		// cancelled" (the latter is a normal close, don't log
		// at WARN).
		if errors.Is(err, context.Canceled) && r.Context().Err() != nil {
			return
		}
		p.logger().Warn("internal round-trip failed",
			"target", p.Target.String(),
			"err", err)
		http.Error(w, "bad gateway: internal round-trip failed", http.StatusBadGateway)
		return
	}
	defer func() { _ = resp.Body.Close() }()
	// Copy headers + body to the inbound writer. Strip hop-by-hop
	// in place on the response (RFC 7230 §6.1) — the internal
	// daemon may have set Connection: close and we don't want to
	// leak that to the customer.
	for k, vv := range resp.Header {
		if isHopByHop(k) {
			continue
		}
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	// Body copy bound to ctx — a hung upstream pins only the
	// in-flight goroutine, not the listener.
	if _, err := copyResponseBody(ctx, w, resp.Body); err != nil && !errors.Is(err, context.Canceled) {
		p.logger().Warn("internal body copy failed",
			"target", p.Target.String(),
			"err", err)
	}
}

// stripHopByHopInPlace deletes the hop-by-hop headers (RFC 7230
// §6.1) from h in place. Avoids the double-alloc of Clone +
// rebuild.
func stripHopByHopInPlace(h http.Header) {
	for k := range h {
		if isHopByHop(k) {
			delete(h, k)
		}
	}
}

// isHopByHop returns true for the headers in hopByHopHeaders
// (case-insensitive per RFC 7230 §3.2).
func isHopByHop(h string) bool {
	for _, x := range hopByHopHeaders {
		if strings.EqualFold(h, x) {
			return true
		}
	}
	return false
}

// copyResponseBody drains src to dst, returning on ctx cancel or
// io.EOF. The caller is expected to have set a request-level
// timeout (r.Context) that bounds the copy.
func copyResponseBody(ctx context.Context, dst io.Writer, src io.Reader) (int64, error) {
	type result struct {
		n   int64
		err error
	}
	done := make(chan result, 1)
	go func() {
		// 32 KiB — same as http.Server's default. Larger buffers
		// cost memory without measurably improving throughput for
		// the customer-app traffic shape (most responses are
		// sub-MiB JSON / HTML).
		n, err := io.Copy(dst, src)
		done <- result{n, err}
	}()
	select {
	case r := <-done:
		return r.n, r.err
	case <-ctx.Done():
		return 0, ctx.Err()
	}
}

func (p *InternalReverseProxy) logger() *slog.Logger {
	if p.Logger != nil {
		return p.Logger
	}
	return slog.Default()
}
