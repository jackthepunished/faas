// Issue #98 / ADR-028: vmmd's HTTP bridge into per-instance netns.
//
// gatewayd's hot path speaks HTTP to vmmd over the Tailscale/Wireguard
// overlay (no second transport — pkg/wire.DialContext does TCP+overlay+mTLS
// already, issue #95). vmmd receives the request, nsenter's the
// per-instance netns, dials netns.GuestIP:netns.AppPort on the inner side,
// streams the response back as a bidi gRPC stream, and exits.
//
// Why gRPC-bridged netns forwarding instead of a per-instance HTTP listener
// bound on the host side: the latter would need one new TCP port per live
// instance (range-allocator + nft publish per Wake + scan-free collision
// detection) AND a second dial leg on the gateway side. This design keeps
// vmmd's listener count flat at one — ForwardHTTPStream is the ONLY RPC
// surface today — and inherits all the auth + overlay configuration from
// the existing vmmd gRPC server.
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
//
// PR-D / ADR-047: the pre-PR-D legacy unary FetchHTTP RPC was removed —
// streaming is the only bridge today.

package vmmdgrpc

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
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

// ForwardStreamMaxBodyBytes is the per-request body cap on the
// streaming path (ADR-047 PR-B + PR-C, PR-D). Mirrors the
// Hobby/Pro/Scale 100 MB cap in pkg/api.MaxResponseBodyBytes. The
// pre-PR-D legacy unary path was capped at 25 MiB; PR-D removes
// that path so this is the only body cap.
const ForwardStreamMaxBodyBytes = 100 * 1024 * 1024

// ForwardStreamResponseTimeout is the bridge-side response timeout
// on the streaming path (ADR-047 PR-B + PR-C, PR-D). Mirrors the
// Hobby/Pro/Scale ResponseWriteTimeout (15 min / 900 s) so an LLM
// stream that takes 30 s end-to-end fits comfortably inside the
// window.
const ForwardStreamResponseTimeout = 900 * time.Second

// ForwardHTTPStream (issue #471 PR-B + PR-C / ADR-047) is the
// bidi bridge the gatewayd hot path uses for every request. Wire
// shape:
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
// The pre-PR-D legacy unary RPC was removed in PR-D — the
// streaming RPC is the only bridge today.
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
	// PR-B (issue #462): in-flight request accounting on the
	// streaming bridge. Begin as soon as the init frame is
	// validated; End runs via defer after the bridge returns
	// or errors. The pair is request-count, not
	// connection-count — the counter captures concurrent
	// ForwardHTTPStream RPCs in flight on vmmd.
	//
	// Streaming-trap note (pkg/fcvm/activity/doc.go "Future
	// work"): the Begin/End pair runs over the whole RPC,
	// not strictly per Recv/Send cycle. A streaming RPC that
	// idles 900 s on the bridge would still hold a gauge
	// tick — this is acceptable as an upper bound on
	// concurrency; a stricter per-chunk accounting would
	// re-enter Begin/End inside the body goroutine and add
	// lock contention for marginal signal value.
	s.beginActivity(reqInit.GetInstance())
	defer s.endActivity(reqInit.GetInstance())
	// Cap-lift (ADR-047 PR-B + PR-C, PR-D finalization): the
	// streaming RPC is the only bridge today, so the base
	// is the streaming cap (100 MiB / 900 s). The legacy
	// unary path that used 25 MiB / 60 s was removed in PR-D.
	// `stream` is still read off the init frame for
	// forward-compatibility with the now-removed unary
	// bridge — a future RPC could re-introduce a smaller
	// canned response for cached HTML pages, but the wire
	// shape today inheres in the streaming envelope.
	maxBody := int64(ForwardStreamMaxBodyBytes)
	respTimeout := ForwardStreamResponseTimeout
	if !reqInit.GetStream() {
		// A non-streaming init frame on the streaming RPC
		// (the legacy `stream=true` field was unimplemented in
		// some early bridge scripts) is treated as the
		// smaller cap. Today gatewayd always sets stream=true
		// (handler.go:setupStreamingWriter stamps the header),
		// so this branch is unreachable from production
		// gatewayd but the guard is cheap insurance.
		maxBody = 25 * 1024 * 1024
		respTimeout = 60 * time.Second
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
					Status:  resp.Status,
					Headers: resp.Headers,
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
		if responseIsChunked(resp.Headers) {
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

// ForwardRawStreamMaxRequestBytes is the default inbound cap on the
// raw-bytes bridge (issue #676 / ADR-080). Mirrors
// ForwardStreamMaxBodyBytes (100 MiB) since the same plan gates
// apply. The init frame's max_request_bytes overrides this when
// non-zero; a zero cap here means "use the default".
const ForwardRawStreamMaxRequestBytes = 100 * 1024 * 1024

// vmmdRawBridgePath is the install path of the cmd/vmmd-raw-bridge
// Go binary. Mirrors the deployment convention of vmmd itself
// (/opt/faas/current/bin/vmmd-raw-bridge). The handler prefers
// this path; a fallback to a relative "vmmd-raw-bridge" lets the
// test suite run via $PATH without installing the binary.
const vmmdRawBridgePath = "/opt/faas/current/bin/vmmd-raw-bridge"

// ForwardRawStream (issue #676 / ADR-080) is the raw-bytes bridge
// for Upgrade (WebSocket / h2c / MQTT-over-WS / long-poll) traffic.
// The legacy ForwardHTTPStream strips Upgrade + Connection as
// hop-by-hop headers and the shell-script bridge hard-codes
// Transfer-Encoding: chunked + a Host rewrite — both destroy the
// raw bytes an Upgrade handshake needs. ForwardRawStream carries
// the customer's verbatim HTTP request bytes (status line + headers
// + body) into the guest's netns TCP socket and reads back the
// raw response — no HTTP parsing, no chunked framing, no header
// rewriting by vmmd.
//
// Wire shape (mirrors ForwardHTTPStream):
//
//	client → server: 1× ForwardRawRequestInit (instance, port, max
//	                 request bytes), then N× body_chunk bytes that
//	                 the bridge writes verbatim to the guest TCP
//	                 socket. Half-close = EOF.
//	server → client: 1× ForwardRawResponseInit (status + headers
//	                 + error message), then N× body_chunk bytes
//	                 that the bridge read off the guest TCP
//	                 socket. Server half-closes when the guest
//	                 closes the connection.
//
// The bridge that owns the guest TCP socket is the new
// vmmd-raw-bridge Go binary (cmd/vmmd-raw-bridge/), spawned by
// vmmd under the stream context via `ip netns exec <netns>
// vmmd-raw-bridge <ip> <port>`. The Go binary replaces the bash
// /dev/tcp pattern with explicit Go netns entry + net.Dial + a
// framing protocol that sends the HTTP response head first
// (status line + headers + blank line) and then raw body bytes —
// parseBridgeOutput consumes the head the same way the existing
// shell bridge does. The bridge spawn is identical in shape to
// the existing ForwardHTTPStream handler (cmd + os.Pipe + exec),
// so the cancellation, body-goroutine, and endActivity plumbing
// are line-for-line the same and stripped nothing from the
// existing RPC.
//
// Errors map the same way as ForwardHTTPStream: NotFound for
// unknown instance, Internal for nsenter / bridge crash,
// Unavailable for guest dial refused. The raw RPC is the
// durable Upgrade path; the legacy ForwardHTTPStream is the
// durable plain-HTTP path. Both RPCs coexist per ADR-016.
func (s *Server) ForwardRawStream(stream grpc.BidiStreamingServer[vmmdpb.ForwardRawRequest, vmmdpb.ForwardRawResponse]) error {
	const op = "ForwardRawStream"
	start := time.Now()
	defer func() { s.ops.Observe(op, time.Since(start), nil) }()

	// Receive the init frame and resolve the netns + dial port
	// (single-frame rule, same contract as ForwardHTTPStream at
	// line 110-124).
	init, err := stream.Recv()
	if err != nil {
		return status.Errorf(codes.InvalidArgument, "expected init frame: %v", err)
	}
	reqInit := init.GetInit()
	if reqInit == nil {
		return status.Error(codes.InvalidArgument, "first frame must be ForwardRawRequestInit")
	}
	if reqInit.GetInstance() == "" {
		return status.Error(codes.InvalidArgument, "instance is required")
	}

	// Per-instance concurrency accounting (mirrors ForwardHTTPStream).
	s.beginActivity(reqInit.GetInstance())
	defer s.endActivity(reqInit.GetInstance())

	netnsName, ok := s.vmm.NetnsFor(reqInit.GetInstance())
	if !ok {
		return status.Errorf(codes.NotFound, "instance %q not live", reqInit.GetInstance())
	}
	dialPort := reqInit.GetPort()
	if dialPort == 0 {
		dialPort = uint32(netns.AppPort)
	}

	// Spawn the bridge, wire the body-copy goroutine, then run
	// the head read + body pump. Each step is its own helper so
	// the handler stays under the CLAUDE.md 50-line cap and the
	// individual concerns (process lifecycle, streaming, error
	// mapping) are testable in isolation.
	cmd, stdinR, stdinW, stdoutR, stderr, err := rawBridgeSpawn(stream.Context(), netnsName, dialPort)
	if err != nil {
		return err
	}
	defer func() { _ = stdinR.Close() }()
	defer func() { _ = stdinW.Close() }()
	defer func() { _ = stdoutR.Close() }()

	bodyErrCh := rawBridgeBodyLoop(stream, stdinW, reqInit.GetMaxRequestBytes())

	headReader := bufio.NewReader(stdoutR)
	parsed, err := rawBridgeReadHead(headReader, stderr.String(), bodyErrCh)
	if err != nil {
		return err
	}
	if err := stream.Send(&vmmdpb.ForwardRawResponse{
		Frame: &vmmdpb.ForwardRawResponse_Init{
			Init: &vmmdpb.ForwardRawResponseInit{
				Status:  parsed.Status,
				Headers: parsed.Headers,
			},
		},
	}); err != nil {
		<-bodyErrCh
		_ = cmd.Wait()
		return status.Errorf(codes.Internal, "raw bridge init send: %v", err)
	}

	if err := rawBridgePumpBody(stream, headReader, parsed.Body); err != nil {
		<-bodyErrCh
		_ = cmd.Wait()
		return err
	}
	return rawBridgeFinish(cmd, bodyErrCh, stderr.String())
}

// rawBridgeSpawn resolves the vmmd-raw-bridge binary path, opens
// the stdio pipes, and starts the bridge under `ip netns exec`.
// Returns the running *exec.Cmd + the pipe ends + the stderr
// capture buffer. Callers own stdinR/stdoutR (close on exit) and
// stdinW (closed by the body-loop goroutine on its own exit).
//
// Bridge path resolution: prefer the production install path;
// fall back to $PATH so the test suite can run without installing
// the binary. The fallback is gated on a path-existence check
// to keep production behaviour deterministic.
func rawBridgeSpawn(ctx context.Context, netnsName string, dialPort uint32) (*exec.Cmd, *os.File, *os.File, *os.File, *bytes.Buffer, error) {
	bridgePath := vmmdRawBridgePath
	if _, statErr := os.Stat(bridgePath); statErr != nil {
		bridgePath = "vmmd-raw-bridge"
	}
	stdinR, stdinW, err := os.Pipe()
	if err != nil {
		return nil, nil, nil, nil, nil, status.Errorf(codes.Internal, "pipe: %v", err)
	}
	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		_ = stdinR.Close()
		_ = stdinW.Close()
		return nil, nil, nil, nil, nil, status.Errorf(codes.Internal, "stdout pipe: %v", err)
	}

	cmd := exec.CommandContext(ctx, "ip", "netns", "exec", netnsName,
		bridgePath, netns.GuestIP, strconv.FormatUint(uint64(dialPort), 10))
	cmd.Stdin = stdinR
	cmd.Stdout = stdoutW
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Start(); err != nil {
		_ = stdinR.Close()
		_ = stdinW.Close()
		_ = stdoutR.Close()
		_ = stdoutW.Close()
		return nil, nil, nil, nil, nil, status.Errorf(codes.Unavailable, "raw bridge start: %v", err)
	}
	// The bridge owns the write end of stdout; we own the read end.
	// Closing stdoutW after cmd.Wait ensures the pipe reader sees EOF.
	defer func() { _ = stdoutW.Close() }()
	return cmd, stdinR, stdinW, stdoutR, &stderr, nil
}

// rawBridgeBodyLoop copies inbound body_chunks → bridge stdin.
// The goroutine owns stdinW (closes it on exit — the EOF signal
// that lets the bridge's stdin read loop return). The byte
// counter enforces maxBody; past the cap the goroutine stops
// reading and surfaces ResourceExhausted via the returned channel.
//
// Returns a buffered channel sized for one error. Callers must
// drain it before declaring the stream done (cmd.Wait alone
// doesn't drain the gRPC-recv path).
func rawBridgeBodyLoop(stream grpc.BidiStreamingServer[vmmdpb.ForwardRawRequest, vmmdpb.ForwardRawResponse], stdinW *os.File, maxRequestBytes int64) chan error {
	maxBody := maxRequestBytes
	if maxBody <= 0 {
		maxBody = ForwardRawStreamMaxRequestBytes
	}
	bodyErrCh := make(chan error, 1)
	go func() {
		defer func() { _ = stdinW.Close() }()
		var written int64
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
			if written+int64(len(chunk)) > maxBody {
				bodyErrCh <- status.Errorf(codes.ResourceExhausted,
					"raw bridge request body exceeded %d bytes", maxBody)
				return
			}
			if _, err := stdinW.Write(chunk); err != nil {
				bodyErrCh <- err
				return
			}
			written += int64(len(chunk))
		}
	}()
	return bodyErrCh
}

// rawBridgeReadHead reads the response head from the bridge's
// stdout, parses it via parseBridgeOutput, and returns the
// status + headers + initial body buffer for the caller to ship
// in the ForwardRawResponseInit frame.
//
// On any error path the function drains the body goroutine so
// the caller doesn't have to think about it; the cap-exit
// surfaces Unavailable with the captured stderr so the gateway
// can log the bridge's error.
func rawBridgeReadHead(r *bufio.Reader, stderr string, bodyErrCh <-chan error) (*parsedBridgeResponse, error) {
	head, err := readUntilBlankLine(r)
	if err != nil {
		<-bodyErrCh
		return nil, status.Errorf(codes.Unavailable, "raw bridge head read: %v (stderr=%q)", err, stderr)
	}
	parsed, err := parseBridgeOutput(head)
	if err != nil {
		<-bodyErrCh
		return nil, status.Errorf(codes.Internal, "raw bridge head parse: %v", err)
	}
	return parsed, nil
}

// rawBridgePumpBody streams the guest's response body to the
// client. The first body buffer (parsed.Body) arrives inside
// the head read; subsequent bytes are read from the same
// bufio.Reader (which the bridge is feeding in 8 KiB-friendly
// chunks). The 8 KiB chunk is hoisted out of the loop so the
// hot path doesn't allocate per iteration.
//
// On any send error the function drains the body goroutine and
// surfaces Internal — the bidi stream is already half-closed by
// the guest at that point.
func rawBridgePumpBody(stream grpc.BidiStreamingServer[vmmdpb.ForwardRawRequest, vmmdpb.ForwardRawResponse], r *bufio.Reader, initialBody []byte) error {
	const respChunkSize = 8 * 1024
	if len(initialBody) > 0 {
		if err := stream.Send(&vmmdpb.ForwardRawResponse{
			Frame: &vmmdpb.ForwardRawResponse_BodyChunk{
				BodyChunk: append([]byte(nil), initialBody...),
			},
		}); err != nil {
			return status.Errorf(codes.Internal, "raw bridge body send: %v", err)
		}
	}
	buf := make([]byte, respChunkSize)
	for {
		n, rerr := r.Read(buf)
		if n > 0 {
			if err := stream.Send(&vmmdpb.ForwardRawResponse{
				Frame: &vmmdpb.ForwardRawResponse_BodyChunk{
					BodyChunk: append([]byte(nil), buf[:n]...),
				},
			}); err != nil {
				return status.Errorf(codes.Internal, "raw bridge body send: %v", err)
			}
		}
		if errors.Is(rerr, io.EOF) {
			return nil
		}
		if rerr != nil {
			return status.Errorf(codes.Internal, "raw bridge body read: %v", rerr)
		}
	}
}

// rawBridgeFinish waits on the bridge process + body goroutine
// and maps the combined result to a gRPC error. cmd.Wait is
// load-bearing — without it the child becomes a zombie and the
// captured stderr is unavailable when the bridge crashes.
//
// ResourceExhausted from the body-loop's cap-enforcement branch
// is the customer-facing signal; any other body error is
// Internal. A clean EOF on both sides returns nil.
func rawBridgeFinish(cmd *exec.Cmd, bodyErrCh <-chan error, stderr string) error {
	waitErr := cmd.Wait()
	bodyErr := <-bodyErrCh
	if bodyErr != nil {
		if st, ok := status.FromError(bodyErr); ok && st.Code() == codes.ResourceExhausted {
			return bodyErr
		}
		// Bridge crash (non-zero exit) — surface stderr so the
		// gateway can log the bridge's last words.
		var exitErr *exec.ExitError
		if errors.As(waitErr, &exitErr) || errors.As(bodyErr, &exitErr) {
			return status.Errorf(codes.Internal, "raw bridge body: %v wait=%v (stderr=%q)", bodyErr, waitErr, stderr)
		}
		// Otherwise: body-side error (client disconnected mid-
		// bidi, pipe broken). Bidi stream is already half-closed
		// by the guest at that point; don't replace gRPC OK.
	}
	if waitErr != nil {
		var exitErr *exec.ExitError
		if errors.As(waitErr, &exitErr) {
			return status.Errorf(codes.Internal, "raw bridge exited: %v (stderr=%q)", exitErr, stderr)
		}
	}
	return nil
}

// readUntilBlankLine reads from r until it sees the HTTP/1.1
// head terminator ("\n\n") or EOF. Returns the head bytes WITH
// each line's trailing "\n" intact (parseBridgeOutput's
// contract — see the legacy ForwardHTTPStream head-read at
// line 294-313) and the terminator line itself dropped. The
// body bytes that arrived inside the same read as the head
// remain on the bufio.Reader for the body loop below.
//
// Mirrors the legacy pattern: line-by-line ReadString('\n')
// + a 64 KiB cap. The cap is the load-bearing piece: a
// malicious or buggy guest that never sends the terminator
// must not OOM the server. In practice HTTP/1.1 heads are
// <8 KiB; 64 KiB is the same budget ForwardHTTPStream uses.
//
// Returns the partial bytes + io.EOF when the bridge closes the
// stream mid-head — the caller decides whether the partial
// bytes are a valid head (they almost never are) and surfaces
// a Unavailable to the gateway.
func readUntilBlankLine(r *bufio.Reader) ([]byte, error) {
	const headCap = 64 * 1024
	var out []byte
	for {
		line, err := r.ReadString('\n')
		out = append(out, line...)
		if line == "\n" {
			// terminator line — return everything before it.
			return out[:len(out)-1], nil
		}
		if len(out) > headCap {
			return nil, fmt.Errorf("bridge head exceeds %d bytes", headCap)
		}
		if errors.Is(err, io.EOF) {
			return out, io.EOF
		}
		if err != nil {
			return out, err
		}
	}
}

// buildStreamingBridgeScript (issue #471 PR-B + PR-C / ADR-047)
// is the streaming-only bridge script. Differences from the
// pre-PR-D legacy script (removed in PR-D):
//
//   - Request body comes from stdin in chunks (the Go server's
//     body-copy goroutine writes each ForwardHTTPStreamRequest
//     body_chunk to the bridge's stdin). The script reads
//     stdin and chunked-encodes it to fd 3; EOF on stdin
//     terminates the body read and the bridge continues to
//     read the response.
//   - Response: status line + headers are written to stdout
//     (terminated by a blank line, mirroring the legacy
//     script's parseBridgeOutput contract); the body bytes
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
// The bridge's request path uses the chunked-encoding
// pattern (`read -r -t 1 -n 8192 CHUNK` → `<hex-len>\r\n<body>\r\n`)
// because the gateway already emits Transfer-Encoding: chunked
// to the guest. respTimeout is reserved for a future per-line
// read deadline inside the cat loop (today, `cat <&3` is the
// simpler streaming primitive and the total budget is enforced
// by exec.CommandContext on the Go side).
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
// so binary payloads (image/jpeg, etc.) survive. The return shape
// mirrors the proto envelope minus the wire-only types removed in
// PR-D (the legacy unary ForwardHTTPRequest/ForwardHTTPResponse
// envelopes were torn out — the streaming RPC surfaces the headers
// via ForwardHTTPResponseInit instead).
func parseBridgeOutput(raw []byte) (*parsedBridgeResponse, error) {
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

	resp := &parsedBridgeResponse{
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

// parsedBridgeResponse is the local envelope returned by
// parseBridgeOutput. It mirrors the pre-PR-D proto type
// ForwardHTTPResponse (status, headers, body) but lives in the
// package instead of the wire so the streaming RPC can populate
// its own ForwardHTTPResponseInit frame without holding a
// removed proto type. Tests reach the value via
// ParseBridgeOutputForTest.
type parsedBridgeResponse struct {
	Status  int32
	Headers []*vmmdpb.Header
	Body    []byte
}

// ParseBridgeOutputForTest exposes the package's response parser so
// unit tests can drive the pure piece without standing up the full
// ForwardHTTPStream server (which depends on `ip netns exec` and is
// gated to //go:build metal). The signature mirrors parseBridgeOutput
// exactly; the only difference is the visibility. Returning the
// envelope as a pointer, not a value, preserves the byte slice
// ownership (the body carries through without an extra copy).
func ParseBridgeOutputForTest(raw []byte) (ParsedBridgeResponseForTest, error) {
	r, err := parseBridgeOutput(raw)
	if r == nil {
		return ParsedBridgeResponseForTest{}, err
	}
	return ParsedBridgeResponseForTest{
		Status:  r.Status,
		Headers: r.Headers,
		Body:    r.Body,
	}, err
}

// ParsedBridgeResponseForTest is the test-only exported mirror of
// parsedBridgeResponse. We keep the production type unexported
// (its only call site is the server's streaming RPC) and re-export
// a copy under the test-visibility name so the unit tests can read
// the headers and body without leaking the helper into the public
// package surface. The fields are identical to parsedBridgeResponse.
type ParsedBridgeResponseForTest struct {
	Status  int32
	Headers []*vmmdpb.Header
	Body    []byte
}

// BuildStreamingBridgeScriptForTest exposes the streaming script
// generator so unit tests can assert the dial-port + Host header
// rewrite (issue #460 / ADR-053 PR-C) without an ip netns exec.
// Tests grep the rendered script for the dial line + Host header;
// the strings are stable (see buildStreamingBridgeScript doc).
func BuildStreamingBridgeScriptForTest(req *vmmdpb.ForwardHTTPRequestInit, respTimeout time.Duration) string {
	return buildStreamingBridgeScript(req, respTimeout)
}

// keep guard reference: io is used in the body-copy goroutine
// (errors.Is(err, io.EOF)) and net/http is used by the chunked
// decoder wrapper (httputil.NewChunkedReader lives in
// net/http/httputil).
var _ = io.EOF
var _ = http.MethodGet

// beginActivity (PR-B, issue #462) is the nil-safe bridge to
// ActivityTracker.Begin. Centralising the guard keeps every
// caller's defer pair simple and makes the "Server wired without
// activity cache" path a no-op without scattering nil checks.
func (s *Server) beginActivity(instanceID string) {
	if s == nil || s.activity == nil {
		return
	}
	s.activity.Begin(instanceID)
}

// endActivity (PR-B, issue #462) is the nil-safe bridge to
// ActivityTracker.End. Pairs with beginActivity as the defer
// after ForwardHTTPStream's bridge work.
func (s *Server) endActivity(instanceID string) {
	if s == nil || s.activity == nil {
		return
	}
	s.activity.End(instanceID)
}
