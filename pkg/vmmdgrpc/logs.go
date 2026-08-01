package vmmdgrpc

import (
	"time"

	vmmdpb "github.com/onebox-faas/faas/api/proto/onebox/faas/vmmd/v1"
	"github.com/onebox-faas/faas/pkg/fcvm/logbuf"
	"github.com/onebox-faas/faas/pkg/wire"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Logs (issue #254 / Move 4) streams the per-instance ring buffer to the
// caller. Two phases:
//
//  1. Initial page: every line with Seq > sinceSeq is sent (Snapshot).
//  2. Live tail: Subscribe() returns a channel that delivers each new
//     committed line until the caller's context is cancelled or the
//     ring is closed (Kill/DestroyWithExport).
//
// The stream.Send call is the natural gRPC backpressure surface: a
// slow consumer starves the ring's Subscribe channel, which drops
// new lines via the ring's non-blocking publish (counter at apid).
//
// Wire status:
//
//   - codes.InvalidArgument when the caller forgot instance
//   - codes.NotFound when the instance is not alive on this vmmd
//   - codes.OK + nil on clean shutdown (caller cancels, ring closes)
//
// We do NOT return a server-streaming "end of stream" frame; gRPC
// carries the EOF out-of-band and the apid producer emits a terminal
// `event: end` frame on top of that.
func (s *Server) Logs(req *vmmdpb.LogsRequest, stream vmmdpb.Vmmd_LogsServer) error {
	const op = "Logs"
	start := time.Now()
	var sendErr error
	defer func() {
		s.ops.Observe(op, time.Since(start), sendErr)
	}()
	// issue #517: lift the wake_id / app_id correlation off the
	// inbound gRPC metadata so any slog record this handler emits
	// (currently just the open/close; PR-C will adopt the canonical
	// timeline event names) carries the same correlation fields
	// the upstream schedd logged. The runtime source is the MD
	// (CorrelationFromIncoming); the proto field on LogsRequest is
	// documentation / future-validation. We prefer the MD value
	// over the proto field so an out-of-band consumer (one that
	// doesn't set the proto field) still gets correlation if its
	// middleware sets the MD.
	fields, _ := wire.CorrelationFromIncoming(stream.Context())
	if fields.WakeID == "" {
		fields.WakeID = req.GetWakeId()
	}
	streamLogger := wire.WithCorrelationFields(s.log, fields)
	if req.GetInstance() == "" {
		sendErr = status.Error(codes.InvalidArgument, "instance is required")
		streamLogger.Warn("vmmd: Logs: missing instance")
		return sendErr
	}
	ring := s.vmm.LogRing(req.GetInstance())
	if ring == nil {
		sendErr = status.Error(codes.NotFound, "instance not live on this vmmd")
		streamLogger.Info("vmmd: Logs: ring not found",
			"instance", req.GetInstance())
		return sendErr
	}
	streamLogger.Info("vmmd: Logs: stream opened",
		"instance", req.GetInstance(),
		"since_seq", req.GetSinceSeq())
	// Initial page: lines with Seq > since_seq in commit order. The
	// ring's Snapshot filters by Seq >= sinceSeq (>= so a sinceSeq
	// equal to the last seq is still considered replayed; the caller
	// who wants strict "after" semantics passes seq+1).
	for _, line := range ring.Snapshot(req.GetSinceSeq()) {
		if err := stream.Send(lineToPb(line)); err != nil {
			sendErr = err
			return err
		}
	}
	// Live tail. Subscribe returns an independent buffered channel per
	// subscriber; concurrent subscribers do not share backpressure.
	ch, cancel := ring.Subscribe()
	defer cancel()
	for {
		select {
		case <-stream.Context().Done():
			return nil
		case line, ok := <-ch:
			if !ok {
				// Ring closed — instance was Killed/Parked.
				return nil
			}
			if err := stream.Send(lineToPb(line)); err != nil {
				sendErr = err
				return err
			}
		}
	}
}

// lineToPb maps a ring Line to the proto envelope. The Seq/Stream/Line
// fields are 1:1; written_at is the host-side ingest time, never the
// guest clock (ADR-022 entropy hazard).
func lineToPb(l logbuf.Line) *vmmdpb.LogsResponse {
	return &vmmdpb.LogsResponse{
		Seq:       l.Seq,
		Stream:    l.Stream,
		Line:      l.Line,
		WrittenAt: timestamppb.New(l.WrittenAt),
	}
}
