package sched

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/onebox-faas/faas/pkg/state"
)

// LogFrameSink is the per-frame callback the StreamAppLogs handler
// invokes for each frame decoded from the per-instance vmmd Logs
// RPC. It returns a non-nil error to abort the stream (the gRPC
// trailer carries it back to the apid caller); a nil return tells
// the handler to keep delivering the next frame.
//
// The production caller (pkg/scheddgrpc.Server.StreamAppLogs) renders
// the frame to a scheddpb.StreamAppLogsResponse proto and forwards
// it on the caller's gRPC stream. That work is bounded by the
// per-frame size (≤ a few KB); long-running work inside the
// callback would stall backpressure on the matching vmmd Logs
// stream.
//
// The writer goroutine that owns the gRPC stream only ever calls
// this from the select-arm that owns the Send — so the callback
// is serialised with the gRPC write and cannot race with the
// reader-side Recv.
type LogFrameSink func(instance string, seq int64, stream, line string, writtenAt time.Time) error

// StreamAppLogs (issue #254 / Move 4) is the schedd-side fan-out
// for the per-app log stream. The engine resolves the live
// instances via the store, opens one vmmd Logs RPC per instance
// via the RoutedVMM, and invokes the sink for every frame (initial-
// page + live tail) until the context cancels.
//
// Returns state.ErrNotFound when the app has zero live instances
// (apid maps this to its 404 "the app is parked; wake it first").
// A node-level vmmd RPC failure logs and continues — the surviving
// instances keep streaming.
//
// Implementation notes:
//
//   - We open one goroutine per live instance. Each goroutine
//     loops on vmmd.Logs.Recv and forwards frames to the shared
//     sink. The sink runs on the writer goroutine so a slow
//     consumer naturally backpressures all per-instance streams.
//
//   - The vmmd Logs RPC emits io.EOF on a clean instance shutdown
//     (ring closed → subscribe channel drained). The goroutine
//     exits on EOF and the wart set shrinks. The outer loop
//     blocks on wg.Wait() until all instances have ended or
//     the context cancels.
//
//   - The writer goroutine owns the sink call (so the proto
//     marshal is serialised with the gRPC Send). The per-
//     instance readers send over a buffered channel; the writer
//     selects on sink err first to short-circuit on cancellation.
func (e *Engine) StreamAppLogs(ctx context.Context, appID string, sinceSeq int64, sink LogFrameSink) error {
	if e.vmm == nil {
		return errors.New("sched: StreamAppLogs requires a vmm router")
	}
	if e.store == nil {
		return errors.New("sched: StreamAppLogs requires a store")
	}
	rows, err := e.store.ListInstancesForApp(ctx, appID)
	if err != nil {
		return fmt.Errorf("sched: StreamAppLogs list instances: %w", err)
	}
	live := make([]state.Instance, 0, len(rows))
	for _, ins := range rows {
		if !state.IsLive(ins.State) || ins.NodeID == "" {
			continue
		}
		live = append(live, ins)
	}
	if len(live) == 0 {
		return state.ErrNotFound
	}
	// Adapter: each per-instance goroutine sends LogLine tuples
	// on this channel; the writer goroutine reads them and
	// invokes the sink. Keeping the proto marshal on the writer
	// goroutine lets us serialise the gRPC Send with the callback
	// chain.
	type frame struct {
		instance  string
		seq       int64
		name      string
		line      string
		writtenAt time.Time
	}
	ch := make(chan frame, 32)
	var wg sync.WaitGroup
	for _, ins := range live {
		ins := ins
		wg.Add(1)
		go func() {
			defer wg.Done()
			stream, err := e.vmm.Logs(ctx, ins.NodeID, ins.ID, sinceSeq)
			if err != nil {
				// Per-instance dial failure is non-fatal;
				// the surviving instances keep streaming.
				return
			}
			for {
				line, err := stream.Recv()
				if err != nil {
					// io.EOF (clean shutdown) or a
					// gRPC status error. Either way,
					// this instance is done.
					return
				}
				select {
				case <-ctx.Done():
					return
				case ch <- frame{
					instance:  ins.ID,
					seq:       line.Seq,
					name:      line.Stream,
					line:      line.Line,
					writtenAt: line.WrittenAt,
				}:
				}
			}
		}()
	}
	// Closer: closes the frame channel once all instance goroutines
	// have ended, so the writer's range loop exits cleanly.
	go func() {
		wg.Wait()
		close(ch)
	}()
	for f := range ch {
		if err := sink(f.instance, f.seq, f.name, f.line, f.writtenAt); err != nil {
			// Bubble up so pkg/scheddgrpc.Server.StreamAppLogs can
			// carry the gRPC trailer (the sink error is the
			// per-frame Send failure; a context.Canceled or
			// codes.Unavailable-from-StreamAppLogs). The
			// per-instance reader goroutines exit on ctx.Done()
			// inside their select; the closer goroutine drains
			// them via wg.Wait.
			return err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
	return nil
}
