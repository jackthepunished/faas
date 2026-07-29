// cmd/gatewayd egress-grpc listener (ADR-046 PR-2 producer channel).
//
// Why this lives in a separate file from main.go: the dialer,
// listener management, and registration logic are small but
// not zero; isolating them keeps main.go's runWithDeps readable
// and gives the test harness a single seam to swap the
// underlying grpc.Server transport.
//
// Why a second unix socket rather than sharing the synth one:
//   A unix socket can host either an HTTP server or a gRPC
//   server, not both. The synth service (pkg/gateway/synth.go)
//   is HTTP-shaped because the cron body is JSON over HTTP/1.1
//   (a simpler wire than gRPC frame encoding for the
//   one-shot-per-tick cron dispatch path). The egress service
//   (pkg/gateway/egressgrpc) is gRPC-shaped because the
//   producer/consumer relationship is long-lived and the
//   server-streaming RPC matches meterd's natural cadence
//   exactly. Separate sockets, identical DAC auth posture
//   (group `faas`, mode 0660 — ADR-015).

package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"time"

	"google.golang.org/grpc"

	gatewaydpb "github.com/onebox-faas/faas/api/proto/onebox/faas/gatewayd/v1"
	"github.com/onebox-faas/faas/pkg/gateway/egressgrpc"
	"github.com/onebox-faas/faas/pkg/gateway/egresssink"
)

const (
	// defaultEgressGRPCSocketPath is the default unix-domain socket
	// ADR-046 PR-2 gRPC producer channel listens on. Override with
	// FAAS_GATEWAY_EGRESS_SOCKET. Distinct from
	// FAAS_GATEWAY_SYNTH_SOCKET so the existing cron/dispatch
	// service stays HTTP-shaped on its own socket (see file
	// header).
	defaultEgressGRPCSocketPath = "/run/faas/gatewayd-egress.sock"

	// egressGRPCSocketMode mirrors the synth socket's 0660 + group
	// `faas` posture (ADR-015). Only schedd/meterd are in the
	// group, so the socket itself IS the auth.
	egressGRPCSocketMode = 0o660
)

// egressGRPCSocketPath returns the socket path to bind, honoring
// the FAAS_GATEWAY_EGRESS_SOCKET override. Empty string disables
// the listener entirely (used by tests + the e2e harness).
func egressGRPCSocketPath() string {
	if v, ok := os.LookupEnv("FAAS_GATEWAY_EGRESS_SOCKET"); ok && v != "" {
		return v
	}
	return defaultEgressGRPCSocketPath
}

// egressGRPCListener owns the *grpc.Server + its bound unix
// socket. Lifetime is bound to the cmd/gatewayd daemon: start()
// binds + serves; stop() shuts down the server with a 5-second
// grace and removes the socket file (the daemon owns the
// socket, recreate is safer than fail-on-EADDRINUSE at next
// boot).
type egressGRPCListener struct {
	socketPath string
	server     *grpc.Server
	sink       *egresssink.EgressSink
	log        *slog.Logger
}

// newEgressGRPCListener constructs (but does not start) the
// gRPC listener. socketPath is the unix-domain bind target.
// Empty path = noop listener (Start/Stop are both no-ops).
func newEgressGRPCListener(socketPath string, srv *egressgrpc.Server, log *slog.Logger) *egressGRPCListener {
	if socketPath == "" {
		return &egressGRPCListener{log: log}
	}
	gs := grpc.NewServer(
		grpc.MaxRecvMsgSize(1<<20), // 1 MiB; one frame is small
		grpc.MaxSendMsgSize(1<<20), // matches above
		grpc.ConnectionTimeout(5*time.Second),
	)
	gatewaydpb.RegisterEgressTxServiceServer(gs, srv)
	return &egressGRPCListener{
		socketPath: socketPath,
		server:     gs,
		sink:       nil, // reserved for a future /debug endpoint
		log:        log,
	}
}

// start binds the unix socket and starts the gRPC server in a
// goroutine. The 0660 chmod follows the synth server pattern —
// once the listener is bound, the socket's permissions become
// the auth posture.
func (l *egressGRPCListener) start() error {
	if l == nil || l.socketPath == "" || l.server == nil {
		return nil
	}
	if err := os.Remove(l.socketPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("gatewayd egress: remove stale socket: %w", err)
	}
	lis, err := net.Listen("unix", l.socketPath)
	if err != nil {
		return fmt.Errorf("gatewayd egress: listen %s: %w", l.socketPath, err)
	}
	if err := os.Chmod(l.socketPath, egressGRPCSocketMode); err != nil {
		_ = lis.Close()
		return fmt.Errorf("gatewayd egress: chmod: %w", err)
	}
	l.log.Info("gatewayd egress: listening", "socket", l.socketPath)
	go func() {
		if err := l.server.Serve(lis); err != nil {
			l.log.Warn("gatewayd egress: serve", "err", err)
		}
	}()
	return nil
}

// stop tears down the gRPC server + removes the socket file.
// The returned error is informational; the daemon continues
// shutdown even on cleanup failures.
func (l *egressGRPCListener) stop(ctx context.Context) error {
	if l == nil || l.server == nil {
		return nil
	}
	l.server.GracefulStop()
	if l.socketPath != "" {
		_ = os.Remove(l.socketPath)
	}
	return nil
}
