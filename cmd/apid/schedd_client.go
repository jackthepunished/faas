package main

import (
	"context"
	"errors"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// stubScheddClient is the per-app log stream fallback used when
// no real schedd dial is available (issue #254 / Move 4). It
// returns a gRPC Unimplemented status so the SSE handler emits
// a `degraded` event with a stable RFC 7807 code:
//
//	error: { "status": 503, "reason": "schedd_unreachable" }
//
// Production wiring replaces it via withScheddClient in
// cmd/apid/main.go once the daemon is alive.
type stubScheddClient struct{}

// StreamAppLogs (issue #254 / Move 4) returns Unimplemented so
// the SSE handler degrades gracefully when the schedd isn't
// reachable. The handler maps the error to its `degraded` event
// and closes the stream — a customer-tail a parked or a not-yet-
// booted app sees a stable wire shape, not a 500.
func (stubScheddClient) StreamAppLogs(_ context.Context, _ string, _ int64) (schedLogStream, error) {
	return nil, status.Error(codes.Unimplemented, "schedd not wired (dev mode)")
}

// errScheddClosed is the sentinel returned by stubScheddStream
// when a caller Recv's on a closed stream. Kept here so the
// SSE handler can branch on "stream ended normally" vs "error".
var errScheddClosed = errors.New("schedd stream closed")
