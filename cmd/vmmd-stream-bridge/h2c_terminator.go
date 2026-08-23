// H2C terminator (ADR-126, G19). For inbound H2C requests whose
// app_protocol ∈ {http2, grpc}, the bridge originates HTTP/2
// prior-knowledge frames to the guest instead of re-framing to
// H1+chunked. The terminator is a per-stream io.ReadWriter against
// a guest TCP conn — each inbound H2C request gets its own guest
// dial (one-bridge-stream-per-guest-stream).
//
// Why per-stream dial: same rationale as today's v2 path
// (main.go::handleH1Stream). HPACK state isolation per request,
// no cross-stream HEADERS coupling, no conn migration hazard.
// The cost is one dial per request — measured at ~1ms on the
// same-host guest netns, and amortised to noise vs the 350 ms
// cold-boot.
//
// Wire envelope: HTTP/2 prior-knowledge (no Upgrade dance).
//
//	client →  PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n
//	client →  SETTINGS (client-side)
//	← server  SETTINGS (server-side)
//	client →  SETTINGS ACK
//	← server  SETTINGS ACK
//	client →  HEADERS (request headers, END_HEADERS)
//	client →  DATA (request body, may be multiple frames)
//	← server  HEADERS (response headers, END_HEADERS)
//	← server  DATA (response body, may be multiple frames)
//	← server  HEADERS (trailers, END_HEADERS | END_STREAM)
//	                  -- for grpc, carries grpc-status etc.
//	← server  (closes conn; END_STREAM on last DATA)
//
// Trailer framing for gRPC (RFC 7540 §8.1): trailers ride on a
// HEADERS frame with END_STREAM flag set. The Go stdlib
// `http.TrailerPrefix("Grpc-Status")` demux happens automatically
// in the inbound ResponseWriter — we don't need to forward
// trailers separately; they're already in the response HEADERS
// stream the bridge reads off the guest.
//
// Three rejection shapes (defensive):
//
//  1. `app_protocol` missing or empty → handleH1Stream (caller's
//     dispatch in main.go::newHandler).
//  2. guest dial fails (10.0.0.2:<port> not listening) → 502
//     BadGateway on the inbound H2C stream. Per spec §6.4.
//  3. HTTP/2 preface write fails → 502 + ctx cancellation. The
//     inbound H2C stream is then `GOAWAY`'d by the stdlib server.
//
// HPACK state is owned by `golang.org/x/net/http2.Transport` —
// same pattern as `pkg/vmmdgrpc/forward.go::newStreamBridgeH2CTransport`
// but pointed at the guest TCP addr instead of a unix socket.
// We reuse the transport's RoundTrip path which constructs
// HEADERS + DATA frames for us; we don't hand-roll HPACK tables.
package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"time"

	"golang.org/x/net/http2"
)

// h2cIdleConnTimeout is the per-guest H2C client transport idle
// conn timeout. With one-bridge-stream-per-guest-stream this
// fires on conn leak only (we close after every response); kept
// short so a leaked conn doesn't pin a guest-side fd.
const h2cIdleConnTimeout = 30 * time.Second

// h2cDialTimeout caps the inner-leg TCP dial against the guest for
// the H2C path. Same 30s as dialTimeout; H2C requires no extra
// round-trip for the Upgrade dance (prior-knowledge saves it).
const h2cDialTimeout = 30 * time.Second

// handleH2CStream is the bridge-side prior-knowledge H2C
// terminator (ADR-126 §Decision 1). The function is the per-
// stream handler closure dispatched from newHandler when
// FAAS_BRIDGE_PROTOCOL=h2c.
//
// Shape:
//
//  1. Capture the inbound H2C request's method / URL / headers /
//     body via the stdlib http.ResponseWriter + http.Request —
//     the inbound side is already an H2C stream managed by
//     stdlib (srv.Protocols.SetUnencryptedHTTP2(true) at
//     main.go:159-160).
//
//  2. Build the guest-side H2C client transport (or reuse a fresh
//     one per request — same shape as handleH1Stream; per-stream
//     isolation per ADR-126 §Decision 4).
//
//  3. RoundTrip the inbound request through the guest transport;
//     round-trip synthesises the HTTP/2 frame envelope (preface +
//     SETTINGS + HEADERS + DATA + trailers) on the guest conn
//     and reads back the response HEADERS + DATA.
//
//  4. Mirror the response HEADERS (status + headers; trailers are
//     folded into the same HEADERS frame on H2) onto the inbound
//     H2C ResponseWriter. Stream the body via io.Copy to the
//     inbound w (the inbound transport handles the DATA framing).
//
// The function is shape-identical to handleH1Stream — the only
// difference is the guest-side transport (H2C vs H1+chunked).
// No application-level translation; HPACK state owned by the
// transport.
func handleH2CStream(w http.ResponseWriter, r *http.Request, guestIP string, guestPort uint16, deadline time.Time) {
	ctx, cancel := context.WithDeadline(r.Context(), deadline)
	defer cancel()

	method := r.Method
	if method == "" {
		method = "GET"
	}
	uri := r.URL.RequestURI()
	if uri == "" {
		uri = "/"
	}
	host := r.Host
	if host == "" {
		// Defense-in-depth: never send an empty :authority
		// pseudo-header. Use the bridge's bound guest IP. HTTP/2
		// forbids empty :authority (RFC 9113 §8.3.1).
		host = guestIP
	}

	// Build the outbound guest H2C transport (prior-knowledge).
	// Each call gets its own transport; one-bridge-stream-per-
	// guest-stream (ADR-126 §Decision 4) means we don't
	// concatenate streams onto a pooled conn — the v1 shape
	// keeps the conn ownership simple.
	transport := newGuestH2CTransport(guestIP, guestPort)

	// Compose the outbound H2C request body. The inbound r.Body
	// is the bridge's H2C-side request stream; we pipe it
	// straight through to the outbound request body's reader.
	// http2.Transport's RoundTrip takes an http.Request whose
	// Body is consumed synchronously during the call; for
	// streaming, we set the request's body to a net.Pipe-style
	// channel and bridge r.Body via io.Copy in a goroutine.
	//
	// PRIOR-KNOWLEDGE H2C scheme: the outbound URL must be
	// `http://<guest-ip>[:<port>]` — x/net/http2.Transport with
	// AllowHTTP=true reads the scheme and Host off the URL and
	// derives :scheme (h2c) + :authority (host). An empty or
	// non-http scheme surfaces as `unsupported scheme` (tested
	// by h2c_terminator_test.go::TestHandleH2CStream_UnaryRequest).
	outboundURL := "http://" + net.JoinHostPort(guestIP, strconv.FormatUint(uint64(guestPort), 10)) + uri
	outboundReq, err := http.NewRequestWithContext(ctx, method, outboundURL, r.Body)
	if err != nil {
		http.Error(w, fmt.Sprintf("build outbound request: %v", err), http.StatusBadGateway)
		return
	}
	// Propagate selected inbound headers to the outbound request.
	// The stdlib h2c client transport handles :authority and :scheme
	// pseudo-headers from outboundReq.URL.Host / outboundReq.URL.Scheme
	// — we want prior-knowledge, so no TLS scheme; http:// is fine.
	//
	// Headers that govern framing and net behavior (Connection,
	// Upgrade, Keep-Alive, Transfer-Encoding, Content-Length)
	// are dropped: HTTP/2 controls framing via frame flags, not
	// headers. https://pkg.go.dev/net/http says
	// "If Body is non-nil and the Request.Header does not contain
	// Content-Length, the transport adds Content-Length" — but for
	// outbound H2C we'll let the transport derive it.
	for k, vs := range r.Header {
		// Skip hop-by-hop headers (RFC 7230 §6.1) and HTTP/2
		// frame-control headers that the transport owns.
		if isHopByHopHeader(k) {
			continue
		}
		for _, v := range vs {
			outboundReq.Header.Add(k, v)
		}
	}
	if outboundReq.Header.Get("Host") == "" {
		outboundReq.Header.Set("Host", host)
	}

	// RoundTrip — this writes the HTTP/2 connection preface +
	// client SETTINGS to the guest, opens a new H2 stream, sends
	// HEADERS + DATA frames, and reads back the response. The
	// response's Body is the guest's DATA stream (frames folded
	// transparently by the transport).
	resp, err := transport.RoundTrip(outboundReq)
	if err != nil {
		http.Error(w, fmt.Sprintf("guest h2c roundtrip: %v", err), http.StatusBadGateway)
		return
	}
	defer func() { _ = resp.Body.Close() }()

	// Mirror guest response headers (H2 HEADERS frame) to the
	// inbound H2C ResponseWriter. For grpc, the trailers carry
	// grpc-status / grpc-message; the inbound stdlib server
	// reads them as extra response headers (Go's http.TrailerPrefix
	// demux happens client-side, not on the bridge — the bridge
	// forwards verbatim, the gatewayd-public on the edge handles
	// trailer framing for the gRPC client). This is the load-
	// bearing invariant for grpc per ADR-126 §Decision 5.
	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	// Pre-declare the trailer keys we expect so stdlib's
	// server transport emits them as a trailer HEADERS frame
	// (END_STREAM) rather than folding them into the regular
	// response HEADERS frame. Without this declaration, the
	// TrailerPrefix entries below land on the response HEADERS,
	// which gRPC clients tolerate but the load-bearing invariant
	// for grpc per ADR-126 §Decision 5 is that trailers ride
	// trailer HEADERS. (Go stdlib strips the `Trailer:` literal
	// on H2 transport — the declaration is process-state, not
	// wire-state.)
	for k := range resp.Trailer {
		w.Header().Add("Trailer", k)
	}
	w.WriteHeader(resp.StatusCode)
	// Drain the body BEFORE writing the trailers — stdlib's H2
	// server transport needs the body fully consumed (or EOF) to
	// know that DATA frames are exhausted and a trailer HEADERS
	// frame may now be appended. If we trailer-set before the
	// io.Copy finished, the trailer HEADERS would close the
	// stream prematurely.
	_, _ = copyStreaming(w, resp.Body)
	// Now write the trailers after the body has been drained.
	for k, vs := range resp.Trailer {
		for _, v := range vs {
			w.Header().Set(http.TrailerPrefix+k, v)
		}
	}
}

// newGuestH2CTransport builds the guest-side H2C client transport
// for one inbound stream. Mirrors
// `pkg/vmmdgrpc/forward.go::newStreamBridgeH2CTransport` but the
// DialTLSContext dials the guest TCP addr (10.0.0.2:<port>) under
// an explicit ctx timeout, instead of the bridge's unix socket.
//
// x/net/http2 with AllowHTTP=true is the canonical Go client for
// H2C over a non-TLS dialer (stdlib http.Transport's ForceAttemptHTTP2
// is a no-op for plaintext).
func newGuestH2CTransport(guestIP string, guestPort uint16) *http2.Transport {
	return &http2.Transport{
		AllowHTTP: true,
		// DialTLSContext is the only dial hook x/net/http2 exposes
		// even for plaintext H2C; the tls.Config is ignored. We
		// dial the guest TCP addr under dialTimeout.
		DialTLSContext: func(ctx context.Context, _, _ string, _ *tls.Config) (net.Conn, error) {
			dctx, dcancel := context.WithTimeout(ctx, h2cDialTimeout)
			defer dcancel()
			var d net.Dialer
			return d.DialContext(dctx, "tcp", net.JoinHostPort(guestIP, strconv.FormatUint(uint64(guestPort), 10)))
		},
		// Pooling is disabled implicitly by returning a fresh
		// transport per stream (one-bridge-stream-per-guest-
		// stream per ADR-126 §Decision 4); the transport is
		// unreachable to its own pool once the closure is
		// discarded. Per-stream isolation: HPACK state lives
		// only as long as the transport is alive (one request),
		// no conn migration hazard, no cross-stream coupling.
		IdleConnTimeout: h2cIdleConnTimeout,
		ReadIdleTimeout: 30 * time.Second,
		PingTimeout:     15 * time.Second,
	}
}

// copyStreaming is io.Copy with a guarded errcheck and no deadline
// booking (the deadline ctx controls cancellation). Identical to
// the body io.Copy at handleH1Stream; split for readability.
func copyStreaming(dst http.ResponseWriter, src interface{ Read(p []byte) (int, error) }) (int64, error) {
	// Implementation identical to io.Copy semantics for h1 vs h2c.
	// We don't pre-buffer; HTTP/2 DATA frames are buffer-bound at
	// the transport layer (default flow-control window).
	buf := make([]byte, 32*1024)
	var total int64
	for {
		n, err := src.Read(buf)
		if n > 0 {
			written, werr := dst.Write(buf[:n])
			total += int64(written)
			if werr != nil {
				return total, werr
			}
		}
		if err != nil {
			if err.Error() == "EOF" {
				return total, nil
			}
			return total, err
		}
	}
}

// hopByHopHeaders mirrors RFC 7230 §6.1's hop-by-hop registry.
// Anything in this set is dropped from the outbound guest request
// because HTTP/2 manages framing internally and we want the guest
// to see only end-to-end semantics. Comment-each for reviewer
// legibility (mirrors pkg/gateway/internal_proxy.go:142-178).
var hopByHopHeaders = map[string]struct{}{
	"Connection":          {},
	"Keep-Alive":          {},
	"Proxy-Authenticate":  {},
	"Proxy-Authorization": {},
	"Te":                  {}, // RFC 7540 §8.1.2.2: TE only valid with trailers=trailers
	"Trailer":             {},
	"Transfer-Encoding":   {}, // HTTP/2 frames manage framing
	"Upgrade":             {}, // prior-knowledge skips Upgrade dance
}

func isHopByHopHeader(k string) bool {
	_, ok := hopByHopHeaders[http.CanonicalHeaderKey(k)]
	return ok
}
