// Advisory gRPC client — Wave 0 PR-C / ADR-047. vmmd dials
// /run/faas/apid.sock to forward guest-init fanotify batches.
//
// First vmmd-issued gRPC client in the repo (the vmmd daemon has
// historically been a *server* only — see pkg/vmmdgrpc/server.go).
// Wire shape: api/proto/onebox/faas/apid/v1/advisory.proto.
//
// ADR-035 best-effort: a failed advisory write is logged + dropped,
// never retried beyond a single in-process retry. The advisory is
// observation, not source of truth; tightening to STREAM-with-retry
// is a Wave 1 follow-up if the audit ever becomes safety-critical.

package vmmdgrpc

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/onebox-faas/faas/api/proto/onebox/faas/apid/v1"
	"github.com/onebox-faas/faas/pkg/fcvm"
	"github.com/onebox-faas/faas/pkg/wire"
)

// advisoryDialTimeout is the per-RPC dial+send budget. The advisory
// goroutine in vmmd's wire receiver should never block on apid —
// fanotify is fire-and-forget, and a stuck dial would back-pressure
// the vsock read.
const advisoryDialTimeout = 200 * time.Millisecond

// advisoryRetryDelay is the wait between the initial attempt and the
// single retry. 200 ms matches the dial timeout so a full retry
// cycle costs at most 600 ms (well under the 1s guest dedupe window).
const advisoryRetryDelay = 200 * time.Millisecond

// AdvisoryClient is the vmmd-side dialer for the apid Advisory
// service. One per vmmd process; serialises calls behind a mutex
// because the underlying *grpc.ClientConn is safe for concurrent use
// but we want one dial to coalesce a burst of advisories into a
// single connection.
//
// ops is the vmmd daemon's *wire.OpsMetrics — used to increment
// the stateless_advisory_batches_emitted_total counter at each
// Forward outcome (Mega-PR B). It is safe to pass nil; the accessor
// is nil-receiver safe and treats a nil *OpsMetrics as a no-op, so
// unit tests that don't wire metrics keep working without stubs.
type AdvisoryClient struct {
	target string // "unix:///run/faas/apid.sock"
	log    *slog.Logger
	ops    *wire.OpsMetrics

	mu   sync.Mutex
	conn *grpc.ClientConn
	cli  apidpb.AdvisoryClient
}

// NewAdvisoryClient builds a client against the given unix-socket
// target (e.g. "unix:///run/faas/apid.sock"). The connection is NOT
// dialled until the first Forward call — keeps boot cheap and
// tolerates a transiently-down apid (the unit test path).
//
// ops is the vmmd daemon's *wire.OpsMetrics. Pass nil if metrics
// are not wired (the accessor is nil-receiver safe). Mega-PR B
// promotes this to a positional parameter (rather than a setter)
// so the receiver loop's race against late-binding is closed at
// construction time — the alternative (SetOpsMetrics after
// construction) would race the AdvisoryClient's Forward goroutines
// for every advisory batch.
func NewAdvisoryClient(target string, log *slog.Logger, ops *wire.OpsMetrics) *AdvisoryClient {
	if log == nil {
		log = slog.Default()
	}
	return &AdvisoryClient{target: target, log: log, ops: ops}
}

// Forward sends one stateless advisory batch to apid. ADR-035
// best-effort: on failure we log Warn, increment the
// stateless_advisory_batches_emitted_total{result} counter (Mega-PR
// B), and drop the batch.
//
// Returns nil even on apid-side NotFound — the advisory row was
// already observation; a missing app row is information, not an
// error to retry.
func (c *AdvisoryClient) Forward(ctx context.Context, instance, appID string, events []fcvm.AdvisoryEvent) error {
	if c == nil {
		return nil
	}
	if appID == "" || instance == "" || len(events) == 0 {
		// Empty batches are a no-op; matches the manager-side guard.
		return nil
	}

	cli, err := c.dial(ctx)
	if err != nil {
		c.log.Warn("advisory forward: dial failed", "target", c.target, "err", err)
		c.ops.ObserveAdvisoryBatchResult(wire.AdvisoryResultDialFailed)
		return nil
	}

	req := &apidpb.ForwardStatelessAdvisoryRequest{
		Instance: instance,
		AppId:    appID,
		Events:   advisoryEventsToProto(events),
	}

	// Single retry on codes.Unavailable. ADR-035 keeps this tight —
	// one retry, then drop. A persistent down apid surfaces via the
	// Warn log, not via a stuck vmmd goroutine.
	for attempt := 0; attempt < 2; attempt++ {
		if attempt > 0 {
			time.Sleep(advisoryRetryDelay)
		}
		callCtx, cancel := context.WithTimeout(ctx, advisoryDialTimeout)
		_, err := cli.ForwardStatelessAdvisory(callCtx, req)
		cancel()
		if err == nil {
			c.ops.ObserveAdvisoryBatchResult(wire.AdvisoryResultOK)
			return nil
		}
		st, ok := status.FromError(err)
		if !ok || st.Code() != codes.Unavailable {
			c.log.Warn("advisory forward: apid rejected", "err", err, "attempt", attempt+1)
			c.ops.ObserveAdvisoryBatchResult(wire.AdvisoryResultRejected)
			return nil
		}
		c.log.Warn("advisory forward: apid unavailable; retrying once", "attempt", attempt+1, "err", err)
	}
	c.log.Warn("advisory forward: giving up after retry", "instance", instance, "app_id", appID, "events", len(events))
	c.ops.ObserveAdvisoryBatchResult(wire.AdvisoryResultUnavailableAfterRetry)
	return nil
}

// dial lazily dials the apid socket. Holds c.mu so the first burst
// of advisories after vmmd boot coalesce on one connection.
func (c *AdvisoryClient) dial(ctx context.Context) (apidpb.AdvisoryClient, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cli != nil && c.conn != nil {
		return c.cli, nil
	}
	dialCtx, cancel := context.WithTimeout(ctx, advisoryDialTimeout)
	defer cancel()
	conn, err := wire.DialContext(dialCtx, c.target, nil)
	if err != nil {
		return nil, fmt.Errorf("advisory: dial %s: %w", c.target, err)
	}
	c.conn = conn
	c.cli = apidpb.NewAdvisoryClient(conn)
	return c.cli, nil
}

// Close releases the underlying gRPC conn. Idempotent. Called from
// vmmd's shutdown path so SIGTERM doesn't leak the dial.
func (c *AdvisoryClient) Close() error {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn == nil {
		return nil
	}
	err := c.conn.Close()
	c.conn = nil
	c.cli = nil
	return err
}

// AdvisoryEvent is an alias for fcvm.AdvisoryEvent so callers can
// type-check against the pkg/fcvm projection without importing
// pkg/fcvm directly. Deprecated as a separate type — the wire
// receiver and the Manager now share fcvm.AdvisoryEvent end-to-end.
// Kept for one release to avoid breaking cmd/vmmd test stubs that
// reach in. Wave 1 follow-up: remove.
type AdvisoryEvent = fcvm.AdvisoryEvent

// advisoryEventsToProto translates the pkg/fcvm projection to the
// generated proto.
func advisoryEventsToProto(in []fcvm.AdvisoryEvent) []*apidpb.AdvisoryEvent {
	if len(in) == 0 {
		return nil
	}
	out := make([]*apidpb.AdvisoryEvent, 0, len(in))
	for _, e := range in {
		out = append(out, &apidpb.AdvisoryEvent{
			Path:     e.Path,
			Mask:     e.Masks,
			Pid:      int32(e.PID),
			TsUnixMs: e.TsUnix,
		})
	}
	return out
}
