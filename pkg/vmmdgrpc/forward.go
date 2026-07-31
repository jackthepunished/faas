// Issue #98 / ADR-028: vmmd's HTTP bridge into per-instance netns.
//
// gatewayd's hot path speaks HTTP to vmmd over the Tailscale/Wireguard
// overlay (no second transport — pkg/wire.DialContext does TCP+overlay+mTLS
// already, issue #95). vmmd receives the request, nsenter's the
// per-instance netns, dials netns.GuestIP:netns.AppPort on the inner side,
// reads the response, and returns it as a ForwardHTTPResponse envelope.
//
// Why gRPC-bridged netns forwarding instead of a per-instance HTTP listener
// bound on the host side: the latter would need one new TCP port per live
// instance (range-allocator + nft publish per Wake + scan-free collision
// detection) AND a second dial leg on the gateway side. This design keeps
// vmmd's listener count flat at one — ForwardHTTP is one unary RPC — and
// inherits all the auth + overlay configuration from the existing vmmd
// gRPC server.
//
// Why nsenter rather than bind-mounting the per-instance netfs into vmmd's
// process namespace: nsenter is exactly the resume-hook pattern ADR-022
// already uses (guest-init) and it stays inside the kernel's namespace
// boundary so the per-instance nftables ruleset (forward chain, egress
// deny list, per-plan tc cap) keeps policing traffic exactly as if the
// guest were talking to a local caller. The bridge only translates the
// transport; it does NOT widen the egress policy.
//
// Failure → gRPC status:
//   - Unknown instance → NotFound (the gateway will re-wake on the next
//     request and the placement engine will pick a fresh node).
//   - nsenter failure (netns gone, kernel EACCES) → Internal. nsenter can
//     only fail on a real kernel bug; logging is enough.
//   - guest dial refused / read timeout → Unavailable. The next gateway
//     retry should re-wake; surfacing Unavailable is what tells the
//     gateway "this node is sick, drop the cached target".
//
// All caps live as exported package-level constants so the proto file's
// inline docstring is the only place they have to be repeated.

package vmmdgrpc

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	vmmdpb "github.com/onebox-faas/faas/api/proto/onebox/faas/vmmd/v1"
	"github.com/onebox-faas/faas/pkg/netns"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// ForwardMaxBodyBytes is the per-request body cap. 25 MiB matches the
// gatewayd upstream cap (spec §13 / pkg/api.Limits.HTTPRequestMax) so a
// request the gateway accepts cannot be rejected here for size.
const ForwardMaxBodyBytes = 25 * 1024 * 1024

// forwardDialTimeout caps the guest-side dial. Cold-boot a guest that's
// still handshaking its TLS server can be slow; 5s is long enough that a
// healthy cold-boot answers on the first attempt. If the guest is hung
// past 5s the request is Unavailable to the caller and the gateway will
// retry the wake on the next hop.
const forwardDialTimeout = 5 * time.Second

// forwardResponseTimeout caps the guest-side response read. The guest
// app serves a request the gateway already validated against spec §13
// (no chunked uploads, no streaming responses) — 60s is the same budget
// spec §6.1 gives cold boots, generous for app code that does a single
// blocking downstream call.
const forwardResponseTimeout = 60 * time.Second

// ForwardStreamMaxBodyBytes is the per-request body cap lifted on the
// streaming path (ADR-047 PR-B + PR-C). Mirrors the
// Hobby/Pro/Scale 100 MB cap in pkg/api.MaxResponseBodyBytes. The
// unary ForwardHTTP stays at ForwardMaxBodyBytes (25 MiB) so a
// misrouted streaming request through the legacy path still hits the
// smaller cap.
const ForwardStreamMaxBodyBytes = 100 * 1024 * 1024

// ForwardStreamResponseTimeout is the bridge-side response timeout
// lifted on the streaming path (ADR-047 PR-B + PR-C). Mirrors the
// Hobby/Pro/Scale ResponseWriteTimeout (15 min / 900 s) so an LLM
// stream that takes 30 s end-to-end fits comfortably inside the
// window. The unary path stays at forwardResponseTimeout (60 s).
const ForwardStreamResponseTimeout = 900 * time.Second

// ForwardHTTP bridges one HTTP request from gatewayd into the per-instance
// netns and dials netns.GuestIP:netns.AppPort (ADR-009 invariant: every
// guest sees the identical inner world, so this address is the same for
// every instance). The bridge is a process boundary cross via nsenter
// (one syscall); bytes flow in cleartext in the kernel's namespace table,
// then over loopback to the guest's user-mode listener.
func (s *Server) ForwardHTTP(ctx context.Context, req *vmmdpb.ForwardHTTPRequest) (*vmmdpb.ForwardHTTPResponse, error) {
	const op = "ForwardHTTP"
	start := time.Now()
	defer func() {
		// No-op on success too; ops is nil-safe via Observe.
		s.ops.Observe(op, time.Since(start), nil)
	}()

	if req.GetInstance() == "" {
		return nil, status.Error(codes.InvalidArgument, "instance is required")
	}
	if len(req.GetBody()) > ForwardMaxBodyBytes {
		return nil, status.Errorf(codes.InvalidArgument,
			"body exceeds %d bytes", ForwardMaxBodyBytes)
	}

	netnsName, ok := s.vmm.NetnsFor(req.GetInstance())
	if !ok {
		// Unknown instance — not parked-but-live, not yet woken. Gateway
		// will re-wake; surfacing NotFound keeps the gateway's error
		// mapping clean.
		return nil, status.Errorf(codes.NotFound, "instance %q not live", req.GetInstance())
	}

	resp, err := s.bridgeIntoNetns(ctx, netnsName, req)
	if err != nil {
		s.log.Error("vmmd: ForwardHTTP bridge failed",
			"instance", req.GetInstance(),
			"netns", netnsName,
			"err", err.Error())
		return nil, err
	}
	return resp, nil
}

// ForwardHTTPStream (issue #471 PR-B + PR-C / ADR-047) is the
// bidi-streaming variant of ForwardHTTP. Wire shape:
//
//	client → server: 1× ForwardHTTPRequestInit, then N× body_chunk
//	                 (where the chunks stream in as r.Body is read
//	                 by the gateway's forwardproxy)
//	server → client: 1× ForwardHTTPResponseInit (status + headers),
//	                 then N× body_chunk (as they arrive from the
//	                 bridge script's response loop)
//
// On the streaming path the bridge script pipes the request body
// from a named FIFO (or stdin), reads the response status+headers
// + body in a streaming loop, and the Go-side server shuttles
// each chunk to the gRPC client. The bridge itself is unchanged
// in shape — only the body plumbing differs (chunked reads on
// stdout instead of cat slurp).
//
// Why bidi instead of server-streaming-only: a streaming response
// is often paired with a streaming request body (an SSE handler
// consuming a client feed). Bidirectional streaming keeps the
// protocol symmetric; client retry semantics are unchanged (the
// request is still scoped to a single bidi stream).
//
// The unary ForwardHTTP stays for one cycle as a deprecated path
// so a rolling deploy across the vmmd fleet doesn't break older
// gatewayd builds. PR-D removes it.
func (s *Server) ForwardHTTPStream(stream grpc.BidiStreamingServer[vmmdpb.ForwardHTTPStreamRequest, vmmdpb.ForwardHTTPStreamResponse]) error {
	const op = "ForwardHTTPStream"
	start := time.Now()
	defer func() { s.ops.Observe(op, time.Since(start), nil) }()

	ctx := stream.Context()

	// 1. Receive the init frame. The bidi protocol is:
	//    [init] [body_chunk]…  on the inbound side; the server
	//    treats everything before the first init as a protocol
	//    error (the gateway always sends init first).
	init, err := stream.Recv()
	if err != nil {
		return status.Errorf(codes.InvalidArgument, "expected init frame: %v", err)
	}
	reqInit := init.GetInit()
	if reqInit == nil {
		return status.Error(codes.InvalidArgument, "first frame must be ForwardHTTPRequestInit")
	}
	if reqInit.GetInstance() == "" {
		return status.Error(codes.InvalidArgument, "instance is required")
	}
	// Cap-lift (ADR-047): when the gateway stamped stream=true
	// (set by setupStreamingWriter at the gateway side), the
	// body cap lifts from 25 MiB to 100 MiB and the bridge
	// timeout lifts from 60 s to 900 s. The cap is checked
	// per-chunk in the recv loop below (so a streaming request
	// with a tiny body never trips the higher cap).
	maxBody := int64(ForwardMaxBodyBytes)
	respTimeout := forwardResponseTimeout
	if reqInit.GetStream() {
		maxBody = int64(ForwardStreamMaxBodyBytes)
		respTimeout = ForwardStreamResponseTimeout
	}

	netnsName, ok := s.vmm.NetnsFor(reqInit.GetInstance())
	if !ok {
		return status.Errorf(codes.NotFound, "instance %q not live", reqInit.GetInstance())
	}

	// 2. Bridge the request body via a temp file (so the shell
	//    script can `cat` it without colliding stdin with the
	//    response read) AND a streaming pipe so the body can
	//    grow as chunks arrive. The simplest correct shape: stage
	//    init headers + dial line in the script as today, but
	//    defer the body bytes to streaming reads. The bridge
	//    script takes the body from stdin in chunks.
	//
	//    Architecture: parent goroutine reads from the gRPC
	//    stream and writes body chunks to a pipe; the bridge
	//    script's stdin is the pipe's read end. When the parent
	//    goroutine sees io.EOF on the stream (gateway signaled
	//    end-of-body), it closes the pipe; the bridge reads EOF
	//    from cat, finishes its response write, and exits.
	stdinR, stdinW, err := os.Pipe()
	if err != nil {
		return status.Errorf(codes.Internal, "pipe: %v", err)
	}
	defer func() { _ = stdinR.Close() }()
	// stdinW.Close is idempotent and a no-op after the first close
	// (the body goroutine below closes it once the gRPC stream
	// signals end-of-body; the deferred close here is the
	// belt-and-suspenders for the cmd.Start error path and any
	// early returns). The defer also drains any pending writes
	// from the body goroutine before closing.
	defer func() { _ = stdinW.Close() }()

	// 3. Spawn the bridge. Bridge stdout pipes to the server
	//    goroutine below via an os.Pipe (NOT a buffer) so the
	//    server can stream the response body chunks out the
	//    gRPC bidi stream as the bridge writes them — the
	//    buffering path defeats the streaming purpose. Stderr
	//    is captured for the Unavailable surfacing path on
	//    bridge failure.
	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		return status.Errorf(codes.Internal, "stdout pipe: %v", err)
	}
	defer func() { _ = stdoutR.Close() }()

	cmd := exec.CommandContext(ctx, "ip", "netns", "exec", netnsName, "sh", "-c",
		buildStreamingBridgeScript(reqInit, respTimeout))
	cmd.Stdin = stdinR
	cmd.Stdout = stdoutW
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Start(); err != nil {
		_ = stdoutW.Close()
		return status.Errorf(codes.Unavailable, "bridge start: %v", err)
	}
	// The bridge owns the write end of stdout; the reader
	// goroutine below owns the read end. Closing stdoutW
	// after cmd.Wait ensures the pipe reader sees EOF.
	stdoutReaderClosed := false
	defer func() {
		if !stdoutReaderClosed {
			_ = stdoutR.Close()
		}
	}()

	// Body-copy goroutine: copies client body_chunks → bridge
	// stdin. Errors are aggregated; the first one cancels the
	// bidi stream via the cancelErr closure capture. The
	// goroutine owns the stdinW writer and closes it on exit —
	// this is the EOF signal that lets the bridge script's
	// stdin read loop return and the process exit. Closing it
	// before reporting via bodyErrCh would race the server's
	// post-loop read; closing AFTER the channel send is safe
	// because the channel is buffered (size 1) and the server
	// only reads from bodyErrCh after the goroutine has
	// returned. (Issue #471 review F1 fix.)
	bodyErrCh := make(chan error, 1)
	go func() {
		var written int64
		// close stdinW exactly once at exit, regardless of
		// which branch returned.
		defer func() { _ = stdinW.Close() }()
		for {
			f, err := stream.Recv()
			if errors.Is(err, io.EOF) {
				bodyErrCh <- nil
				return
			}
			if err != nil {
				bodyErrCh <- err
				return
			}
			chunk := f.GetBodyChunk()
			if len(chunk) == 0 {
				continue
			}
			written += int64(len(chunk))
			if written > maxBody {
				bodyErrCh <- status.Errorf(codes.InvalidArgument,
					"streaming body exceeds %d bytes", maxBody)
				return
			}
			if _, err := stdinW.Write(chunk); err != nil {
				bodyErrCh <- err
				return
			}
		}
	}()

	// 4. Stdout reader goroutine: reads headers from the bridge
	//    stdout pipe, parses them via parseBridgeOutput, sends
	//    the init frame on the gRPC stream, then streams the
	//    body bytes as ForwardHTTPStreamResponse_BodyChunk
	//    frames. The body stream is chunked-decoded on the fly
	//    via httputil.NewChunkedReader when the parsed
	//    Transfer-Encoding header indicates chunked encoding
	//    (issue #471 review F2 fix; the prior buffered +
	//    bytes.Buffer path forwarded raw chunked-encoded bytes
	//    including chunk-size lines to the client).
	//
	//    The reader owns the read end of stdoutR; the bridge
	//    process owns the write end. The reader sees EOF when
	//    the bridge closes its stdout (i.e. when the bridge
	//    process exits after `cat <&3` returns).
	streamErrCh := make(chan error, 1)
	go func() {
		defer close(streamErrCh)
		br := bufio.NewReader(stdoutR)
		// Read until the header/body separator (\n\n). Cap
		// the header read at 64 KiB so a malformed guest
		// that never sends the separator can't OOM the
		// server; in practice, HTTP/1.1 headers are <8 KiB.
		var headBuf bytes.Buffer
		for {
			line, err := br.ReadString('\n')
			if err != nil && line == "" {
				streamErrCh <- status.Errorf(codes.Internal, "read bridge headers: %v", err)
				return
			}
			headBuf.WriteString(line)
			if line == "\n" {
				break
			}
			if headBuf.Len() > 64*1024 {
				streamErrCh <- status.Error(codes.ResourceExhausted, "bridge headers exceed 64 KiB")
				return
			}
		}
		resp, err := parseBridgeOutput(headBuf.Bytes())
		if err != nil {
			streamErrCh <- status.Errorf(codes.Internal, "parse bridge headers: %v", err)
			return
		}

		// Send the init frame first (status + headers, no body yet).
		if err := stream.Send(&vmmdpb.ForwardHTTPStreamResponse{
			Frame: &vmmdpb.ForwardHTTPStreamResponse_Init{
				Init: &vmmdpb.ForwardHTTPResponseInit{
					Status:  resp.GetStatus(),
					Headers: resp.GetHeaders(),
				},
			},
		}); err != nil {
			streamErrCh <- status.Errorf(codes.Internal, "send init: %v", err)
			return
		}

		// Wrap the body stream in a chunked decoder if the
		// guest emitted Transfer-Encoding: chunked. The
		// decoder consumes the chunked framing (size-line,
		// body bytes, CRLF terminator per chunk) and emits
		// the decoded payload as the next Read result;
		// io.EOF signals end-of-body.
		var bodySrc io.Reader = br
		if responseIsChunked(resp.GetHeaders()) {
			bodySrc = httputil.NewChunkedReader(br)
		}

		// Stream the body in 8 KiB chunks. The chunk size
		// matches the bridge's per-read `cat <&3` granularity
		// at the byte level — chunks emerge from the bridge
		// as they arrive, the gateway's statusRecorder.Write
		// triggers maybeFlush on the 256 KiB / 200 ms
		// boundary, and the per-flush tx_bytes increments
		// attribute every egress byte to
		// (instance_id, current minute).
		buf := make([]byte, 8*1024)
		for {
			n, rerr := bodySrc.Read(buf)
			if n > 0 {
				if serr := stream.Send(&vmmdpb.ForwardHTTPStreamResponse{
					Frame: &vmmdpb.ForwardHTTPStreamResponse_BodyChunk{
						BodyChunk: append([]byte(nil), buf[:n]...),
					},
				}); serr != nil {
					streamErrCh <- status.Errorf(codes.Internal, "send body chunk: %v", serr)
					return
				}
			}
			if errors.Is(rerr, io.EOF) {
				streamErrCh <- nil
				return
			}
			if rerr != nil {
				streamErrCh <- status.Errorf(codes.Internal, "read body: %v", rerr)
				return
			}
		}
	}()

	// Drain the body-copy goroutine BEFORE waiting on the bridge.
	// The bridge's stdin read loop only returns EOF when stdinW
	// is closed, and the goroutine closes stdinW on exit (above).
	// Reading from bodyErrCh first forces that sequence:
	//   1. stream.Recv returns io.EOF on the body goroutine
	//   2. body goroutine sends nil to bodyErrCh + closes stdinW
	//   3. bridge stdin read returns EOF → bridge exits
	//   4. cmd.Wait returns promptly
	// (Issue #471 review F1 fix; the prior ordering
	// cmd.Wait → stdinW.Close deadlocked every well-formed
	// streaming request until the 900 s exec.CommandContext
	// killed the bridge.)
	bodyErr := <-bodyErrCh
	bridgeErr := cmd.Wait()
	// Close stdoutW now that the bridge has exited; the reader
	// goroutine will see EOF and exit cleanly.
	_ = stdoutW.Close()
	streamErr := <-streamErrCh
	stdoutReaderClosed = true
	_ = stdoutR.Close()

	if bridgeErr != nil {
		var exitErr *exec.ExitError
		if errors.As(bridgeErr, &exitErr) {
			s.log.Warn("vmmd: streaming bridge non-zero exit",
				"instance", reqInit.GetInstance(),
				"netns", netnsName,
				"exit_code", exitErr.ExitCode(),
				"stderr", stderr.String())
			return status.Errorf(codes.Unavailable,
				"guest unreachable (exit %d): %s",
				exitErr.ExitCode(), strings.TrimSpace(stderr.String()))
		}
		return status.Errorf(codes.Unavailable, "bridge exec: %v", bridgeErr)
	}
	if bodyErr != nil && !errors.Is(bodyErr, io.EOF) {
		return status.Errorf(codes.InvalidArgument, "body stream: %v", bodyErr)
	}
	if streamErr != nil {
		return streamErr
	}
	return nil
}

// bridgeIntoNetns runs `ip netns exec <netns> sh -c '<script>'` via
// nsenter, capturing stdout into resp.Body and the script's exit status
// into the gRPC status. The shell script is the simplest correct
// implementation: it dials the inner IP, writes the request, reads the
// response. Doing this in Go inside the netns would require a separate
// binary that calls setns() before net.Dial — netns.NewConfig is the
// kernel-side realisation of that pattern, but reusing the existing
// `ip netns exec` makes the bridge a single argv and inherits every
// quirk of how ip netns exec handles EACCES, ENOENT, etc.
//
// Why script and not just raw net.Dial after setns: the process can be
// vmmd's main goroutine; setns() from a multi-threaded process is
// unreliable (see `man 2 setns` — "calling setns() from a multi-threaded
// process ... is unsupported"). Forking via ip netns exec sidesteps that
// entirely; vmmd's main thread keeps running, and the script lives in a
// single-threaded child.
//
// The script format is deterministic — pipe-build-and-exec, no shell
// variables from caller input. Gatewayd-injected bytes never reach the
// shell because the script reads from a tmpfile written with os.WriteFile.
func (s *Server) bridgeIntoNetns(ctx context.Context, netnsName string, req *vmmdpb.ForwardHTTPRequest) (*vmmdpb.ForwardHTTPResponse, error) {
	// Stage the request bytes in a tmpfile so the script reads them
	// without going through argv or stdin (stdin would collide with the
	// HTTP response read on a busy fd).
	tmp, err := writeTempRequest(req)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "stage request: %v", err)
	}
	defer tmp.Cleanup()

	// Cap the response read at ForwardResponseTimeout + a small grace so
	// a hung guest doesn't wedge the bridge.
	dialCtx, cancel := context.WithTimeout(ctx, forwardDialTimeout+forwardResponseTimeout+2*time.Second)
	defer cancel()

	script := buildBridgeScript(tmp.Path, req, forwardDialTimeout, forwardResponseTimeout)
	cmd := exec.CommandContext(dialCtx, "ip", "netns", "exec", netnsName, "sh", "-c", script)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		// Distinguish "guest returned non-zero HTTP status" from "guest
		// unreachable": the script writes "<status>\n<headers>\n\n<body>"
		// on success and exits 0; any other exit is a transport failure.
		// We surface Unavailable so the gateway drops the cached target
		// and re-wakes; an explicit status code on the wire would look
		// like a successful HTTP round-trip and bypass the retry path.
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			s.log.Warn("vmmd: bridge script non-zero exit",
				"netns", netnsName,
				"exit_code", exitErr.ExitCode(),
				"stderr", stderr.String())
			return nil, status.Errorf(codes.Unavailable,
				"guest unreachable (exit %d): %s",
				exitErr.ExitCode(), strings.TrimSpace(stderr.String()))
		}
		return nil, status.Errorf(codes.Unavailable, "bridge exec: %v", err)
	}

	return parseBridgeOutput(stdout.Bytes())
}

// buildBridgeScript returns the shell script that runs inside the
// per-instance netns. It uses only POSIX-portable tools (printf, cat,
// awk) that are guaranteed in the minimal nftables/iproute2 image vmmd
// already depends on. The script:
//
//  1. dials <netns.GuestIP>:<netns.AppPort> over TCP via /dev/tcp (bash)
//     — fall back to /bin/sh's /dev/tcp which is in busybox and bash;
//     if absent we use `nc` if present. /dev/tcp is the lightest path
//     and ships with busybox, so the Lima arm64 guest has it.
//  2. writes the request line, headers, blank line, body.
//  3. reads the response: status line, headers (until blank line), body.
//  4. prints a single "<status>\n<headers-blocks>\n\n<body>" record so
//     parseBridgeOutput can split on the first "\n\n" cleanly.
//
// The Host header is rewritten to netns.GuestIP:<dial-port> so the
// guest's vhost matcher (apps that pin Host) sees the inner identity.
// gatewayd already strips hop-by-hop headers (Connection, etc.) so we
// don't repeat that here — keeping the bridge dumb about HTTP semantics
// is what keeps the script auditable.
//
// dial-port resolution (issue #460 / ADR-053, PR-C): the per-deployment
// override port the customer's app binds inside the guest comes in via
// req.GetPort(). 0 = legacy 8080 (netns.AppPort default). The host's
// waitReady + DNAT stay fixed on 8080 (ADR-009 +
// guest/init/portnorm_linux.go); only the vmmd bridge uses this port to
// dial the guest.
func buildBridgeScript(reqPath string, req *vmmdpb.ForwardHTTPRequest, dialTimeout, respTimeout time.Duration) string {
	dialPort := req.GetPort()
	if dialPort == 0 {
		dialPort = uint32(netns.AppPort)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "set -eu\n")
	fmt.Fprintf(&b, "exec 3<>/dev/tcp/%s/%d\n",
		netns.GuestIP, dialPort)
	// Request line.
	fmt.Fprintf(&b, "printf '%%s %%s HTTP/1.1\\r\\n' %s %s >&3\n",
		shellQuote(req.GetMethod()), shellQuote(req.GetRequestUri()))
	// Host header (rewritten to inner identity — apps with a vhost pin
	// must see the inner addr, not the overlay one).
	fmt.Fprintf(&b, "printf 'Host: %%s\\r\\n' %s >&3\n",
		shellQuote(fmt.Sprintf("%s:%d", netns.GuestIP, dialPort)))
	fmt.Fprintf(&b, "printf 'Content-Length: %%d\\r\\n' %d >&3\n", len(req.GetBody()))
	// Caller-supplied headers (already had hop-by-hop stripped upstream).
	for _, h := range req.GetHeaders() {
		fmt.Fprintf(&b, "printf '%%s: %%s\\r\\n' %s %s >&3\n",
			shellQuote(h.GetName()), shellQuote(h.GetValue()))
	}
	fmt.Fprintf(&b, "printf '\\r\\n' >&3\n")
	// Body.
	fmt.Fprintf(&b, "cat %s >&3\n", shellQuote(reqPath))
	// Read response. status then headers until blank line, then body.
	// timeout protects against a hung guest.
	fmt.Fprintf(&b, "read -r STATUS <&3 || true\n")
	fmt.Fprintf(&b, "printf '%%s\\n' \"$STATUS\"\n")
	fmt.Fprintf(&b, "while IFS= read -r -t %d LINE <&3; do\n",
		int(respTimeout.Seconds()))
	fmt.Fprintf(&b, "  [ -z \"$LINE\" ] && break\n")
	fmt.Fprintf(&b, "  printf '%%s\\n' \"$LINE\"\n")
	fmt.Fprintf(&b, "done\n")
	fmt.Fprintf(&b, "printf '\\n'\n")
	fmt.Fprintf(&b, "cat <&3\n")
	_ = dialTimeout // reserved for a future `timeout` wrapper around the dial.
	return b.String()
}

// buildStreamingBridgeScript (issue #471 PR-B + PR-C / ADR-047)
// is the streaming counterpart to buildBridgeScript. Differences:
//
//   - Request body comes from stdin in chunks (the Go server's
//     body-copy goroutine writes each ForwardHTTPStreamRequest
//     body_chunk to the bridge's stdin). The script reads
//     stdin and chunked-encodes it to fd 3; EOF on stdin
//     terminates the body read and the bridge continues to
//     read the response.
//   - Response: status line + headers are written to stdout
//     (terminated by a blank line, mirroring the unary
//     bridge's parseBridgeOutput contract); the body bytes
//     are then streamed to stdout via a single `cat <&3` —
//     raw from the wire, including chunked framing if the
//     guest emitted `Transfer-Encoding: chunked`. The Go-side
//     ForwardHTTPStream server reads the body stream
//     incrementally via a pipe (NOT a buffer) and applies
//     `httputil.NewChunkedReader` when the parsed
//     Transfer-Encoding header indicates chunked encoding.
//     This is what lets the bridge stay shell-simple while
//     the wire-level decoding happens in Go (where binary
//     data + per-byte framing is straightforward).
//   - Host header rewrite + port-resolution logic mirror the
//     legacy script (the same per-deployment override port,
//     the same inner-IP Host).
//
// The bridge's request path still uses the chunked-encoding
// pattern (`read -r -t 1 -n 8192 CHUNK` → `<hex-len>\r\n<body>\r\n`)
// because the gateway already emits Transfer-Encoding: chunked
// to the guest; the unary bridge took the same shape.
// respTimeout is reserved for a future per-line read deadline
// inside the cat loop (today, `cat <&3` is the simpler
// streaming primitive and the total budget is enforced by
// exec.CommandContext on the Go side).
//
// (Issue #471 review F2 fix: the prior body loop
// `while IFS= read -r -t N -n 8192 CHUNK <&3` emitted raw
// chunked-encoded bytes — including chunk-size lines and
// CRLF separators — as the body, which the gateway then
// forwarded verbatim to the client. The Go-side pipe +
// httputil.NewChunkedReader is the correct fix.)
func buildStreamingBridgeScript(req *vmmdpb.ForwardHTTPRequestInit, respTimeout time.Duration) string {
	dialPort := req.GetPort()
	if dialPort == 0 {
		dialPort = uint32(netns.AppPort)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "set -eu\n")
	fmt.Fprintf(&b, "exec 3<>/dev/tcp/%s/%d\n",
		netns.GuestIP, dialPort)
	// Request line.
	fmt.Fprintf(&b, "printf '%%s %%s HTTP/1.1\\r\\n' %s %s >&3\n",
		shellQuote(req.GetMethod()), shellQuote(req.GetRequestUri()))
	// Host header (rewritten to inner identity).
	fmt.Fprintf(&b, "printf 'Host: %%s\\r\\n' %s >&3\n",
		shellQuote(fmt.Sprintf("%s:%d", netns.GuestIP, dialPort)))
	fmt.Fprintf(&b, "printf 'Transfer-Encoding: chunked\\r\\n' >&3\n")
	// Caller-supplied headers (already had hop-by-hop stripped
	// upstream). Skip Content-Length because chunked encoding
	// governs; emitting both would be a protocol violation.
	for _, h := range req.GetHeaders() {
		if strings.EqualFold(h.GetName(), "Content-Length") {
			continue
		}
		fmt.Fprintf(&b, "printf '%%s: %%s\\r\\n' %s %s >&3\n",
			shellQuote(h.GetName()), shellQuote(h.GetValue()))
	}
	fmt.Fprintf(&b, "printf '\\r\\n' >&3\n")
	// Request body: streaming read from stdin. The Go server
	// writes body_chunk bytes to stdin; EOF on stdin closes
	// the request body and the guest sees the chunked
	// terminator.
	fmt.Fprintf(&b, "while IFS= read -r -t 1 -n 8192 CHUNK; do\n")
	fmt.Fprintf(&b, "  printf '%%x\\r\\n' ${#CHUNK} >&3\n")
	fmt.Fprintf(&b, "  printf '%%s' \"$CHUNK\" >&3\n")
	fmt.Fprintf(&b, "  printf '\\r\\n' >&3\n")
	fmt.Fprintf(&b, "done\n")
	fmt.Fprintf(&b, "printf '0\\r\\n\\r\\n' >&3\n")
	// Response status + headers (terminated by a blank line).
	fmt.Fprintf(&b, "read -r STATUS <&3 || true\n")
	fmt.Fprintf(&b, "printf '%%s\\n' \"$STATUS\"\n")
	fmt.Fprintf(&b, "while IFS= read -r -t %d LINE <&3; do\n",
		int(respTimeout.Seconds()))
	fmt.Fprintf(&b, "  [ -z \"$LINE\" ] && break\n")
	fmt.Fprintf(&b, "  printf '%%s\\n' \"$LINE\"\n")
	fmt.Fprintf(&b, "done\n")
	fmt.Fprintf(&b, "printf '\\n'\n")
	// Body stream: copy raw bytes from fd 3 to stdout. The Go
	// side reads stdout via a pipe (NOT a buffer) and applies
	// chunked decoding if the parsed Transfer-Encoding header
	// indicates chunked encoding. `cat <&3` is the simplest
	// streaming primitive available in the minimal guest base
	// image (POSIX-required, ships in busybox + dash); it
	// exits when the guest closes the connection (the
	// end-of-body signal for both Content-Length and
	// chunked-encoded responses).
	fmt.Fprintf(&b, "cat <&3\n")
	return b.String()
}

// responseIsChunked reports whether the guest's response carries
// a chunked Transfer-Encoding coding (issue #471 PR-B + PR-C /
// ADR-047, F2 review fix). Per RFC 7230 §3.3.1, the
// Transfer-Encoding header value is a comma-separated list of
// codings and tokens are case-insensitive. A "chunked" coding
// anywhere in the list means the body stream needs to be
// decoded via httputil.NewChunkedReader before forwarding to
// the client — otherwise chunk-size lines and CRLF separators
// leak into the response body.
//
// The helper accepts the parsed header slice returned by
// parseBridgeOutput; nil/empty headers yield false (no chunked
// decoding, pass-through).
func responseIsChunked(headers []*vmmdpb.Header) bool {
	for _, h := range headers {
		if !strings.EqualFold(h.GetName(), "Transfer-Encoding") {
			continue
		}
		for _, tok := range strings.Split(h.GetValue(), ",") {
			if strings.EqualFold(strings.TrimSpace(tok), "chunked") {
				return true
			}
		}
	}
	return false
}

// parseBridgeOutput splits "<status>\n<header lines>\n\n<body bytes>"
// back into proto types. The script prints bytes verbatim for the body,
// so binary payloads (image/jpeg, etc.) survive.
func parseBridgeOutput(raw []byte) (*vmmdpb.ForwardHTTPResponse, error) {
	// Split on the first \n\n that marks end-of-headers.
	sep := []byte("\n\n")
	idx := bytes.Index(raw, sep)
	if idx < 0 {
		return nil, fmt.Errorf("bridge: malformed output (no header terminator)")
	}
	head, body := raw[:idx], raw[idx+len(sep):]

	lines := bytes.Split(head, []byte("\n"))
	if len(lines) == 0 {
		return nil, fmt.Errorf("bridge: empty status line")
	}
	statusLine := strings.TrimSpace(string(lines[0]))
	// "HTTP/1.1 200 OK" — take the middle token.
	parts := strings.SplitN(statusLine, " ", 3)
	if len(parts) < 2 {
		return nil, fmt.Errorf("bridge: bad status line %q", statusLine)
	}
	code, err := strconv.Atoi(parts[1])
	if err != nil {
		return nil, fmt.Errorf("bridge: bad status code %q", parts[1])
	}
	// Bound check before the int32 cast: a guest app emitting a
	// synthetic status line with a multi-digit code (e.g.
	// "HTTP/1.1 9999") would otherwise wrap to negative in proto's
	// int32 and look like an Unavailable at the gateway.
	if code < 100 || code > 599 {
		return nil, fmt.Errorf("bridge: out-of-range status code %d", code)
	}

	resp := &vmmdpb.ForwardHTTPResponse{
		Status: int32(code), //nolint:gosec // Bounded above to a valid HTTP status range.
		Body:   body,
	}
	for _, h := range lines[1:] {
		h := string(h)
		if h == "" {
			continue
		}
		colon := strings.IndexByte(h, ':')
		if colon < 0 {
			continue
		}
		name := strings.TrimSpace(h[:colon])
		value := strings.TrimSpace(h[colon+1:])
		if name == "" {
			continue
		}
		resp.Headers = append(resp.Headers, &vmmdpb.Header{Name: name, Value: value})
	}
	return resp, nil
}

// tempRequestFile is a small helper around os.WriteFile + os.Remove.
// We need a real path so the shell script can `cat` it; an anonymous
// pipe would force the script to read from stdin which then collides
// with the response read.
type tempRequestFile struct {
	Path string
}

func (t *tempRequestFile) Cleanup() {
	// Best-effort: a leaked tmp on error doesn't break anything because
	// tmpfs cleans up at reboot and vmmd's process tree owns the dir.
	_ = removeFile(t.Path)
}

// writeTempRequest writes the body bytes to a tmpfile under os.TempDir()
// and returns a handle whose Cleanup removes it. The function takes no
// override parameter today — tests stub the package-level `tempDir`
// var so the production call and the test both land on the same code
// path. Reserving a dir arg is documented as a future extension point.
func writeTempRequest(req *vmmdpb.ForwardHTTPRequest) (*tempRequestFile, error) {
	f, err := os.CreateTemp(tempDir(), "vmmd-fwd-*.body")
	if err != nil {
		return nil, fmt.Errorf("create tmp: %w", err)
	}
	path := f.Name()
	if len(req.GetBody()) > 0 {
		if _, err := f.Write(req.GetBody()); err != nil {
			_ = f.Close()
			_ = os.Remove(path)
			return nil, fmt.Errorf("write tmp body: %w", err)
		}
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(path)
		return nil, fmt.Errorf("close tmp: %w", err)
	}
	return &tempRequestFile{Path: path}, nil
}

// removeFile is split out so tests can substitute a fake without
// touching os.Remove.
var removeFile = func(path string) error { return os.Remove(path) }

// tempDir is the directory writeTempRequest creates tmpfiles in.
// Default os.TempDir(); tests inject a stub dir to avoid /tmp on
// constrained CI runners. Splitting it via a package var (not a
// parameter) lets the production call site stay zero-arg.
var tempDir = func() string { return os.TempDir() }

// shellQuote wraps s in single quotes and escapes any embedded single
// quotes. The bridge script never executes caller-controlled bytes
// through eval — the printf format strings are fixed and the only
// caller input is the %s slot — but quoting is cheap insurance.
func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}

// avoid unused imports in builds without the bridge (some test files
// import the package without using ForwardHTTP).
var (
	_ = net.IPv4zero
	_ = http.MethodGet
	_ = io.Discard
	_ = slog.Default
)

// ParseBridgeOutputForTest exposes the package's response parser so
// unit tests can drive the pure piece without standing up the full
// ForwardHTTP server (which depends on `ip netns exec` and is gated
// to //go:build metal). The signature mirrors parseBridgeOutput
// exactly; the only difference is the visibility. Returning the
// envelope as a value, not a pointer, makes the call-site contract
// obvious to a future maintainer reading the test.
func ParseBridgeOutputForTest(raw []byte) (*vmmdpb.ForwardHTTPResponse, error) {
	return parseBridgeOutput(raw)
}

// BuildBridgeScriptForTest exposes the script generator so unit tests
// can assert the dial-port + Host header rewrite (issue #460 /
// ADR-053 PR-C) without an ip netns exec. Tests grep the rendered
// script for the dial line + Host header; the strings are stable
// (see forward.go buildBridgeScript doc).
func BuildBridgeScriptForTest(reqPath string, req *vmmdpb.ForwardHTTPRequest, dialTimeout, respTimeout time.Duration) string {
	return buildBridgeScript(reqPath, req, dialTimeout, respTimeout)
}
