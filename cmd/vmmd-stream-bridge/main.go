// Command vmmd-stream-bridge (issue #686) is the inner-leg H2C
// streaming bridge for the gatewayd → vmmd → guest path.
//
// The legacy streaming bridge (buildStreamingBridgeScript in
// pkg/vmmdgrpc/forward.go:950) shell-scripts a `/dev/tcp` dial and
// hard-codes HTTP/1.1 on the wire (forward.go:960). With the
// gatewayd-public → gatewayd-internal hop now running H2C
// (ADR-070 + PR #713/#719), the inner leg is the only plaintext
// H1 hop in the chain. vmmd-stream-bridge replaces the shell
// bridge with a small Go binary that:
//
//  1. Listens on a unix socket inside the jailer tmpfs
//     (default /srv/fc/jail/<uid>/stream.sock).
//  2. Speaks H2C on that socket (cleartext HTTP/2, no TLS).
//  3. Opens a plain HTTP/1.1 connection to the guest at 10.0.0.2:<port>
//     and proxies frames bidirectionally.
//
// This binary is OFF by default — the v1 codepath in pkg/vmmdgrpc/
// forward.go keeps the shell bridge in place. A future PR flips the
// FAAS_STREAM_BRIDGE const to v2 once the metal streaming test
// confirms H2C framing end-to-end on Lima nested KVM. Keeping the
// v1 default means the binary can ship, be unit-tested, and be
// staged into the jailer chroot today without touching production
// traffic.
//
// Args (positional, no flags):
//
//	argv[1] = bind unix socket path
//	          (e.g. /srv/fc/jail/20001/stream.sock)
//	argv[2] = guest IP (e.g. 10.0.0.2)
//	argv[3] = guest TCP port
//	argv[4] = session deadline (RFC3339 or duration like "24h")
//
// Wire protocol:
//
//   - server side: H2C (cleartext HTTP/2) per RFC 7540 / 9113
//   - client side: plain HTTP/1.1 to the guest (the legacy inner
//     contract; the guest doesn't speak H2C and doesn't need to)
//   - bidi byte-copy via http2.Server with AllowHTTP=true
//
// Failure modes (exit code != 0):
//
//	2 = usage error (bad argv, bad deadline)
//	3 = bind failure on the unix socket
//	4 = accept loop fatal
//
// See docs/adr/028-gatewayd-remote-routing.md (draft ADR-028) for
// the architectural context; the bridge is the artifact that
// closes issue #686.
package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"
)

// dialTimeout caps the inner-leg TCP dial against the guest.
// Past 30s the gateway has already-abandoned the wake.
const dialTimeout = 30 * time.Second

// readHeaderTimeout caps how long we wait for the guest's first
// response byte after the H2C request lands.
const readHeaderTimeout = 30 * time.Second

// defaultSessionDeadline is the wall-clock ceiling for a streaming
// session; matches rawStreamSessionDeadline in pkg/gateway/forwardproxy.go.
const defaultSessionDeadline = 24 * time.Hour

func main() {
	if len(os.Args) != 5 {
		fmt.Fprintf(os.Stderr, "usage: vmmd-stream-bridge <bind-unix-sock> <guest-ip> <guest-port> <deadline>\n")
		os.Exit(2)
	}
	bind := os.Args[1]
	guestIP := os.Args[2]
	port, err := strconv.ParseUint(os.Args[3], 10, 16)
	if err != nil {
		fmt.Fprintf(os.Stderr, "invalid port %q: %v\n", os.Args[3], err)
		os.Exit(2)
	}
	deadline, err := parseDeadline(os.Args[4])
	if err != nil {
		fmt.Fprintf(os.Stderr, "invalid deadline %q: %v\n", os.Args[4], err)
		os.Exit(2)
	}

	// Remove any stale socket from a previous crashed run; the
	// jailer tmpfs is process-scoped so this is local to the
	// current instance.
	if err := os.Remove(bind); err != nil && !errors.Is(err, os.ErrNotExist) {
		fmt.Fprintf(os.Stderr, "remove stale socket %s: %v\n", bind, err)
		os.Exit(2)
	}
	ln, err := net.Listen("unix", bind)
	if err != nil {
		fmt.Fprintf(os.Stderr, "listen %s: %v\n", bind, err)
		os.Exit(3)
	}
	defer func() { _ = ln.Close() }()

	// chmod 0600 — only vmmd (and the jailer user) can dial this
	// socket. Per spec §11 the jailer tmpfs is mode 0700 so this is
	// belt-and-braces, but the explicit chmod is the source of
	// truth in the manpage sense.
	if err := os.Chmod(bind, 0o600); err != nil {
		fmt.Fprintf(os.Stderr, "chmod %s: %v\n", bind, err)
		os.Exit(3)
	}

	srv := &http.Server{
		Handler: newHandler(guestIP, uint16(port), deadline),
		// ReadHeaderTimeout is the H2C connection preface budget;
		// 10s is generous on a same-host unix socket.
		ReadHeaderTimeout: 10 * time.Second,
	}
	// Enable cleartext HTTP/2 (H2C) on this listener via the stdlib
	// Protocols API (Go 1.24+). Replaces the deprecated
	// golang.org/x/net/http2/h2c.NewHandler wrapper; the vmmd client
	// now sets its transport to AllowHTTP=true and dials the unix
	// socket directly.
	srv.Protocols.SetUnencryptedHTTP2(true)

	// SIGTERM/SIGINT → graceful shutdown. vmmd sends SIGTERM after
	// the gRPC ForwardHTTPStream returns; the bridge exits cleanly
	// without truncating an in-flight stream.
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()

	errc := make(chan error, 1)
	go func() {
		err := srv.Serve(ln)
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			errc <- err
			return
		}
		errc <- nil
	}()

	select {
	case <-ctx.Done():
		shutCtx, shutCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer shutCancel()
		_ = srv.Shutdown(shutCtx)
		<-errc
	case err := <-errc:
		if err != nil {
			fmt.Fprintf(os.Stderr, "serve: %v\n", err)
			os.Exit(4)
		}
	}
}

// newHandler builds the H2C handler that proxies a single H2C
// stream to the guest at <guestIP>:<port>. The handler is single-
// use per connection: each inbound request opens a fresh dial
// against the guest, copies bidirectionally, and tears down.
//
// We do NOT keep a long-lived guest conn per H2C stream. The
// guest at 10.0.0.2:<port> is HTTP/1.1 and a long-lived conn
// would have to serialize requests through it; the simpler shape
// is "one H2C request = one guest dial." A future optimisation
// (HTTP/1.1 keep-alive multiplexing on the guest side) is out of
// scope for the cutover.
func newHandler(guestIP string, guestPort uint16, deadline time.Time) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithDeadline(r.Context(), deadline)
		defer cancel()

		d := net.Dialer{Timeout: dialTimeout}
		conn, err := d.DialContext(ctx, "tcp", net.JoinHostPort(guestIP, strconv.FormatUint(uint64(guestPort), 10)))
		if err != nil {
			http.Error(w, fmt.Sprintf("dial guest: %v", err), http.StatusBadGateway)
			return
		}
		defer func() { _ = conn.Close() }()

		// Set a per-stream read deadline so a wedged guest does not
		// hold the H2C stream open past the session budget. Use the
		// larger of readHeaderTimeout and the remaining deadline.
		if remaining := time.Until(deadline); remaining > readHeaderTimeout {
			_ = conn.SetReadDeadline(time.Now().Add(readHeaderTimeout))
		} else {
			_ = conn.SetReadDeadline(deadline)
		}

		// Bridge request body → guest.
		go func() {
			defer func() { _ = conn.Close() }()
			_, _ = io.Copy(conn, r.Body)
		}()

		// Bridge guest → response writer.
		// Status code comes back from the guest as a parsed
		// response; net/http's ReadResponse handles the framing.
		// Use a bufio.Reader on the conn to allow re-reading the
		// first line.
		br := newBufioReader(conn)
		resp, err := http.ReadResponse(br, r)
		if err != nil {
			http.Error(w, fmt.Sprintf("read guest response: %v", err), http.StatusBadGateway)
			return
		}
		defer func() { _ = resp.Body.Close() }()

		// Mirror guest response headers back to the H2C caller.
		for k, vs := range resp.Header {
			for _, v := range vs {
				w.Header().Add(k, v)
			}
		}
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
	})
}

// newBufioReader is a tiny indirection so future tuning of the
// buffer size (currently stdlib default 4096) is one edit. The
// guest at 10.0.0.2:<port> typically returns <4 KiB headers and
// small bodies per request; stdlib default is fine.
func newBufioReader(r io.Reader) *bufio.Reader {
	return bufio.NewReader(r)
}

// parseDeadline accepts either an RFC3339 timestamp or a Go
// duration string ("24h", "30m"). An empty string falls back to
// defaultSessionDeadline-from-now.
func parseDeadline(s string) (time.Time, error) {
	if s == "" {
		return time.Now().Add(defaultSessionDeadline), nil
	}
	if d, err := time.ParseDuration(s); err == nil {
		return time.Now().Add(d), nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	return time.Time{}, fmt.Errorf("not a duration or RFC3339 timestamp")
}
