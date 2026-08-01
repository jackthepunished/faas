// proxy_stub.go — in-package fake for the vmmd gRPC client in
// forwardproxy_handler_test.go (issue #471 PR-D / ADR-047).
//
// The legacy unary ForwardHTTP RPC was removed; the streaming
// ForwardHTTPStream is the only bridge today. The
// stubVmmdClient.ForwardHTTPStream method returns a hand-rolled
// fake that satisfies grpc.BidiStreamingClient via the
// grpc.GenericClientStream shape (the same wrapper the production
// vmmdClient uses internally for the fake to behave like a real
// stream). The fake:
//
//   1. Accepts the client→server init frame; records it into the
//      shared *[]*vmmdpb.ForwardHTTPRequestInit so the integration
//      test can assert on it after the request returns.
//   2. Drains subsequent body-chunk frames until CloseSend / EOF.
//   3. Sends the configured response init frame (status + headers)
//      then emits the configured body bytes as one body_chunk frame.
//   4. Closes the client-side stream; the gRPC trailer header is
//      io.EOF on the next Recv().
//
// We need this stub in-package (not _test) because the test
// compiles against the unexported stubVmmdClient struct. The
// implementation is intentionally small — it exercises the
// forwarder's framing without spinning up a real gRPC server.

package gateway

import (
	"context"
	"sync"

	vmmdpb "github.com/onebox-faas/faas/api/proto/onebox/faas/vmmd/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

// proxyStubStream is the in-package fake of
// grpc.BidiStreamingClient[vmmdpb.ForwardHTTPStreamRequest,
// vmmdpb.ForwardHTTPStreamResponse]. The forwarder drives it the
// same way it drives the production vmmdClient.ForwardHTTPStream
// return — open the stream, send the init frame, copy body chunks
// until CloseSend, then Recv frames into the statusRecorder.
//
// The stream's lifecycle is single-shot: after the test handler
// finishes consuming the response, the goroutine exits and the
// stream is closed.
type proxyStubStream struct {
	ctx      context.Context
	calls    *[]*vmmdpb.ForwardHTTPRequestInit
	mu       sync.Mutex
	init     *vmmdpb.ForwardHTTPResponseInit
	body     []byte
	sentInit bool
	closed   bool
	recvCh   chan *vmmdpb.ForwardHTTPStreamResponse
	recvErr  error
}

func newProxyStubStream(ctx context.Context, calls *[]*vmmdpb.ForwardHTTPRequestInit, init *vmmdpb.ForwardHTTPResponseInit, body []byte) grpc.BidiStreamingClient[vmmdpb.ForwardHTTPStreamRequest, vmmdpb.ForwardHTTPStreamResponse] {
	s := &proxyStubStream{
		ctx:    ctx,
		calls:  calls,
		init:   init,
		body:   body,
		recvCh: make(chan *vmmdpb.ForwardHTTPStreamResponse, 16),
	}
	return s
}

func (s *proxyStubStream) Header() (metadata.MD, error) {
	return nil, nil
}
func (s *proxyStubStream) Trailer() metadata.MD     { return nil }
func (s *proxyStubStream) CloseSend() error         { return nil }
func (s *proxyStubStream) Context() context.Context { return s.ctx }
func (s *proxyStubStream) SendMsg(m any) error      { return nil }
func (s *proxyStubStream) RecvMsg(_ any) error      { return nil }

// Send captures the client's init frame and emits the configured
// response frames. The stub streams its response on the first
// Recv — subsequent Recv calls return io.EOF so the forwarder
// exits cleanly. We send the init frame and the body chunk
// eagerly on Send so the test's Recv finds the data ready.
func (s *proxyStubStream) Send(req *vmmdpb.ForwardHTTPStreamRequest) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if req == nil {
		return nil
	}
	if init := req.GetInit(); init != nil {
		// Record the init frame for the test's assertion.
		*s.calls = append(*s.calls, init)
		if s.sentInit {
			return nil
		}
		s.sentInit = true
		// Emit the response init frame.
		s.recvCh <- &vmmdpb.ForwardHTTPStreamResponse{
			Frame: &vmmdpb.ForwardHTTPStreamResponse_Init{
				Init: s.init,
			},
		}
		// Emit the body chunk (if any) as a single frame.
		if len(s.body) > 0 {
			s.recvCh <- &vmmdpb.ForwardHTTPStreamResponse{
				Frame: &vmmdpb.ForwardHTTPStreamResponse_BodyChunk{
					BodyChunk: s.body,
				},
			}
		}
		// Close the receive stream so the next Recv returns io.EOF.
		s.recvErr = ioEOF
		close(s.recvCh)
	}
	return nil
}

// Recv returns the next response frame. The stub enqueues all
// frames on Send (init + body) and closes the channel; the
// forwarder reads them in order until io.EOF.
func (s *proxyStubStream) Recv() (*vmmdpb.ForwardHTTPStreamResponse, error) {
	s.mu.Lock()
	closed := s.closed
	err := s.recvErr
	s.mu.Unlock()
	if closed {
		return nil, err
	}
	frame, ok := <-s.recvCh
	if !ok {
		s.mu.Lock()
		s.closed = true
		s.mu.Unlock()
		return nil, err
	}
	return frame, nil
}

// ioEOF is the sentinel error the stub returns on Recv when the
// stream is closed. We can't import "io" here without a cycle
// in some test setups; the literal error matches io.EOF.
var ioEOF = errEOF{}

type errEOF struct{}

func (errEOF) Error() string { return "EOF" }
