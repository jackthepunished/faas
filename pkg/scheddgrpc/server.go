// Package scheddgrpc turns pkg/sched.Engine into the gRPC service defined in
// api/proto/onebox/faas/schedd/v1 (ADR-018). Handlers stay thin — each wraps a
// single Engine call and translates its result into the proto envelope, routing
// errors through pkg/grpcerr so the gateway maps them straight to RFC 7807.
// Mirrors pkg/vmmdgrpc on the vmmd side.

package scheddgrpc

import (
	"context"
	"errors"
	"log/slog"
	"time"

	scheddpb "github.com/onebox-faas/faas/api/proto/onebox/faas/schedd/v1"
	vmmdpb "github.com/onebox-faas/faas/api/proto/onebox/faas/vmmd/v1"
	"github.com/onebox-faas/faas/pkg/api"
	"github.com/onebox-faas/faas/pkg/grpcerr"
	"github.com/onebox-faas/faas/pkg/sched"
	"github.com/onebox-faas/faas/pkg/sched/instancestats"
	"github.com/onebox-faas/faas/pkg/state"
	"github.com/onebox-faas/faas/pkg/wire"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// LogFrameSink is the per-frame callback the StreamAppLogs handler
// invokes for each frame decoded from the per-instance vmmd Logs
// RPC. It returns a non-nil error to abort the stream (the gRPC
// trailer carries it back to the apid caller); a nil return tells
// the handler to keep delivering the next frame.
//
// The production caller (Server.StreamAppLogs) renders the frame
// to a scheddpb.StreamAppLogsResponse proto and forwards it on
// the caller's gRPC stream. That work is bounded by the per-frame
// size (≤ a few KB); long-running work inside the callback would
// stall backpressure on the matching vmmd Logs stream.
//
// The writer goroutine that owns the gRPC stream only ever calls
// this from the select-arm that owns the Send — so the callback
// is serialised with the gRPC write and cannot race with the
// reader-side Recv.
type LogFrameSink = sched.LogFrameSink

// SchedAPI is the slice of pkg/sched.Engine the handlers need. Defined here (not
// imported as a concrete type) so unit tests can pass a fake without standing up
// a store + vmmd.
type SchedAPI interface {
	Wake(ctx context.Context, appID string) (sched.WakeResult, error)
	// AdmitInstance is the schedule scale-out primitive (issue #168).
	// Bypasses the Phase-1 fast-path so a gateway can demand a new
	// instance even when others are already RUNNING. Returns a typed
	// AtCapacity result on the benign "already at max_concurrency"
	// outcome — see sched.WakeResult.AtCapacity.
	AdmitInstance(ctx context.Context, appID string) (sched.WakeResult, error)
	ReportActivity(ctx context.Context, touches []state.InstanceTouch) (int, error)
	// ParkWithReason is the meterd-triggered variant (M7, spec §4.7).
	// The reason string is for the audit log; the park semantics are
	// identical to the idle-reaper Park.
	ParkWithReason(ctx context.Context, instanceID, reason string) error
	// StreamAppLogs (issue #254 / Move 4) fans out the per-instance
	// log stream to a callback. The engine resolves the live
	// instances, opens one vmmd Logs RPC per instance, and invokes
	// the sink for every frame (initial-page + live tail) until
	// the context cancels. Returns nil on a clean shutdown; the
	// underlying gRPC / vmmd errors propagate as-is so the
	// handlers can decide to lift or map them.
	StreamAppLogs(ctx context.Context, appID string, sinceSeq int64, sink LogFrameSink) error
}

// StatsReader is the per-instance snapshot the schedd's
// instancestats.Poller publishes. The scheddgrpc server exposes
// this to meterd via ListInstanceStats (issue #279 / PR-B). A thin
// interface keeps pkg/scheddgrpc decoupled from pkg/sched — tests
// pass a fake without standing up the poller.
type StatsReader interface {
	SnapshotAll() []instancestats.InstanceStat
}

// Server implements scheddpb.ScheddServer.
type Server struct {
	scheddpb.UnimplementedScheddServer

	engine SchedAPI
	stats  StatsReader
	ops    *wire.OpsMetrics
	log    *slog.Logger
}

// New wires the server. ops may be nil (a throwaway registry used by
// bufconn tests); log may be nil (slog default). The default ops
// registry is namespaced "schedd" so metrics from a misconfigured
// test harness don't collide with production series.
func New(engine SchedAPI, ops *wire.OpsMetrics, log *slog.Logger) *Server {
	if log == nil {
		log = slog.Default()
	}
	if ops == nil {
		ops = wire.NewOpsMetrics("schedd")
	}
	return &Server{engine: engine, ops: ops, log: log}
}

// NewWithStats wires the server with an instancestats.Reader for
// the ListInstanceStats RPC. Production calls this from cmd/schedd;
// tests that don't exercise CPU stats use plain New. Issue #279 /
// PR-B.
func NewWithStats(engine SchedAPI, stats StatsReader, ops *wire.OpsMetrics, log *slog.Logger) *Server {
	if log == nil {
		log = slog.Default()
	}
	if ops == nil {
		ops = wire.NewOpsMetrics("schedd")
	}
	return &Server{engine: engine, stats: stats, ops: ops, log: log}
}

// Register binds s to a gRPC server.
func (s *Server) Register(g *grpc.Server) {
	scheddpb.RegisterScheddServer(g, s)
}

// Wake asks the engine to bring up an instance for the app and returns its
// address. Admission denials arrive as *api.Problem and travel as a
// ResourceExhausted status the gateway turns into a 503.
func (s *Server) Wake(ctx context.Context, req *scheddpb.WakeRequest) (*scheddpb.WakeResponse, error) {
	const op = "Wake"
	start := time.Now()
	res, err := s.engine.Wake(ctx, req.GetAppId())
	s.ops.Observe(op, time.Since(start), err)
	if err != nil {
		return nil, grpcerr.ToStatus(toProblem(err))
	}
	return &scheddpb.WakeResponse{
		InstanceId: res.InstanceID,
		NodeId:     res.NodeID,
		Method:     mapMethod(res.Method),
		WakeId:     res.WakeID,
	}, nil
}

// AdmitInstance (issue #168) is the schedule scale-out RPC. Unlike
// Wake it does not run the Phase-1 fast-path: each call either admits
// a fresh instance or returns at_capacity=true. The gateway calls
// this to fan-out across max_concurrency without hitting the
// WakeGate's single-flight coalescing.
//
// Three outcomes map to three wire shapes:
//   - admitted:        instance_id/node_id/wake_id populated, at_capacity=false
//   - at_capacity:    at_capacity=true, identity fields empty
//   - failure:        ResourceExhausted / Internal status with the
//     RFC 7807 problem in the response's `problem`
//     field — only on real admission errors (RAM
//     headroom, chooser, store). The benign
//     app_concurrency_reached outcome is NEVER lifted
//     to a gRPC error: it surfaces as at_capacity=true
//     so the gateway can treat it as a no-op.
func (s *Server) AdmitInstance(ctx context.Context, req *scheddpb.AdmitInstanceRequest) (*scheddpb.AdmitInstanceResponse, error) {
	const op = "AdmitInstance"
	start := time.Now()
	res, err := s.engine.AdmitInstance(ctx, req.GetAppId())
	s.ops.Observe(op, time.Since(start), err)
	if err != nil {
		return nil, grpcerr.ToStatus(toProblem(err))
	}
	return &scheddpb.AdmitInstanceResponse{
		InstanceId: res.InstanceID,
		NodeId:     res.NodeID,
		Method:     mapMethod(res.Method),
		WakeId:     res.WakeID,
		AtCapacity: res.AtCapacity,
	}, nil
}

// ReportActivity persists a last_request_at batch from the gateway.
func (s *Server) ReportActivity(ctx context.Context, req *scheddpb.ReportActivityRequest) (*scheddpb.ReportActivityResponse, error) {
	const op = "ReportActivity"
	start := time.Now()
	in := req.GetTouches()
	touches := make([]state.InstanceTouch, 0, len(in))
	for _, t := range in {
		touches = append(touches, state.InstanceTouch{
			InstanceID:  t.GetInstanceId(),
			LastRequest: time.UnixMilli(t.GetUnixMs()),
		})
	}
	applied, err := s.engine.ReportActivity(ctx, touches)
	s.ops.Observe(op, time.Since(start), err)
	if err != nil {
		return nil, grpcerr.ToStatus(toProblem(err))
	}
	return &scheddpb.ReportActivityResponse{Applied: int32(applied)}, nil
}

// ParkInstance is the meterd-driven explicit park (M7, spec §4.7).
// Idempotent: parking an already-parked instance is a no-op + Ok=true.
func (s *Server) ParkInstance(ctx context.Context, req *scheddpb.ParkInstanceRequest) (*scheddpb.ParkInstanceResponse, error) {
	const op = "ParkInstance"
	start := time.Now()
	err := s.engine.ParkWithReason(ctx, req.GetInstanceId(), req.GetReason())
	s.ops.Observe(op, time.Since(start), err)
	if err != nil {
		// ErrNotFound → NotFound status; everything else Internal.
		if errors.Is(err, state.ErrNotFound) {
			return nil, status.Error(codes.NotFound, err.Error())
		}
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &scheddpb.ParkInstanceResponse{Ok: true}, nil
}

// StreamAppLogs (issue #254 / Move 4) implements the schedd-side
// half of the per-app log stream. It fans out the per-instance
// vmmd Logs RPCs into a single server-streaming response so the
// apid daemon can render one SSE stream per app.
//
// Two phases that mirror the vmmd Logs RPC:
//
//  1. Initial page — every line with Seq > since_seq, emitted in
//     per-instance arrival order (then instance order, so a
//     reconnect carries the same frame sequence the client last
//     saw — identical to the vmmd snapshot semantics).
//  2. Live tail — schedd subscribes to each live instance's
//     vmmd Logs stream and emits new lines as they arrive. On
//     instance death (park / snapshot) the per-instance stream
//     ends with io.EOF and the next iteration of the outer loop
//     drops that goroutine from the wart set.
//
// Backpressure: the per-instance vmmd Logs stream is gRPC; the
// apid-side caller's gRPC stream is also gRPC. The two-stage
// pipeline keeps the per-instance ring's Subscribe channel drops
// bounded by the round-trip time of the matching vmmd RPC.
//
// Wire error mapping:
//
//   - codes.NotFound when the app has zero live instances — apid
//     maps this to its 404 "the app is parked; wake it first"
//   - codes.OK + nil on clean shutdown (caller cancels, all
//     instances died)
//
// Additive per ADR-016: the new RPC + new messages append at the
// end of the proto file.
func (s *Server) StreamAppLogs(req *scheddpb.StreamAppLogsRequest, stream scheddpb.Schedd_StreamAppLogsServer) error {
	const op = "StreamAppLogs"
	start := time.Now()
	var sendErr error
	defer func() {
		s.ops.Observe(op, time.Since(start), sendErr)
	}()
	if req.GetAppId() == "" {
		sendErr = status.Error(codes.InvalidArgument, "app_id is required")
		return sendErr
	}
	sinceSeq := req.GetSinceSeq()
	if sinceSeq < 0 {
		sinceSeq = 0
	}
	// sink is the per-frame callback that writes one
	// StreamAppLogsResponse onto the caller's gRPC stream. We
	// synchronise on stream.Context() so a cancelled caller
	// short-circuits the vmmd-side drain immediately.
	sink := func(instance string, seq int64, streamName, line string, writtenAt time.Time) error {
		if stream.Context().Err() != nil {
			return stream.Context().Err()
		}
		resp := &scheddpb.StreamAppLogsResponse{
			InstanceId: instance,
			Seq:        seq,
			Stream:     streamName,
			Line:       line,
		}
		if !writtenAt.IsZero() {
			resp.WrittenAt = timestamppb.New(writtenAt)
		}
		return stream.Send(resp)
	}
	if err := s.engine.StreamAppLogs(stream.Context(), req.GetAppId(), sinceSeq, sink); err != nil {
		// Engine.StreamAppLogs surfaces "no live instances" as
		// state.ErrNotFound (apid maps this to its 404 "the app
		// is parked; wake it first"). A clean caller cancel
		// surfaces as context.Canceled — gRPC's expected
		// behaviour on a dropped streaming caller (codes.Canceled
		// is what the apid sees). Anything else (per-instance
		// vmmd dial failure that escalated to a Send error) lands
		// here as codes.Unavailable so the apid can render a
		// degraded frame and let the consumer retry.
		sendErr = err
		switch {
		case errors.Is(err, state.ErrNotFound):
			return status.Error(codes.NotFound, err.Error())
		case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
			return status.Error(codes.Canceled, err.Error())
		default:
			return status.Error(codes.Unavailable, err.Error())
		}
	}
	return nil
}

// ListInstanceStats returns the per-instance CPU-µs snapshot the
// schedd's instancestats.Poller maintains. The meterd sampler
// calls this once per minute and computes the per-min CPU delta
// per instance, writing it to usage_minutes.cpu_usec. Issue #279 /
// PR-B.
//
// The response carries every live instance's current row
// (deterministic (app_id, instance_id) order — the Reader's
// contract). The cpu_valid field mirrors instancestats.Validity:
// 0 = Valid, 1 = Unknown. Callers MUST skip rows where
// cpu_valid != 0; otherwise the post-regression baseline becomes
// "stuck" on the pre-regression counter and the per-minute delta
// is meaningless. The vmmd cpustats.Cache already drops the
// baseline on regression, so the next sample the poller sees comes
// back as Valid and the meterd sampler picks up from the new
// counter.
//
// stats may be nil (e.g. a test harness that doesn't wire the
// Reader); the handler returns an empty response rather than a
// gRPC error so the meterd sampler can degrade to "no CPU data"
// without restarting the loop.
func (s *Server) ListInstanceStats(ctx context.Context, _ *scheddpb.ListInstanceStatsRequest) (*scheddpb.ListInstanceStatsResponse, error) {
	const op = "ListInstanceStats"
	start := time.Now()
	rows := make([]*scheddpb.InstanceStatsRow, 0)
	if s.stats != nil {
		// Time the SnapshotAll fan-out + per-row proto-build
		// separately on `schedd_list_instance_stats_collect_seconds`
		// (issue #279 / PR-B / ADR-039) so an operator can graph
		// the CPU rate-and-accumulator hot path on the schedd
		// side in isolation from the rest of the RPC.
		cpuStart := time.Now()
		for _, r := range s.stats.SnapshotAll() {
			row := &scheddpb.InstanceStatsRow{
				InstanceId: r.InstanceID,
				AppId:      r.AppID,
				NodeId:     r.NodeID,
				CpuUsec:    r.CPUUsageUsec,
				CpuValid:   uint32(r.CPU),
			}
			rows = append(rows, row)
		}
		if h := s.ops.CPUStatsCollectDuration(); h != nil {
			h.Observe(time.Since(cpuStart).Seconds())
		}
	}
	s.ops.Observe(op, time.Since(start), nil)
	return &scheddpb.ListInstanceStatsResponse{Rows: rows}, nil
}

// mapMethod translates the engine's vmmd-side WakeMethod to the schedd wire
// enum. The two enums share values (0=cold boot, 1=restore); the switch keeps
// them decoupled if either drifts.
func mapMethod(m vmmdpb.WakeMethod) scheddpb.WakeMethod {
	if m == vmmdpb.WakeMethod_WAKE_RESTORE {
		return scheddpb.WakeMethod_WAKE_RESTORE
	}
	return scheddpb.WakeMethod_WAKE_COLD_BOOT
}

// toProblem lifts a plain error to *api.Problem if it isn't one already, so
// internal go-stack details never leak across the wire.
func toProblem(err error) *api.Problem {
	if err == nil {
		return nil
	}
	if p := api.AsProblem(err); p != nil {
		return p
	}
	return api.NewProblem(int(codes.Internal), "internal", "schedd operation failed", err.Error())
}
