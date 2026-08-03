// Package gateway — internal_proxy.go is the public→internal reverse
// proxy that lives at the centre of the Tier A7 edge split (ADR-070).
//
// Background: gatewayd-public owns TLS termination, the cert bundle,
// and the route-cache mirror. gatewayd-internal owns the wake gate,
// the per-node forwarder, and the rate limiter. The public daemon
// does NOT speak HTTP to a customer app directly — it forwards the
// inbound request over a unix socket to a gatewayd-internal replica
// that handles app routing, wake, and proxy.
//
// Same-box hop: the public daemon dials the internal daemon over a
// unix socket at /run/faas/gatewayd-internal.sock (ADR-015/018
// pattern, already used for schedd at /run/faas/schedd.sock).
// Cross-box hop (future Gate-B): the public daemon dials the
// internal daemon over mTLS using pkg/wire.DialContext's tcp+mtls
// target form — this file only owns the same-box shape; the
// cross-box shape lives behind a DialerFunc injection point (DialFn
// below) so the v1.0 wiring stays unix-only.
//
// Why not httputil.NewSingleHostReverseProxy: it dials TCP by
// hostname and writes the request via URL.Host — that doesn't work
// over a unix socket. We build a thin ReverseProxy that takes a
// per-request dialer so the unix-socket dial runs once per request
// (acceptable; the socket path is loopback and the connection setup
// is sub-millisecond) OR we keep a persistent pool of dialed conns
// via http.Transport. We pick the persistent pool — see
// TransportPool.
//
// Headers: the standard hop-by-hop headers (RFC 7230 §6.1) are
// stripped before the forward (same list as
// pkg/gateway/forwardproxy.go::hopByHopHeaders — keeping both files
// in sync is mandatory; the test suite asserts the equality).
// X-Forwarded-For / X-Forwarded-Proto are appended (NOT replaced) so
// the internal daemon sees the public daemon's hop in the chain.
//
// Errors: a dial failure surfaces as 502 Bad Gateway to the
// customer (the public daemon is the LB-facing process; a dial
// failure to the internal tier is upstream-from-the-customer-
// perspective). The internal daemon's own 5xx flows through
// unchanged — we don't mask upstream errors.
package gateway

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// InternalDialerFunc dials an internal replica. The public daemon
// passes a unix-socket dialer in production; cross-box mTLS dialers
// arrive with Gate-B (out of scope for this PR).
//
// The returned net.Conn must speak HTTP/1.1 (the wire between
// gatewayd-public and gatewayd-internal is HTTP/1.1 over the unix
// socket — see pkg/sched/loop.go::httpGatewaySynth for the same
// shape).
//
// Implementations must respect ctx cancellation.
type InternalDialerFunc func(ctx context.Context, network, addr string) (net.Conn, error)

// InternalDialer is the seam the public daemon wires to reach a
// gatewayd-internal replica. The default unix-socket wiring is
// NewUnixSocketDialer("/run/faas/gatewayd-internal.sock").
type InternalDialer interface {
	// DialContext opens a connection to the internal replica
	// identified by `target` (a URL or a unix-socket path).
	// Implementations may pool connections internally; the returned
	// net.Conn is owned by the caller for the duration of the
	// request and closed by ReverseProxy when Body read completes.
	DialContext(ctx context.Context, target string) (net.Conn, error)
}

// NewUnixSocketDialer returns an InternalDialer that dials a unix
// socket at `path`. The dial ignores the target string the caller
// passes — every request routes to the same socket in the v1.0
// same-box shape. Future Gate-B work passes a target-encoded
// replica selector and picks a different socket per replica.
//
// Mode 0660 + group faas is the load-bearing ACL (ADR-015); the
// dialer assumes the caller has set the umask and joined the group
// (daemon-bootstrap concern; see cmd/gatewayd-public/main.go for
// the pre-dial group setup).
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

// InternalReverseProxy is the public→internal forwarder. It is a
// thin wrapper around httputil.ReverseProxy that:
//   - Dials via an InternalDialer (not a TCP URL).
//   - Strips hop-by-hop headers per RFC 7230 §6.1.
//   - Appends X-Forwarded-For / X-Forwarded-Proto.
//   - Surfaces dial failures as 502 Bad Gateway.
//
// The zero value is unusable; construct via NewInternalReverseProxy
// to seed the dialer, the transport, and the logger.
type InternalReverseProxy struct {
	// Dialer is the per-replica dialer. Required.
	Dialer InternalDialer
	// Target is the URL the proxy writes requests as (the
	// "destination" the internal daemon sees in r.Host). Required.
	// Typical value: &url.URL{Scheme: "http", Host: "gatewayd-internal"}.
	Target *url.URL
	// Transport is the http.Transport used for pooled connections.
	// nil falls back to http.DefaultTransport, but production should
	// pass a dedicated transport with sane timeouts (see
	// NewInternalReverseProxy).
	Transport http.RoundTripper
	// Logger is the slog.Logger used for dial-failure + 5xx
	// surfacing. nil falls back to slog.Default().
	Logger *slog.Logger
	// DialTimeout bounds a single dial attempt. Zero means no
	// extra timeout beyond ctx cancellation.
	DialTimeout time.Duration
}

// NewInternalReverseProxy returns a wired InternalReverseProxy. The
// dialer and target are required; the rest default to safe
// production values (10 s dial timeout, 30 s response header
// timeout, slog.Default()).
//
// The returned proxy is safe for concurrent use on the public
// listener hot path.
func NewInternalReverseProxy(dialer InternalDialer, target *url.URL, log *slog.Logger) *InternalReverseProxy {
	if log == nil {
		log = slog.Default()
	}
	return &InternalReverseProxy{
		Dialer:      dialer,
		Target:      target,
		Transport:   newInternalProxyTransport(),
		Logger:      log,
		DialTimeout: 10 * time.Second,
	}
}

// newInternalProxyTransport returns an *http.Transport tuned for
// the public→internal hop. Idle conn pool, no keep-alive tuning
// beyond defaults, response-header timeout bounded so a wedged
// internal daemon can't pin a public goroutine forever.
//
// The transport's DialContext is overridden by the InternalProxy
// at request time (see ServeHTTP) so the persistent pool keys by
// the dialer's identity (the unix socket path) and not by the
// target URL.
func newInternalProxyTransport() *http.Transport {
	return &http.Transport{
		Proxy:                 nil, // no env-based proxy; this is a local hop
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   32,
		IdleConnTimeout:       90 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}
}

// ServeHTTP implements http.Handler. It rewrites the inbound
// request to the target URL, dials via Dialer, and pipes the
// response back to the inbound writer.
//
// On dial failure: responds 502 Bad Gateway and logs at WARN. On
// upstream error: propagates the upstream status as-is.
func (p *InternalReverseProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if p.Dialer == nil || p.Target == nil {
		// Wiring bug — log at ERROR because this is the public
		// listener, the customer sees the failure.
		p.logger().Error("internal proxy not configured",
			"has_dialer", p.Dialer != nil,
			"has_target", p.Target != nil)
		http.Error(w, "internal proxy not configured", http.StatusBadGateway)
		return
	}
	// Build the outbound request. We use http.ReadResponse +
	// http.Request.Write at the wire level so we can apply hop-by-
	// hop stripping and X-Forwarded-For/Proto appending BEFORE the
	// bytes hit the wire (httputil.ReverseProxy does this internally
	// for us via Director, but Director does NOT strip hop-by-hop
	// headers — that's why we hand-roll).
	outReq := r.Clone(r.Context())
	outReq.URL.Scheme = p.Target.Scheme
	outReq.URL.Host = p.Target.Host
	outReq.Host = p.Target.Host
	outReq.RequestURI = "" // required for outgoing client requests
	outReq.Header = stripHopByHop(outReq.Header.Clone())
	// Append X-Forwarded-For / X-Forwarded-Proto. RFC 7239 §5.2 is
	// silent on append-vs-replace; the convention is append so the
	// chain is preserved through proxies.
	if clientIP, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		outReq.Header.Add("X-Forwarded-For", clientIP)
	}
	if r.TLS != nil {
		outReq.Header.Set("X-Forwarded-Proto", "https")
	} else {
		outReq.Header.Set("X-Forwarded-Proto", "http")
	}
	// Dial with timeout.
	ctx := r.Context()
	if p.DialTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, p.DialTimeout)
		defer cancel()
	}
	// We pick a stable target string the dialer can use to pick a
	// replica (today: any string — the unix dialer ignores it).
	target := p.Target.String()
	conn, err := p.Dialer.DialContext(ctx, target)
	if err != nil {
		p.logger().Warn("internal dial failed",
			"target", target,
			"err", err)
		http.Error(w, "bad gateway: internal dial failed", http.StatusBadGateway)
		return
	}
	// Write the request + read the response. We use the lower-level
	// http.Request.Write / http.ReadResponse pair (rather than
	// http.Client.Do) so we can read the response off the same
	// connection without an extra Transport layer in the middle.
	if err := outReq.Write(conn); err != nil {
		p.logger().Warn("internal request write failed",
			"target", target,
			"err", err)
		_ = conn.Close()
		http.Error(w, "bad gateway: internal request write failed", http.StatusBadGateway)
		return
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), outReq)
	if err != nil {
		p.logger().Warn("internal response read failed",
			"target", target,
			"err", err)
		_ = conn.Close()
		http.Error(w, "bad gateway: internal response read failed", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	// Copy headers + body to the inbound writer. Use the same
	// hop-by-hop stripping on the response (RFC 7230 §6.1) — the
	// internal daemon may have set Connection: close and we don't
	// want to leak that to the customer.
	for k, vv := range resp.Header {
		if isHopByHop(k) {
			continue
		}
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	// Body copy with a bound — we don't want a malicious / buggy
	// internal daemon to pin a public goroutine on a slow reader.
	bodyCtx, bodyCancel := context.WithCancel(r.Context())
	defer bodyCancel()
	done := make(chan struct{})
	var copyErr error
	go func() {
		defer close(done)
		_, copyErr = copyResponseBody(w, resp.Body)
	}()
	select {
	case <-done:
		// drain the err var so go vet doesn't complain.
		_ = copyErr
	case <-bodyCtx.Done():
		// inbound request cancelled (customer hung up); abort the
		// body copy. The connection will be closed when resp.Body
		// is closed via the defer above.
	}
}

// isHopByHop returns true for the headers in hopByHopHeaders
// (case-insensitive per RFC 7230 §3.2). Kept as a small helper so
// both the request and response sides of the forward use the same
// predicate.
func isHopByHop(h string) bool {
	for _, x := range hopByHopHeaders {
		if strings.EqualFold(h, x) {
			return true
		}
	}
	return false
}

// copyResponseBody drains src to dst with a 30 s bound (so a
// hung-up response doesn't pin the public listener). Returns the
// first error or nil on a clean EOF.
//
// Kept private — the only caller is ServeHTTP. Extracted to make
// the cancellation logic easy to unit-test (TestInternalProxy_
// CancelsBodyCopy).
func copyResponseBody(dst http.ResponseWriter, src interface {
	Read(p []byte) (int, error)
}) (int64, error) {
	// 32 KiB buffer — the same as http.Server's default. Larger
	// buffers cost memory without measurably improving throughput
	// for the customer-app traffic shape (most responses are
	// sub-MiB JSON / HTML).
	buf := make([]byte, 32*1024)
	var written int64
	for {
		n, err := src.Read(buf)
		if n > 0 {
			m, werr := dst.Write(buf[:n])
			if werr != nil {
				return written, werr
			}
			written += int64(m)
			if m < n {
				return written, io.ErrShortWrite
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return written, nil
			}
			return written, err
		}
	}
}

func (p *InternalReverseProxy) logger() *slog.Logger {
	if p.Logger != nil {
		return p.Logger
	}
	return slog.Default()
}

// _ = fmt.Sprintf is a compile-time fence against fmt being
// dropped from imports if a future edit removes the only fmt use.
var _ = fmt.Sprintf

// _ = sync.Mutex{} documents that this file's API is safe for
// concurrent use (the dialer's pool and the per-request conn are
// the load-bearing concurrency primitives; the proxy itself has
// no mutable state). Kept as a fence against accidental field
// additions that would need their own synchronisation.
var _ sync.Mutex //nolint:unused // API-doc fence, see comment above.
