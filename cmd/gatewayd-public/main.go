// Command gatewayd-public — plain-HTTP edge (Tier A7 split, ADR-070).
//
// gatewayd-public is the box's only public listener. In production
// it sits BEHIND Caddy + Cloudflare (api.gregale.dev) which handle
// TLS termination; this daemon serves plain HTTP on 127.0.0.1:8080
// and reverse-proxies to gatewayd-internal over the unix socket
// /run/faas/gatewayd-internal.sock.
//
// It owns:
//   - The plain-HTTP listener at FAAS_PUBLIC_LISTEN_ADDR
//     (default 127.0.0.1:8080 — loopback only, behind Caddy upstream)
//   - pkg/httpsec outer wrapper (HSTS / CSP nonce / X-Frame-Options /
//     Referrer-Policy / X-Content-Type-Options / Permissions-Policy)
//   - /healthz, /readyz, /metrics on loopback FAAS_PUBLIC_CONTROL_ADDR
//     (default 127.0.0.1:9090)
//   - Drain semantics: SIGTERM → flip /readyz → wait in-flight → Shutdown
//
// It does NOT own:
//   - hostname→app routing (gatewayd-internal does)
//   - the wake gate (gatewayd-internal does)
//   - the per-node forwarder (gatewayd-internal does)
//   - the rate limiter (gatewayd-internal does)
//   - TLS termination (Caddy + Cloudflare do, upstream)
//
// Inbound traffic shape:
//
//	customer HTTPS → Caddy (:443, ACME) → 127.0.0.1:8080 here →
//	                                          httpsec outer wrapper
//	                                          → gatewayd-internal over
//	                                            /run/faas/gatewayd-internal.sock
//
// Operators configure gatewayd-public via env overrides only (no
// certmagic, no TOML — TLS is upstream): FAAS_INTERNAL_SOCKET,
// FAAS_PUBLIC_LISTEN_ADDR, FAAS_PUBLIC_CONTROL_ADDR.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

	"github.com/onebox-faas/faas/pkg/api"
	"github.com/onebox-faas/faas/pkg/capdecl/runtimecheck"
	"github.com/onebox-faas/faas/pkg/db"
	"github.com/onebox-faas/faas/pkg/gateway"
	"github.com/onebox-faas/faas/pkg/httpsec"
	"github.com/onebox-faas/faas/pkg/wire"
)

// hstsEnabledFromEnv is the httpsec.HSTSEnabledFromEnv-shaped
// adapter. Note we use os.LookupEnv here (per the FAAS_APID_METRICS_ADDR
// empty=skip precedent) so an operator who explicitly sets
// FAAS_HSTS_ENABLED="" can distinguish "unset" from "off".
func hstsEnabledFromEnv(k string) string {
	if v, ok := os.LookupEnv(k); ok {
		return v
	}
	return ""
}

const (
	// defaultListenAddr is the loopback bind for the public listener.
	// Caddy (api.gregale.dev) reverse-proxies here.
	defaultListenAddr = "127.0.0.1:8080"
	// defaultInternalSocket is the unix socket gatewayd-public dials
	// to reach gatewayd-internal; the path lives under faas-vmmd's
	// /run/faas tmpfs (the SOLE RuntimeDirectory=faas).
	defaultInternalSocket = "/run/faas/gatewayd-internal.sock"
	// defaultPublicControlAddr is the loopback control plane
	// (/healthz, /readyz, /metrics). Loopback (not :9090) because
	// the legacy gatewayd daemon binds :9090.
	defaultPublicControlAddr = "127.0.0.1:9090"
)

// gatewaydPublicCapCheck is the DEPLOY-1 / ADR-075 capdecl gate
// seam (review finding M2). It is a package-level var instead of
// a runDeps field because gatewayd-public has a single-file
// `run()` body with no DI struct to extend. Production leaves it
// nil → run() falls back to the live runtimecheck.MustCheckOnBoot
// call against /proc/self/status. Tests override it (typically
// with func() error { return nil }) so the test runner's
// capset doesn't fail the gate. The override is package-scoped,
// not exported, so cmd/gatewayd-public_test.go can swap it within
// a t.Cleanup.
var gatewaydPublicCapCheck func() error

func main() {
	wire.Daemon("gatewayd-public", run)
}

// run is the daemon entry point. It builds the listener stack,
// wires the readiness probe, and blocks on ctx cancellation.
func run(ctx context.Context, log *slog.Logger) error {
	// DEPLOY-1 / ADR-075 capdecl gate. gatewayd-public is
	// unprivileged — no Allow, no Deny. The plain-HTTP loopback
	// listener, the httpsec outer wrapper, the unix-socket
	// reverse-proxy to gatewayd-internal, and the Postgres
	// readiness ping all run inside the unit's systemd hardening
	// (NoNewPrivileges, ProtectSystem, PrivateTmp, etc.). Any
	// future cap_ add lands here, not in the unit file.
	//
	// Note (review finding M2): unlike the runDeps-shaped
	// daemons, gatewayd-public has a single-file `run()` body —
	// there is no test seam struct to extend. The
	// capCheck-style override lives in the cmd-level
	// helper below for tests that want to drive the body
	// without exercising the live /proc/self/status check.
	capCheck := gatewaydPublicCapCheck
	if capCheck == nil {
		capCheck = func() error { return runtimecheck.MustCheckOnBoot(capsDecl, log, nil) }
	}
	if err := capCheck(); err != nil {
		return err
	}

	log.Info("gatewayd-public: starting", "pid", os.Getpid())

	// Postgres — required for the readiness ping (no other PG
	// dependency: certsync leader election is gone in plain-HTTP
	// mode, the warm-hint mirror is owned by gatewayd-internal).
	pool, err := db.Open(ctx, "")
	if err != nil {
		return fmt.Errorf("gatewayd-public: open db: %w", err)
	}
	defer pool.Close()

	// Readiness probe. Single signal: PG ping. (certsync/internal-proxy
	// signals are gone in plain-HTTP mode — once both listeners are
	// bound, /readyz=200.)
	probe, pgProbeSig, pgStop := setupReadiness(ctx, pool, log)
	defer pgStop()

	// Reverse-proxy to gatewayd-internal over the unix socket.
	internalSocket := envOr("FAAS_INTERNAL_SOCKET", defaultInternalSocket)
	internalURL := &url.URL{Scheme: "http", Host: "gatewayd-internal"}
	proxy := gateway.NewInternalReverseProxy(
		gateway.NewUnixSocketDialer(internalSocket),
		internalURL,
		log,
	)
	// Issue #555 PR-3: mount otelhttp.NewTransport so the outbound
	// request to gatewayd-internal carries the same trace context
	// (gateway.route span). The wrapper sits UNDER the proxy's
	// RoundTripper so the inbound span context is propagated on the
	// outbound hop.
	proxy.Transport = otelhttp.NewTransport(proxy.Transport)

	// Trace pipeline (issue #555 PR-2). Builds the in-memory ring,
	// wires the OTLP/HTTP exporter when OTEL_EXPORTER_OTLP_ENDPOINT
	// is set, and installs the GET /v1/traces/{trace_id} handler.
	// The trace setup is gatewayd-public's responsibility because
	// the ring is per-daemon (ADR-070 cross-box HA is N boxes each
	// with their own ring; the GET endpoint reaches this box only).
	traceSetup, err := gateway.InstallTracePipeline(ctx, "gatewayd-public", wire.Version, log)
	if err != nil {
		return fmt.Errorf("gatewayd-public: trace setup: %w", err)
	}
	// traceHandler mounts under /v1/traces/ (the trace handler
	// reads the path suffix). Wrap the proxy in a mux that
	// routes /v1/traces/ to the handler and falls through to the
	// proxy for everything else.
	traceMux := http.NewServeMux()
	traceMux.Handle("/v1/traces/", traceSetup.Handler)
	traceMux.Handle("/", proxy)

	// Public-facing handler: httpsec outer wrapper → trace mux → internal proxy.
	httpsec.SetHSTSEnabled(httpsec.HSTSEnabledFromEnv(hstsEnabledFromEnv))
	// Issue #555 PR-3: otelhttp.NewHandler extracts W3C traceparent
	// from inbound headers and starts a server span per request; the
	// emitted spans flow into the TraceRing (PR-2) + OTLP exporter.
	// This wrap sits INSIDE httpsec.Security so the response headers
	// (HSTS, CSP nonce, etc.) don't show up as span attributes (the
	// OTel view is the request itself, not the security headers).
	publicHandler := httpsec.Static(otelhttp.NewHandler(traceMux, "gatewayd-public.handler"))

	// Control mux + listeners.
	controlMux := gateway.ControlMux(nil, probe.ReadyFunc())
	controlAddr := envOr("FAAS_PUBLIC_CONTROL_ADDR", defaultPublicControlAddr)
	listenAddr := envOr("FAAS_PUBLIC_LISTEN_ADDR", defaultListenAddr)
	publicSrv, controlSrv := buildServers(listenAddr, controlAddr, publicHandler, controlMux)

	// Drain orchestration.
	if err := runDrain(ctx, log, publicSrv, controlSrv, pgProbeSig, pgStop, traceSetup); err != nil {
		return err
	}
	return nil
}

// setupReadiness builds the probe + the PG-ping signal whose bit is
// mirrored onto the probe's registered signal. The mirror is the
// bug-fixed bridge — the previous version read pgSig.Report() and
// discarded the values, so PG outages never flipped /readyz. Now
// the goroutine pushes the bit into the registered signal.
func setupReadiness(ctx context.Context, pool *pgxpool.Pool, log *slog.Logger) (*gateway.ReadyzProbe, *gateway.ReadySignal, func()) {
	probe := &gateway.ReadyzProbe{}
	pgSig, pgStop := gateway.NewPGPingSignal(ctx, pool, 5*time.Second)
	// Register a dedicated signal that is NOT pre-armed true (the
	// bridge below is the only writer; it flips it to the PG
	// liveness on the first tick).
	pgProbeSig := probe.Register()
	// Initial state: PG not yet checked. The bridge will flip it
	// within pgSig's first ping (synchronous, NewPGPingSignal
	// immediate-ping path).
	pgProbeSig.Set(false, "pg ping not yet sampled")
	go func() {
		t := time.NewTicker(2 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				ready, reason := pgSig.Report()
				if ready {
					pgProbeSig.Set(true, "")
				} else {
					pgProbeSig.Set(false, reason)
				}
			}
		}
	}()
	// Single readiness signal: PG ping. /readyz=200 once PG is
	// reachable; 503 (with the PG-ping reason) otherwise.
	_ = log
	return probe, pgProbeSig, pgStop
}

// buildServers constructs the plain-HTTP public + loopback control
// servers.
func buildServers(listenAddr, controlAddr string, publicHandler http.Handler, controlMux *http.ServeMux) (*http.Server, *http.Server) {
	publicSrv := &http.Server{
		Addr:              listenAddr,
		Handler:           publicHandler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		WriteTimeout:      300 * time.Second,
	}
	controlSrv := &http.Server{
		Addr:              controlAddr,
		Handler:           controlMux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	return publicSrv, controlSrv
}

// runDrain blocks on (ctx, SIGTERM, listener error). On SIGTERM it
// flips /readyz probes to not-ready, sleeps up to
// GatewayDrainGraceSeconds (cancellable on a second SIGTERM), then
// Shutdowns both servers. The PG signal is stopped FIRST so the
// probe stays 503 during the entire drain (caller pauses LB after
// observing 503, then in-flight requests finish).
//
// The order matches the ADR-068 spec:
//
//  1. SIGTERM → flip probe bits to false. /readyz=503 immediately.
//  2. Stop the PG ping signal so a wedged connection can't stall
//     the drain (default 30 s TimeoutStopSec vs 25 s grace + 5 s
//     Shutdown).
//  3. Sleep GatewayDrainGraceSeconds (cancellable on a second
//     SIGTERM — the signal channel has capacity 1, so the second
//     signal is dropped at the OS layer; we use a select with a
//     1s ticker to make the sleep effectively cancellable).
//  4. Shutdown both servers with a 5 s grace.
//  5. pgStop() (already done above; kept here as a no-op safety net).
func runDrain(ctx context.Context, log *slog.Logger, publicSrv, controlSrv *http.Server, pgProbeSig *gateway.ReadySignal, pgStop func(), traceSetup *gateway.TraceSetup) error {
	drainCtx, cancelDrain := context.WithCancel(context.Background())
	defer cancelDrain()
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigCh)
	go func() {
		<-sigCh
		log.Info("gatewayd-public: SIGTERM received; draining")
		pgProbeSig.Set(false, "draining")
		// Stop the PG ping signal BEFORE the grace sleep so the
		// probe stays 503 during the entire drain. The PG signal
		// goroutine may otherwise be stuck in pool.Ping and add
		// up to every/2 to the drain time.
		pgStop()
		// Cancellable sleep — a second SIGTERM short-circuits the
		// grace period and runs Shutdown immediately.
		grace := time.Duration(api.GatewayDrainGraceSeconds) * time.Second
		t := time.NewTicker(1 * time.Second)
		defer t.Stop()
		deadline := time.NewTimer(grace)
		defer deadline.Stop()
		select {
		case <-deadline.C:
		case <-sigCh:
			log.Warn("gatewayd-public: second SIGTERM received; skipping drain grace")
		}
		cancelDrain()
	}()

	// Start the listeners.
	errc := make(chan error, 2)
	go func() {
		l, lerr := net.Listen("tcp", publicSrv.Addr)
		if lerr != nil {
			errc <- fmt.Errorf("gatewayd-public: listen %s: %w", publicSrv.Addr, lerr)
			return
		}
		log.Info("gatewayd-public: public listening", "addr", publicSrv.Addr)
		if err := publicSrv.Serve(l); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errc <- err
		}
	}()
	go func() {
		log.Info("gatewayd-public: control listening", "addr", controlSrv.Addr)
		if err := controlSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errc <- err
		}
	}()

	select {
	case <-drainCtx.Done():
		log.Info("gatewayd-public: shutting down")
	case err := <-errc:
		return err
	}
	// Shutdown both servers gracefully. 5 s grace.
	// context.WithoutCancel detaches from the parent's cancellation
	// so a SIGTERM-driven parent cancel can't short-circuit the
	// grace period before in-flight requests finish (golangci-lint
	// v8 contextcheck: `Background` would lose any deadline set by
	// the wire.Daemon harness; WithoutCancel keeps the deadline
	// without inheriting the parent cancel).
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := publicSrv.Shutdown(sctx); err != nil {
		log.Warn("gatewayd-public: public Shutdown", "err", err)
	}
	if err := controlSrv.Shutdown(sctx); err != nil {
		log.Warn("gatewayd-public: control Shutdown", "err", err)
	}
	// Flush any in-flight spans from the BatchSpanProcessor.
	// WithoutCancel detaches from the daemon's cancelled ctx so the
	// SDK shutdown can flush its full batch; the 5s upper bound
	// matches the public-server shutdown grace so a slow collector
	// doesn't stall the daemon drain. (Issue #555 review: the
	// previous revision used context.Background(), discarding any
	// deadline the wire.Daemon harness set on ctx.)
	if traceSetup != nil {
		flushCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		if err := traceSetup.Shutdown(flushCtx); err != nil {
			log.Warn("gatewayd-public: trace shutdown", "err", err)
		}
		cancel()
	}
	return nil
}

// envOr is the canonical env-override helper (per cmd/gatewayd/main.go).
// Empty env falls back to `def`. Use os.LookupEnv directly when the
// caller needs to distinguish unset from empty (the empty=skip
// pattern; see FAAS_APID_METRICS_ADDR memory note).
func envOr(envKey, def string) string {
	if v := os.Getenv(envKey); v != "" {
		return v
	}
	return def
}
