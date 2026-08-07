// proxy_raw_stub.go — in-package fake for the raw-bytes Upgrade
// bridge (issue #676 / ADR-080). The PR-3 gateway forwarder
// opens VmmdClient.ForwardRawStream when it detects a request with
// Connection: Upgrade + Upgrade: <token>; the test seam needs a
// hand-rolled BidiStreamingClient[vmmdpb.ForwardRawRequest,
// vmmdpb.ForwardRawResponse] that records the init frame and
// replays a canned response (status + headers + body bytes).
//
// The stub mirrors proxyStubStream (proxy_stub.go) one-for-one —
// same lifecycle, same eager-on-Send emission, same io.EOF on
// drain — so the integration test's assertions read identically
// across the two bridges. The two shapes diverge at the wire
// frame names (ForwardRawRequest vs ForwardHTTPStreamRequest, the
// former with an Init/BodyChunk oneof); the surface is otherwise
// identical.
//
// Lives in package gateway (not _test) so forwardproxy_handler_test.go
// can wire it through the same stubVmmdClient seam the legacy
// streaming path uses.

package gateway

import (
	"context"
	"io"
	"sync"

	vmmdpb "github.com/onebox-faas/faas/api/proto/onebox/faas/vmmd/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

// proxyRawStubStream is the in-package fake of
// grpc.BidiStreamingClient[vmmdpb.ForwardRawRequest,
// vmmdpb.ForwardRawResponse]. The forwarder drives it the same
// way it drives the production vmmdClient.ForwardRawStream —
// open the stream, send the init frame, copy body chunks until
// CloseSend, then Recv frames into the statusRecorder /
// inbound ResponseWriter.
//
// Lifecycle is single-shot: after the test handler finishes
// consuming the response, the goroutine exits and the stream is
// closed.
type proxyRawStubStream struct {
	ctx      context.Context
	calls    *[]*vmmdpb.ForwardRawRequestInit
	mu       sync.Mutex
	init     *vmmdpb.ForwardRawResponseInit
	body     []byte
	sentInit bool
	closed   bool
	recvCh   chan *vmmdpb.ForwardRawResponse
	recvErr  error
}

func newProxyRawStubStream(ctx context.Context, calls *[]*vmmdpb.ForwardRawRequestInit, init *vmmdpb.ForwardRawResponseInit, body []byte) grpc.BidiStreamingClient[vmmdpb.ForwardRawRequest, vmmdpb.ForwardRawResponse] {
	s := &proxyRawStubStream{
		ctx:    ctx,
		calls:  calls,
		init:   init,
		body:   body,
		recvCh: make(chan *vmmdpb.ForwardRawResponse, 16),
	}
	return s
}

func (s *proxyRawStubStream) Header() (metadata.MD, error) {
	return nil, nil
}
func (s *proxyRawStubStream) Trailer() metadata.MD     { return nil }
func (s *proxyRawStubStream) CloseSend() error         { return nil }
func (s *proxyRawStubStream) Context() context.Context { return s.ctx }
func (s *proxyRawStubStream) SendMsg(m any) error      { return nil }
func (s *proxyRawStubStream) RecvMsg(_ any) error      { return nil }

// Send captures the client's init frame and emits the configured
// response frames. The stub streams its response on the first
// Recv — subsequent Recv calls return io.EOF so the forwarder
// exits cleanly. We send the init frame and the body chunk
// eagerly on Send so the test's Recv finds the data ready.
//
// Body-chunk frames from the gateway side (after the init) are
// captured into the configured calls slice for completeness —
// PR-3 tests assert on the init frame's Instance/Port, not the
// body bytes; the bridge carries them verbatim and the vmmd-side
// forwarder is unit-tested in pkg/vmmdgrpc/forward_internal_test.go.
func (s *proxyRawStubStream) Send(req *vmmdpb.ForwardRawRequest) error {
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
		s.recvCh <- &vmmdpb.ForwardRawResponse{
			Frame: &vmmdpb.ForwardRawResponse_Init{
				Init: s.init,
			},
		}
		// Emit the body chunk (if any) as a single frame.
		if len(s.body) > 0 {
			s.recvCh <- &vmmdpb.ForwardRawResponse{
				Frame: &vmmdpb.ForwardRawResponse_BodyChunk{
					BodyChunk: s.body,
				},
			}
		}
		// Close the receive stream so the next Recv returns io.EOF.
		s.recvErr = io.EOF
		close(s.recvCh)
	}
	return nil
}

// Recv returns the next response frame. The stub enqueues all
// frames on Send (init + body) and closes the channel; the
// forwarder reads them in order until io.EOF.
func (s *proxyRawStubStream) Recv() (*vmmdpb.ForwardRawResponse, error) {
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
