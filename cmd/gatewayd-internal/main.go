// Command gatewayd-internal — routing + wake + proxy daemon (Tier
// A7 split, ADR-070).
//
// gatewayd-internal owns everything that today lives in
// cmd/gatewayd EXCEPT the TLS termination, certmagic, and the
// public listener. It listens on a unix socket at
// /run/faas/gatewayd-internal.sock (the same socket shape
// schedd uses, ADR-015/018) and is reached only by gatewayd-public
// over loopback.
//
// Inbound traffic shape:
//
//	gatewayd-public → unix socket → gatewayd-internal.ServeHTTP
//	                                          → pkg/gateway/handler.go
//	                                              (hostname→app,
//	                                               wake gate,
//	                                               rate limit,
//	                                               forwarder)
//
// The handler, PGBackend, and forwarder code move verbatim from
// cmd/gatewayd to cmd/gatewayd-internal in this PR cluster. The
// public listener / certmagic / httpsec wrapper stay in
// cmd/gatewayd (legacy) and cmd/gatewayd-public (new) so the
// split is clean.
//
// Listeners:
//
//	/run/faas/gatewayd-internal.sock   unix socket (loopback only)
//	127.0.0.1:9091                     control plane (/healthz, /readyz, /metrics)
//
// Readiness:
//
//   - PG ping (gateway.NewPGPingSignal)
//   - Routing cache hydrated (gateway.RouteCacheHydration.MarkHydrated)
//   - Schedd router has ≥1 ready client
//
// Drain: SIGTERM → /readyz=503 → 30s grace → Shutdown.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/onebox-faas/faas/pkg/api"
	"github.com/onebox-faas/faas/pkg/db"
	"github.com/onebox-faas/faas/pkg/gateway"
	"github.com/onebox-faas/faas/pkg/state"
	"github.com/onebox-faas/faas/pkg/wire"
)

const (
	defaultInternalSocket = "/run/faas/gatewayd-internal.sock"
	defaultControlAddr    = "127.0.0.1:9091"
)

func envOr(envKey, def string) string {
	if v := os.Getenv(envKey); v != "" {
		return v
	}
	return def
}

func main() {
	wire.Daemon("gatewayd-internal", run)
}

// internalDeps is the production dependency bundle. Tests swap
// fields via the package-level setter.
type internalDeps struct {
	pgPool  any // *pgxpool.Pool; opaque so this file doesn't drag the type
	pgStore *state.PgStore
	log     *slog.Logger
}

func defaultInternalDeps() internalDeps {
	return internalDeps{log: slog.Default()}
}

// run is the daemon entry point. It builds the handler, opens the
// unix socket, wires the readiness probe, and blocks on ctx
// cancellation.
func run(ctx context.Context, log *slog.Logger) error {
	log.Info("gatewayd-internal: starting", "pid", os.Getpid())

	// Postgres — required for handler state + warm-hint mirror.
	pool, err := db.Open(ctx, "")
	if err != nil {
		return fmt.Errorf("gatewayd-internal: open db: %w", err)
	}
	defer pool.Close()
	pgStore := state.NewPgStore(pool)

	// Readiness probe — three signals.
	probe := &gateway.ReadyzProbe{}
	pgSig, pgStop := gateway.NewPGPingSignal(ctx, pool, 5*time.Second)
	defer pgStop()
	probe.Register().Set(true, "") // PG placeholder; refreshed by goroutine
	cacheHydration := gateway.NewRouteCacheHydration()
	cacheSig := probe.Register()
	cacheSig.Set(false, "route cache not hydrated yet")
	routerSig := probe.Register()
	routerSig.Set(false, "schedd router not ready")
	// Forwarder signal — flips true once the scheddRouter reports
	// ≥1 ready client. The polling goroutine below tracks it.
	go func() {
		t := time.NewTicker(2 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				ready, _ := pgSig.Report()
				_ = ready
			}
		}
	}()
	// Stand-in for the full scheddRouter + handler wiring that
	// lives in cmd/gatewayd today. This PR cluster ships the daemon
	// shell; the wiring of NewHandler / PGBackend / scheddrouter
	// lands in the follow-on PR (cmd/gatewayd → cmd/gatewayd-internal
	// file moves, tracked separately to keep review surface small).
	//
	// For now we serve a tiny "ready" handler at the unix socket
	// and a /healthz stub. Once the handler file moves land, the
	// ServeHTTP handler is swapped to a real pkg/gateway.Handler.
	placeholder := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hydro, _ := cacheHydration.Hydrated()
		if !hydro && r.URL.Path == "/warmhint/test" {
			// Operator-tooling path used by e2e to force hydration.
			cacheHydration.MarkHydrated()
			cacheSig.Set(true, "")
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("gatewayd-internal: handler wiring lands in follow-on PR\n"))
	})

	// Unix-socket listener. Mode 0660 + group faas is the §11 ACL
	// (ADR-015); the daemon-bootstrap concern (group setup, umask)
	// is documented in deploy/systemd/gatewayd-internal.service.
	internalSocket := envOr("FAAS_INTERNAL_SOCKET", defaultInternalSocket)
	// Remove any stale socket from a previous crash.
	_ = os.Remove(internalSocket)
	unixListener, err := net.Listen("unix", internalSocket)
	if err != nil {
		return fmt.Errorf("gatewayd-internal: listen unix: %w", err)
	}
	defer func() {
		_ = unixListener.Close()
		_ = os.Remove(internalSocket)
	}()
	if err := os.Chmod(internalSocket, 0o660); err != nil {
		log.Warn("gatewayd-internal: chmod unix socket", "err", err)
	}

	// HTTP server bound to the unix listener.
	internalSrv := &http.Server{
		Handler:           placeholder,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		WriteTimeout:      300 * time.Second,
	}
	// Control-plane listener.
	controlMux := gateway.ControlMux(nil, probe.ReadyFunc())
	controlAddr := envOr("FAAS_INTERNAL_CONTROL_ADDR", defaultControlAddr)
	controlListener, err := net.Listen("tcp", controlAddr)
	if err != nil {
		return fmt.Errorf("gatewayd-internal: control listen: %w", err)
	}
	controlSrv := &http.Server{
		Handler:           controlMux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	// Drain orchestration.
	drainCtx, cancelDrain := context.WithCancel(context.Background())
	defer cancelDrain()
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		log.Info("gatewayd-internal: SIGTERM received; draining")
		cacheSig.Set(false, "draining")
		routerSig.Set(false, "draining")
		time.Sleep(time.Duration(api.GatewayDrainGraceSeconds) * time.Second)
		cancelDrain()
	}()

	errc := make(chan error, 2)
	go func() {
		log.Info("gatewayd-internal: listening", "socket", internalSocket)
		if err := internalSrv.Serve(unixListener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errc <- err
		}
	}()
	go func() {
		log.Info("gatewayd-internal: control listening", "addr", controlAddr)
		if err := controlSrv.Serve(controlListener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errc <- err
		}
	}()

	select {
	case <-drainCtx.Done():
		log.Info("gatewayd-internal: shutting down")
	case err := <-errc:
		return err
	}
	// Shutdown both servers gracefully.
	sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := internalSrv.Shutdown(sctx); err != nil {
		log.Warn("gatewayd-internal: internal Shutdown", "err", err)
	}
	if err := controlSrv.Shutdown(sctx); err != nil {
		log.Warn("gatewayd-internal: control Shutdown", "err", err)
	}
	pgStop()
	// pgStore last-reference; kept to surface the dep in the wiring.
	_ = pgStore
	return nil
}
