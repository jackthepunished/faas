// client_warmhints.go — gatewayd-side typed adapter for the
// schedd StreamWarmHints gRPC stream (ADR-025 axis 4).
//
// Mirrors client_logs.go:50-98 (the StreamAppLogs adapter). The
// proto stream is converted into a typed WarmHintStream surface
// so the gatewayd consumer doesn't reach into the generated
// protobuf package. Errors pass through unchanged — the consumer
// in cmd/gatewayd/warmhints.go distinguishes io.EOF (clean
// shutdown) from gRPC status codes via errors.Is / status.FromError.

package scheddgrpc

import (
	"context"
	"errors"
	"io"

	scheddpb "github.com/onebox-faas/faas/api/proto/onebox/faas/schedd/v1"
	"github.com/onebox-faas/faas/pkg/sched"
	"google.golang.org/grpc"
)

// WarmHintEvent is the transport-neutral mirror of
// scheddpb.StreamWarmHintsResponse. Defined here so the gatewayd
// consumer can name scheddgrpc.WarmHintEvent without importing
// either the protobuf package or pkg/sched directly — same shape
// as sched.WarmHintEvent (the engine-side broadcaster type),
// type-aliased so the two packages agree on the wire by
// construction.
//
// WrittenAt is converted via timestamppb.AsTime; an unset proto
// timestamp yields the zero time.Time. Production always sets it
// (the broadcaster's emit site stamps it at fanout time), so
// callers can rely on the field being populated.
type WarmHintEvent = sched.WarmHintEvent

// WarmHintStream is the per-event stream returned by
// Client.StreamWarmHints. Recv blocks until the next event or the
// stream ends. A successful Recv returns (event, nil) for a data
// event; (zero, io.EOF) for a clean shutdown; (zero, gRPC-status)
// for any error.
//
// Errors pass through unchanged so callers can map gRPC codes
// (Canceled, Unavailable) themselves. Do not call liftErr on
// these — the consumer in cmd/gatewayd/warmhints.go treats
// Unavailable as a transient reconnect signal rather than a
// platform-level failure.
type WarmHintStream interface {
	Recv() (WarmHintEvent, error)
}

// StreamWarmHints opens a server-streaming RPC against the
// schedd's StreamWarmHints endpoint (ADR-025 axis 4). The
// schedd fans out (appID → nodeID) changes from its
// warmHintBroadcaster into one server stream; this method hands
// the caller a typed view of that stream.
//
// The request is empty (StreamWarmHintsRequest has no fields
// today); the proto is reserved for future per-node / per-app
// filters. Tail-from-now semantics: the caller sees events
// emitted after the dial, never replays state on connect.
//
// Returned errors pass through unchanged. The caller owns error
// mapping; this method never lifts gRPC statuses to *api.Problem.
func (c *Client) StreamWarmHints(ctx context.Context) (WarmHintStream, error) {
	stream, err := c.cli.StreamWarmHints(ctx, &scheddpb.StreamWarmHintsRequest{})
	if err != nil {
		return nil, err
	}
	return &warmHintStreamAdapter{inner: stream}, nil
}

// warmHintStreamAdapter converts the proto stream into the typed
// WarmHintStream surface. It does not transform errors — the raw
// gRPC status (or io.EOF) flows back to the caller so the
// consumer can distinguish a clean shutdown from a transient
// reconnect signal.
type warmHintStreamAdapter struct {
	inner grpc.ServerStreamingClient[scheddpb.StreamWarmHintsResponse]
}

var _ WarmHintStream = (*warmHintStreamAdapter)(nil)

// Recv blocks for the next event or end-of-stream. WrittenAt is
// converted via timestamppb.AsTime; an unset proto timestamp
// yields the zero time.Time, which the gatewayd cache treats
// as "use the existing entry" (no-op update).
func (a *warmHintStreamAdapter) Recv() (WarmHintEvent, error) {
	resp, err := a.inner.Recv()
	if err != nil {
		// io.EOF and gRPC status errors pass through untouched.
		// The consumer distinguishes them via errors.Is(err,
		// io.EOF) and status.FromError respectively.
		if errors.Is(err, io.EOF) {
			return WarmHintEvent{}, io.EOF
		}
		return WarmHintEvent{}, err
	}
	return WarmHintEvent{
		AppID:     resp.GetAppId(),
		NodeID:    resp.GetNodeId(),
		WrittenAt: resp.GetWrittenAt().AsTime(),
	}, nil
}
