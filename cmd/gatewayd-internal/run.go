// Command gatewayd — routing + wake + proxy daemon (legacy / Tier A7 split, ADR-070).
//
// This daemon predates the Tier A7 split (gatewayd-public + gatewayd-internal,
// ADR-068). After the split ships, gatewayd-public owns TLS termination +
// ACME :80/:443 + the httpsec outer wrapper, and gatewayd-internal owns
// routing + wake + proxy on the unix socket /run/faas/gatewayd-internal.sock.
// This legacy binary remains in-tree (and on the box) during the migration
// window (cd-controlplane.yml BINARIES="apid schedd gatewayd …" still installs
// it) but is not in FAAS_SERVICES / FAAS_RESTART_ORDER. Operators disable the
// legacy systemd unit once the split pair is healthy.
//
// In the split topology the daemon body moves verbatim into
// cmd/gatewayd-internal/ (PR-A: pure refactor; PR-B: replace the
// placeholder handler); the legacy binary keeps its current shape so
// cd-controlplane installs it byte-for-byte until operators retire it.
//
// Listeners:
//
//	public :8080 plain HTTP   (legacy / e2e harness fallback)
//	private :9090 loopback    /healthz /readyz /metrics
//
// All share ctx cancellation so a SIGTERM shuts them down in parallel.
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/onebox-faas/faas/pkg/api"
	"github.com/onebox-faas/faas/pkg/audit"
	authmw "github.com/onebox-faas/faas/pkg/auth/middleware"
	"github.com/onebox-faas/faas/pkg/db"
	"github.com/onebox-faas/faas/pkg/events"
	"github.com/onebox-faas/faas/pkg/gateway"
	"github.com/onebox-faas/faas/pkg/gateway/egressgrpc"
	"github.com/onebox-faas/faas/pkg/gateway/egresssink"
	"github.com/onebox-faas/faas/pkg/httpsec"
	"github.com/onebox-faas/faas/pkg/middleware"
	"github.com/onebox-faas/faas/pkg/scheddgrpc"
	"github.com/onebox-faas/faas/pkg/session"
	"github.com/onebox-faas/faas/pkg/state"
	"github.com/onebox-faas/faas/pkg/wire"
)

// scheddSocket is schedd's gRPC unix socket (ADR-018). Phase 2 /
// Gate A: kept for tests and the e2e harness that point gatewayd at
// a single schedd by env override. Production wiring in run()
// ignores scheddSocket and uses the per-node scheddRouter (the
// cache derives each per-node client's dial target from
// compute_nodes.schedd_target_url, not from this var).
var scheddSocket = envOrGateway("FAAS_SCHEDD_SOCKET", "/run/faas/schedd.sock")

// gatewaydInternalSocket is the unix-domain socket schedd dials to
// fire synthetic cron requests through gatewayd (spec §4.4, M7).
// Mode 0660 group `faas` (ADR-015); only schedd can dial. Overridable
// via FAAS_GATEWAY_SYNTH_SOCKET so the e2e harness can place it on a
// per-test path without needing /run/faas on the host (PR #203).
var gatewaydInternalSocket = envOrGateway("FAAS_GATEWAY_SYNTH_SOCKET", "/run/faas/gatewayd-internal.sock")

// listenAddr is the public listener (TLS lands here in M8). Overridable via
// FAAS_GATEWAY_LISTEN so the e2e harness can bind a free port without colliding
// with a dev daemon on :8080.
var listenAddr = envOrGateway("FAAS_GATEWAY_LISTEN", ":8080")

// configPath is the on-disk TOML config. Overridable via FAAS_GATEWAYD_CONFIG
// for non-standard deployments; production uses /etc/faas/gatewayd.toml.
var configPath = envOrGateway("FAAS_GATEWAYD_CONFIG", "/etc/faas/gatewayd.toml")

// controlAddr is the private control-plane listener — never reachable from
// the internet; bound to the loopback interface by default so an
// operator-prometheus scrape is the only thing that can reach it.
// SECURITY: the env override must stay loopback in production. An
// operator who sets FAAS_GATEWAY_CONTROL_LISTEN to a non-loopback
// address exposes /metrics to the network (the control mux is
// unauthenticated by design — it's intended for a trusted prometheus
// sidecar on the same host). Refuse anything that resolves to a
// non-loopback interface when the env knob is used (PR #218 review
// finding). Overridable via FAAS_GATEWAY_CONTROL_LISTEN so the e2e
// harness can pick a free port (without this, two parallel harness
// runs would race for the hard-coded 127.0.0.1:9090 — the
// deploy_wake_metal test is the only consumer of /metrics and lives
// behind a metal build tag, so CI on plain ubuntu-latest doesn't trip
// the race, but a local dev box with two concurrent invocations
// would). The default matches the legacy constant so existing
// deployments are unaffected.
var controlAddr = envOrGateway("FAAS_GATEWAY_CONTROL_LISTEN", "127.0.0.1:9090")

// streamingEnabledFromEnv is the per-process opt-in for the
// streaming response path (issue #471 / ADR-047). PR-A wires the
// flag end-to-end but the actual Flusher path ships in PR-B; while
// PR-A's buffered-fallback AC #4 needs the flag to flow into the
// Handler, the runtime behaviour is unchanged (every request still
// buffers). Production default is false — operators opt in per-cluster
// after PR-B ships; the e2e harness sets it to true via the
// FAAS_GATEWAY_STREAMING env var.
func streamingEnabledFromEnv() bool {
	v := strings.ToLower(strings.TrimSpace(envOrGateway("FAAS_GATEWAY_STREAMING", "false")))
	return v == "1" || v == "true" || v == "yes"
}

// synthAdapter implements gateway.SynthDispatcher on top of the schedd
// gRPC client + the in-process gateway handler. Move 1 widens the
// surface from Wake-only to two methods so the synthetic HTTP envelope
// (method + path + body + headers) actually reaches the runner on
// cron / async / queue-pull / delayed-task paths — the legacy
// wake-only path left cron traffic invisible to the runner and the
// meter (spec §4.4, M7).
type synthAdapter struct {
	wake   func(ctx context.Context, appID string) error
	invoke func(ctx context.Context, appID string, inv state.Invocation) (state.Invocation, error)
}

func (a *synthAdapter) Wake(ctx context.Context, appID string) error { return a.wake(ctx, appID) }

// Invoke first wakes an instance (idempotent if RUNNING — always-Wake
// per the Move 1 plan), then routes the synthetic envelope through
// the wake gate via the in-process gateway handler. The runner side
// receives (method, path, headers, body), parses them, and answers
// the HTTP response; that response becomes the Invocation.Result the
// caller writes back via Store.CompleteInvocation.
func (a *synthAdapter) Invoke(ctx context.Context, appID string, inv state.Invocation) (state.Invocation, error) {
	if a.invoke == nil {
		return inv, fmt.Errorf("gateway synth: invoke is not wired (legacy wake-only adapter)")
	}
	return a.invoke(ctx, appID, inv)
}

// prodRunDeps is the dependency seam for run. Tests inject net.Listen / http.Server
// wrappers so the seam is fully exercised without spawning a real daemon.
type prodRunDeps struct {
	listen  func(network, addr string) (net.Listener, error)
	newSrv  func(addr string, handler http.Handler) *http.Server
	backend gateway.Backend
	// streamingEnabled (issue #471 / ADR-047) is the per-process
	// opt-in for the streaming response path. Production passes
	// envOr(TOML) || env("FAAS_GATEWAY_STREAMING"); tests leave it
	// false. PR-A wires the flag into the Handler but the actual
	// Flusher path ships in PR-B; PR-A only uses the flag to emit
	// the buffered-fallback deprecation log when an SSE-emitting app
	// lands on the legacy buffered path.
	streamingEnabled bool
	// synth is the internal unix-socket RPC server schedd dials for cron
	// dispatch (spec §4.4, M7). nil in tests; production wires it after
	// the schedd client is dialed.
	synth *gateway.SynthServer
	// egressGRPC is the ADR-046 PR-2 producer channel — a
	// *grpc.Server on a second unix socket dedicated to the egress
	// stream (one unix socket can serve either HTTP or gRPC, not
	// both; the synth socket stays HTTP). nil in tests; production
	// wires it after the Handler + EgressSink are constructed.
	egressGRPC *egressGRPCListener
	// lastSeen flushes per-instance last_request_at to schedd (spec §4.1). nil in
	// tests (the wake/routing path doesn't need it); production wires the
	// schedFlushSink.
	lastSeen gateway.LastSeenSink
	// metrics is the process-local *gateway.Metrics bundle (Prometheus
	// collectors owned by gatewayd — separate from pkg/wire/metrics.go
	// OpsMetrics, which gatewayd does not instantiate). Constructed in run()
	// before the handler is built so the handler + warm-hint consumer +
	// top-N sampler share the same registry. Tests leave it nil; every
	// downstream consumer accepts nil safely.
	metrics *gateway.Metrics
	// controlAddr is the loopback control-plane bind (default
	// 127.0.0.1:9090). Tests inject a free-port value via "127.0.0.1:0"
	// + the resolved listener so two tests in the same package don't race
	// for the hard-coded production port.
	controlAddr string
	// apidLoopback is the operator-configured upstream URL the apidProxy
	// forwards to (cfg.APIDLoopback / deploy/digitalocean/config/
	// gatewayd.toml `apid_loopback`). Empty in tests; run() populates it
	// from cfg before invoking prodRunWithDeps.
	apidLoopback string
	// writeTimeout is the http.Server.WriteTimeout override (issue #471 /
	// ADR-047). When 0, the legacy 300 s default (spec §4.1) applies.
	// run() resolves this from cfg.ResponseWriteTimeout || api.ResponseWriteTimeoutDefault
	// so a TOML-only override (production) and a Plan-default fallback
	// (PR-B's per-plan cap lift) both flow into the listener. nil in
	// tests; the test seam injects the bit directly.
	writeTimeout time.Duration
	// nodeCache holds the per-node *grpc.ClientConn cache plus the
	// compute_node_changed pg_notify subscriber (issue #98 / ADR-028).
	// nil in tests; production wires it after pgStore opens. PR
	// scale-out readiness: also the sink for the heartbeat goroutine
	// that bumps gateway_compute_node_changed_subscriber_alive every
	// subscriberHeartbeatInterval (30s); newNodeCache receives
	// deps.metrics for this purpose.
	nodeCache *nodeCache
	// pgStore is the shared state.Store; used by the githubd proxy
	// for the issue #294 replay dedupe and by the gatewayd audit
	// emitter. nil in tests (the githubdProxy then skips the
	// replay check).
	pgStore *state.PgStore
	// authMw is the pkg/auth.Middleware that powers the AppLogsHandler
	// (issue #254 / Move 4 PR-2). It hosts the bearer / session / MFA
	// / scope checks + the IDOR-safe LoadApp + the shared per-IP
	// AuthLimit bucket. nil in tests → the AppLogsHandler is constructed
	// in non-auth mode (the whitebox tests in cmd/gatewayd/app_logs_test.go
	// drive `serveAppLogs` directly with a stub stream and bypass the
	// chain). ADR-046.
	authMw *authmw.Middleware
	// apiAuthLimiter is the shared per-IP bucket that backs
	// `authMw.RequireLimited`. cmd/apid carries its own instance of
	// the same bucket so the spec §11 10/min/IP rule is per-process
	// (the gatewayd-edge counter is the load-bearing one for
	// internet-facing traffic; apid's bucket is the loopback-only
	// counter for management plane traffic). nil in tests.
	apiAuthLimiter *middleware.Limiter
	// sessions is the *session.Manager that issues + verifies the
	// session cookie. cmd/apid shares the same construct; the two
	// daemons are independent processes so the AEAD keys are loaded
	// separately per daemon. nil in tests.
	sessions *session.Manager
	// audit is the *pkg/audit.Auditor that emits the auth.mfa_gate_hit
	// / auth.session.stolen rows. nil in tests.
	audit *audit.Auditor
	// scheddClient is the ScheddClient interface (production: a
	// single *scheddgrpc.Client from the per-node cache) used by
	// AppLogsHandler (issue #254 / Move 4 PR-2) and the warm hint
	// stream consumer. Phase 2 / Gate A: production wires a single
	// client from the per-node cache; multi-stream fan-in is a v1.1
	// follow-up. nil in tests (the AppLogsHandler constructor
	// guards + the carve-out is silently disabled).
	scheddClient scheddgrpc.ScheddClient
	// warmHints is the long-lived StreamWarmHints consumer
	// (ADR-025 axis 4) that drains the sticky-warm affinity
	// stream from schedd and updates the picker's hint cache.
	// nil in tests; production wires it after the schedd client
	// is dialed so the consumer goroutine shares the existing
	// /run/faas/schedd.sock connection (no second dial).
	warmHints *warmHintConsumer
	// egressTLS is the server mTLS config the egress gRPC listener
	// uses when meterd dials it from a remote compute node (ADR-052).
	// nil in tests; production wires it in run() from
	// cfg.LoadEgressTLS() so the [egress_tls_*] TOML keys apply
	// before the listener binds.
	egressTLS *tls.Config
	// scheddRouter (Phase 2 / Gate A) is the per-node schedd gRPC
	// client cache. Wired in run() from newScheddRouter. nil in
	// tests — the legacy single-sched path stays in effect.
	scheddRouter *scheddRouter
}

func prodDefaultDeps() prodRunDeps {
	return prodRunDeps{
		listen:      net.Listen,
		newSrv:      defaultServer,
		backend:     unwiredBackend{},
		controlAddr: controlAddr,
	}
}

func defaultServer(addr string, handler http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
	}
}

// prodRun is the production run() body moved verbatim from
// cmd/gatewayd/main.go. The `prod` prefix avoids colliding with
// the placeholder's `run` in cmd/gatewayd-internal/main.go (PR-A
// keeps the placeholder serving TEMPLATE_OK so wait_healthy stays
// green; PR-B drops the placeholder and renames prodRun → run).
func prodRun(ctx context.Context, log *slog.Logger) error {
	pool, err := db.Open(ctx, "")
	if err != nil {
		return fmt.Errorf("gatewayd: open db: %w", err)
	}
	defer pool.Close()

	cfg, err := LoadConfig(configPath)
	if err != nil {
		return err
	}

	// Phase 2 / Gate A: per-node schedd client cache. Wires the
	// production dial closure (cross-box via pkg/overlay for tcp
	// targets; unix for single-box). The router's WatchNodeChanges
	// goroutine maintains the cache from compute_node_changed
	// pg_notify. The legacy FAAS_SCHEDD_SOCKET dial is still wired
	// below as a fallback for the warm-hint stream consumer and the
	// AppLogsHandler (single-stream consumers; multi-stream fan-in
	// is a v1.1 follow-up).
	pgStore := state.NewPgStore(pool)
	vmmdTLS, err := cfg.LoadVMMDPingTLS()
	if err != nil {
		return fmt.Errorf("gatewayd: load vmmd TLS: %w", err)
	}
	deps := prodDefaultDeps()
	deps.scheddRouter = newScheddRouter(pgStore, vmmdTLS, nil, log)
	go deps.scheddRouter.WatchNodeChanges(ctx, pool, nil)
	// Single-stream fallback: dial the legacy schedd socket once for
	// the consumers that don't currently fan-in (warm hints, log
	// stream). On the single-box default-local fleet this dials the
	// same client the router would resolve; on a multi-box install
	// the stream comes from one schedd only and is documented as
	// such in cmd/gatewayd/warmhints.go.
	sched, err := scheddgrpc.DialContext(ctx, scheddSocket, nil)
	if err != nil {
		return fmt.Errorf("gatewayd: dial schedd: %w", err)
	}
	defer func() { _ = sched.Close() }()

	// Env-derived AppsDomain wins over the TOML file so the e2e harness can
	// run without writing a TOML. The legacy path is plain :8080 with the
	// suffix filter on; the production path is certmagic + :443/:80.
	appsDomain := os.Getenv("FAAS_APPS_DOMAIN")
	if appsDomain == "" {
		appsDomain = cfg.AppsDomain
	}
	router := pgRouter{store: pgStore, appsSuffix: appsSuffix(appsDomain)}
	// ADR-025 axis 4: sticky-warm affinity cache. Built first so the
	// picker's WarmHintFunc reads from it on every Pick. The cache
	// itself is empty at this point — it gets populated by the
	// warmHintConsumer goroutine below as the schedd stream delivers
	// events. An empty cache degrades to per-node healthyCount
	// scoring, identical to a fresh install (ADR-005 cold boot must
	// always work).
	warmHintCache := gateway.NewWarmHintCache()
	backend := gateway.NewPGBackend(router, sched, log).
		WithWarmHint(warmHintCache.HintFunc()).
		WithAppResolver(func(ctx context.Context, appID string) (gateway.App, bool, error) {
			app, err := pgStore.AppByID(ctx, appID)
			if err != nil {
				if errors.Is(err, state.ErrNotFound) {
					return gateway.App{}, false, nil
				}
				return gateway.App{}, false, err
			}
			acct, err := pgStore.AccountByID(ctx, app.AccountID)
			if err != nil {
				return gateway.App{}, false, err
			}
			return gateway.App{ID: app.ID, AccountID: acct.ID, Plan: acct.Plan, StreamingEnabled: app.StreamingEnabled, NodeID: app.NodeID}, true, nil
		}).
		WithClientForApp(func(ctx context.Context, app gateway.App) (gateway.Scheduler, bool, error) {
			full, err := pgStore.AppByID(ctx, app.ID)
			if err != nil {
				return nil, false, err
			}
			cli, err := deps.scheddRouter.ScheddForApp(ctx, full)
			if err != nil {
				return nil, false, err
			}
			return cli, true, nil
		})

	// Phase 2 / Gate A: gate the resolveSched legacy fallback on the
	// active fleet. Single-box posture (only default-local active)
	// keeps the legacy fallback — every app is owned by the local
	// schedd, so a transient resolver miss is harmless. Multi-box
	// posture (any non-default-local active row) DISABLES the
	// fallback — b.sched is the legacy default-local dial and would
	// return FailedPrecondition for any foreign-owned app on transient
	// miss, surfacing as a 503 storm (PR #509 review finding F1).
	//
	// The posture check runs once at startup; subsequent compute_node
	// changes that flip the posture are out of scope (an operator
	// adding a second box restarts gatewayd alongside schedd).
	if nodes, err := pgStore.ActiveComputeNodes(ctx); err == nil {
		multiBox := false
		for _, n := range nodes {
			if n.Name != "default-local" {
				multiBox = true
				break
			}
		}
		backend.WithLegacySingleBox(!multiBox)
		log.Info("gatewayd: schedd posture", "legacy_single_box", !multiBox, "active_nodes", len(nodes))
	} else {
		log.Warn("gatewayd: schedd posture probe failed; defaulting to legacy single-box fallback", "err", err)
		backend.WithLegacySingleBox(true)
	}

	// Keep the routing + target caches fresh from apid/schedd's pg_notify
	// stream (spec §4.1): an instance state change evicts the app's cached
	// target so the next request re-resolves via an idempotent wake; an app or
	// domain change flushes the host→app routes.
	go watchInvalidations(ctx, pool, backend, log)

	deps.backend = backend
	// Flush per-instance last_request_at to schedd so its idle reaper sees
	// gateway traffic (spec §4.1, ADR-018) — without this a busy app parks once
	// its idle timer fires. schedd is the sole writer to `instances`, so we hand
	// it the batch over gRPC (the same client we wake through). Issue #168
	// dropped the addr→instance resolver hop: the handler now Touches by
	// instance_id directly, and schedd drops unknown ids on its side.
	// Phase 2 / Gate A: the flush is partitioned by instance.node_id
	// via the per-node router so touches land on the owner schedd.
	deps.lastSeen = newSchedFlushSink(deps.scheddRouter, pgStore, log)
	// Internal unix-socket RPC for schedd's cron dispatch loop (spec §4.4,
	// M7). Routes a synthetic wake through schedd so metering + the
	// per-minute sampler see the live instance. lastSeen-touches for cron
	// traffic land in a follow-up once we expose an app-scoped touch RPC
	// (today schedd's ReportActivity takes instance_ids, which the synth
	// path doesn't have without a Wake first). Phase 2 / Gate A: synth
	// calls resolve the owner schedd per app via the router — they no
	// longer dial the legacy single schedd.
	deps.synth = gateway.NewSynthServer(gatewaydInternalSocket, &synthAdapter{
		wake: func(ctx context.Context, appID string) error {
			// wake_id is discarded on the synth path (gaps analysis
			// 2026-07-23): synthesized requests don't return a
			// response header to a customer, so x-faas-wake-id has
			// no consumer here. AdmitInstance is still called for
			// the admit + boot side effects.
			//
			// method is discarded for the same reason: the
			// wake-locality counter is observed at the public
			// handler's after-proxy chokepoint, not on the synth
			// path. Synth traffic is operational (cron, per-minute
			// sampler), not customer traffic, and would skew the
			// "what fraction of admissions were local" reading.
			//
			// Phase 2 / Gate A: resolve the owner schedd via the
			// per-node router so the cron path lands on the right
			// box.
			app, err := pgStore.AppByID(ctx, appID)
			if err != nil {
				return err
			}
			cli, err := deps.scheddRouter.ScheddForApp(ctx, app)
			if err != nil {
				return err
			}
			_, _, _, _, _, _, err = cli.AdmitInstance(ctx, appID)
			return err
		},
		// Move 1: Wake the instance, then route the synthetic
		// envelope. The wake gate handles admit + boot; the
		// envelope delivery to the runner is the per-app internal
		// queue Move 2 introduces. For now the drain records the
		// post-state as 'dispatching' — the row's result is set
		// later (or left NULL for async sources) by the drain's
		// CompleteInvocation call. The live instance handle from
		// the Wake response is echoed back on the returned
		// Invocation so schedd can StampInstanceInvocation —
		// without it the meter's per-instance count lands on 0.
		invoke: func(ctx context.Context, appID string, inv state.Invocation) (state.Invocation, error) {
			app, err := pgStore.AppByID(ctx, appID)
			if err != nil {
				return inv, fmt.Errorf("synth invoke resolve app %s: %w", appID, err)
			}
			cli, err := deps.scheddRouter.ScheddForApp(ctx, app)
			if err != nil {
				return inv, fmt.Errorf("synth invoke resolve schedd %s: %w", appID, err)
			}
			instanceID, _, _, _, err := cli.Wake(ctx, appID)
			if err != nil {
				return inv, fmt.Errorf("synth invoke wake %s: %w", appID, err)
			}
			inv.InstanceID = instanceID
			inv.State = state.InvocationDispatching
			return inv, nil
		},
	}, log)
	// Process-local Prometheus registry (spec §12). Constructed here so
	// every downstream consumer — handler, warm-hint consumer, top-N
	// sampler — shares the same registry. The registry is exposed via
	// :9090/metrics by gateway.RunControlServer; the gate/handler pick
	// it up via deps.metrics in prodRunWithDeps.
	//
	// ADR-068 / Tier A7 split: TLS termination moves to gatewayd-public
	// (certmagic, httpsec, :443/:80 ACME mux). The legacy binary stays
	// plain HTTP on :8080; PR-A removes the resolved-TLS branch entirely.
	deps.metrics = gateway.NewMetrics()

	// Forward the operator-configured apid loopback URL through the
	// test seam so prodRunWithDeps can stay TOML-free (issue #85).
	deps.apidLoopback = cfg.APIDLoopback
	// Issue #98 / ADR-028, plumbed via issue #120: per-node vmmd
	// client cache. The dial closure routes through pkg/overlay so the
	// cross-box dial primitive lives in one place; mTLS material is
	// loaded from the [vmmd_tls_*] TOML keys (mirroring schedd's
	// cmd/schedd/config.go LoadVMMTLS). For single-box deployments
	// all three paths are empty and LoadVMMDPingTLS returns nil,
	// which overlay.Dial (and the underlying wire.DialContext)
	// accepts on unix targets. Subscribing to compute_node_changed
	// runs in a goroutine that dies with ctx.
	// (vmmdTLS was loaded earlier in run() — see the scheddRouter
	// wiring above. The same TLS bundle is shared with the vmmd
	// node cache.)
	// ADR-052: load the server mTLS material the egress gRPC
	// listener uses when meterd dials it from a remote compute
	// node. Empty cluster returns (nil, nil); partial cluster is
	// rejected with the egress_tls_* field names.
	deps.egressTLS, err = cfg.LoadEgressTLS()
	if err != nil {
		return fmt.Errorf("gatewayd: load egress TLS: %w", err)
	}
	// PR scale-out readiness: thread *gateway.Metrics into the node
	// cache so the WatchEvictions heartbeat goroutine has a sink for
	// gateway_compute_node_changed_subscriber_alive. deps.metrics is
	// always non-nil after NewMetrics (cmd/gatewayd/main.go:284).
	//
	// issue #517 / PR-C / ADR-064: thread the events Platform into
	// the node cache so the forwarder emits wake.proxy_first_byte
	// on the first downstream byte. The Platform writes the events
	// row + bumps wake_phase_emitted_total{wake.proxy_first_byte}.
	// We do NOT pass a Broadcaster — gatewayd is the public
	// listener (CLAUDE.md ownership), not an SSE fan-out source.
	gatewayOps := wire.NewOpsMetrics("gatewayd")
	eventsPlatform := events.NewPlatform("gatewayd", pgStore, log, gatewayOps, nil)
	deps.nodeCache = newNodeCache(pgStore, vmmdTLS, log, deps.metrics).WithEvents(eventsPlatform)
	go deps.nodeCache.WatchEvictions(ctx, pool)
	deps.pgStore = pgStore
	// Issue #471 / ADR-047 (PR-A): merge the per-process streaming
	// opt-in from TOML + env. Either source flips the bit. Production
	// default is false (no streaming — the legacy buffered path).
	// Tests override by setting one of the two knobs.
	deps.streamingEnabled = cfg.StreamingEnabled || streamingEnabledFromEnv()
	// Issue #471 / ADR-047 (PR-A): resolve the http.Server.WriteTimeout.
	// Spec §4.1 baseline is 300 s; the TOML [response_write_timeout]
	// key overrides per-cluster. PR-B lifts the per-plan cap on
	// Hobby+ to 900 s; that path flips deps.writeTimeout to the
	// per-app app.Plan.ResponseWriteTimeout() at request-init time
	// (http.Server.WriteTimeout is global, so the per-app lift needs
	// per-request bookkeeping via http.ResponseController — out of
	// scope here). 0 means "spec default"; prodRunWithDeps fills it in.
	if cfg.ResponseWriteTimeout > 0 {
		deps.writeTimeout = cfg.ResponseWriteTimeout
	}
	// Issue #254 / Move 4 PR-2: pkg/auth.Middleware construction.
	// AppLogsHandler (cmd/gatewayd/app_logs.go) shares the auth chain
	// with cmd/apid via ADR-046 — same bearer / session / MFA / scope
	// / IDOR-safe LoadApp, same per-IP AuthLimit bucket family. The
	// session manager is loaded from FAAS_SESSION_KEY (see
	// session_key.go); the auditor is the same pkg/audit.Auditor
	// gatewayd already uses for githubd replay-dedupe audit rows
	// (cmd/gatewayd/auditor.go). nil-safe when the env is unset so
	// the e2e harness + these dev-mode paths still boot.
	sessions := loadSessionManager(osGetenv, log)
	if sessions == nil {
		return fmt.Errorf("gatewayd: session manager init failed")
	}
	deps.sessions = sessions
	deps.apiAuthLimiter = middleware.NewLimiter(middleware.AuthLimitConfig{Log: log})
	deps.audit = audit.New(pgStore, log, nil, "gatewayd")
	deps.authMw = authmw.New(
		storeAsAuthenticator(pgStore),
		sessions,
		storeAsSessionLookup(pgStore),
		auditorAsAuthAuditor(deps.audit),
		log,
		deps.apiAuthLimiter,
	)
	// The scheddClient reference is needed by AppLogsHandler (PR-2).
	// It outlives `run` because we want the AppLogsHandler to keep a
	// pointer to the same client; defers Close.
	deps.scheddClient = sched
	// ADR-025 axis 4: StreamWarmHints consumer. Long-lived
	// goroutine under the same ctx as the rest of the daemon —
	// drains the sticky-warm affinity stream from schedd into
	// the picker's hint cache. Reconnects with backoff on
	// transient errors; freezes the cache on persistent errors
	// (Phase 3 review policy). nil in tests (the e2e harness
	// doesn't drive the stream; the picker falls through to
	// per-node healthyCount as it always did).
	deps.warmHints = newWarmHintConsumer(sched, warmHintCache, log)
	go deps.warmHints.Run(ctx)
	return prodRunWithDeps(ctx, log, deps)
}

// prodRunWithDeps is the test-friendly variant. It exercises:
//
//   - public listen on listenAddr via deps.listen / deps.newSrv (DI seam)
//   - control listen on controlAddr via gateway.RunControlServer
//   - SIGHUP-triggered rate-limit-bucket reset (same behaviour as production)
//
// Production calls run → prodRunWithDeps(prodDefaultDeps()); tests inject a custom
// deps.listen so they can probe a real socket without binding :8080.
func prodRunWithDeps(ctx context.Context, log *slog.Logger, deps prodRunDeps) error {
	// ADR-068 / Tier A7 split: TLS termination moved to gatewayd-public.
	// The legacy daemon always serves plain HTTP on :8080 (the e2e
	// harness path); production traffic terminates TLS at gatewayd-public
	// and proxies to the unix socket bound in cmd/gatewayd-internal/.

	handler := gateway.NewHandlerWith(deps.backend, deps.metrics, log)
	handler.SetWakeGateHook()
	// Issue #471 / ADR-047: per-process streaming opt-in. PR-A wires
	// the flag end-to-end but the actual Flusher path ships in PR-B;
	// while PR-A the Handler still buffers every response (so
	// behaviour is unchanged). The buffered-fallback AC #4 needs the
	// flag to flow into the Handler so a misconfigured SSE-emitting
	// app surfaces the once-per-process deprecation log instead of a
	// silent buffered blob. The flag here is the merged
	// (cfg.StreamingEnabled || env(FAAS_GATEWAY_STREAMING)) — run()
	// populates deps.streamingEnabled; tests inject the bit directly.
	handler.WithStreamingEnabled(deps.streamingEnabled)
	// ADR-046 PR-2: per-instance egress ring buffer + the gRPC
	// producer channel. The sink is shared between Handler.recordEgress
	// (writer) and egressgrpc.Server (drainer-on-cadence reader).
	// The gRPC server runs on a second unix socket (FAAS_GATEWAY_EGRESS_SOCKET)
	// because the existing synth socket serves an HTTP/1.1 listener
	// (pkg/gateway/synth.go) and a single unix socket cannot host both
	// HTTP and gRPC simultaneously; the DAC auth posture (ADR-015) is
	// identical — group `faas` ownership + 0660 mode. netns dialer is
	// unchanged.
	egressSink := egresssink.NewEgressSink()
	handler.WithEgressSink(egressSink)
	egressGRPCSocket := egressGRPCSocketPath()
	// Reject the silent-no-TLS path: a non-unix target without TLS
	// would build an insecure server (ADR-052). deps.egressTLS is
	// loaded in run() — see TomlTLSLoad order below.
	if !isUnixSocketPath(egressGRPCSocket) && deps.egressTLS == nil {
		return fmt.Errorf("gatewayd: egress target %q is non-unix but egress_tls_* is empty (set egress_tls_cert_path / key_path / ca_path or point the target at a unix socket for single-box mode)", egressGRPCSocket)
	}
	egressGRPCSrv := egressgrpc.NewServer(egressSink, log)
	deps.egressGRPC = newEgressGRPCListener(egressGRPCSocket, deps.egressTLS, egressGRPCSrv, log)
	// Best-effort start, mirroring the synth listener pattern
	// (prodRunWithDeps internal RPC). If the unix socket can't bind
	// (e.g. /run/faas doesn't exist on a dev/test box), log + continue
	// — the public + control listeners are still up, and the
	// per-instance egress sink continues to accumulate in memory
	// even though no consumer can dial the stream. The meterd side
	// reports ok=false on every EgressBytes read during the gap;
	// AppendUsage writes 0 to tx_bytes for that minute. Once the
	// socket becomes bindable (deploy-time /run/faas exists), the
	// next daemon restart picks up the stream automatically.
	if err := deps.egressGRPC.start(ctx); err != nil {
		log.Warn("gatewayd egress listen failed; tx_bytes will stay 0 until restart",
			"socket", egressGRPCSocket, "err", err)
		deps.egressGRPC = nil
	}
	//nolint:contextcheck // shutdown ctx must outlive the cancelled caller ctx per net/http + gRPC GracefulStop contract.
	defer func() {
		if deps.egressGRPC == nil {
			return
		}
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = deps.egressGRPC.stop(shutdownCtx)
	}()
	// Issue #300: topNSampler drives the gateway_top_tenant_rps
	// gauge from the rolling per-app count fed by Handler.observe
	// (pkg/gateway/handler.go:observe). 5s tick; runs for the
	// daemon's lifetime; stops cleanly on ctx cancel. Local
	// topAccountSet mirrors pkg/wire/topn.go so pkg/gateway stays
	// free of any dependency on pkg/wire (avoids an import cycle
	// through cmd/gatewayd → pkg/gateway → pkg/wire → cmd/gatewayd).
	gatewayTopNSampler := newTopNSampler(handler.Metrics(), log)
	go gatewayTopNSampler.run(ctx)

	// Issue #98 / ADR-028 + ADR-047: install the per-node HTTP→gRPC
	// forwarder. Backend.Target returns a compute_node.id (string-typed
	// for backwards compat); the forwarder dereferences it via the
	// per-node vmmd client cache and bridges HTTP bytes to the
	// instance netns through vmmd's bidi ForwardHTTPStream RPC. nil
	// cache = legacy addr-based path (tests + e2e harness without
	// vmmd overlay).
	if deps.nodeCache != nil {
		handler.WithForwarding(deps.nodeCache.Forwarding())
	}

	// Per-instance last_request_at flush loop (spec §4.1). Present in production;
	// nil in tests. FlushEvery stops with ctx; drain its error channel so a flaky
	// schedd logs rather than leaks a goroutine.
	if deps.lastSeen != nil {
		handler.WithLastSeenSink(deps.lastSeen)
		errc := gateway.FlushEvery(ctx, lastSeenFlushInterval, deps.lastSeen)
		go func() {
			for range errc {
			}
		}()
	}

	// SIGHUP = "drop in-memory rate-limit buckets". Operators use this after
	// a mass-app-delete (apid → publish app.deleted; once M5 ships the LISTEN
	// channel, SIGHUP becomes the manual fallback). It's also safe to send
	// if rate-limit state ever drifts.
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	defer signal.Stop(hup)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-hup:
				// Reset both per-app and per-account buckets (ADR-040).
				// appDropped = per-app buckets cleared; acctDropped =
				// per-account buckets cleared. Operators see the sum.
				appDropped := handler.Limiter().ForgetAll()
				acctDropped := handler.AccountLimiter().ForgetAll()
				log.Info("gatewayd sighup reload",
					"action", "rate_limit_buckets_dropped",
					"app_count", appDropped,
					"account_count", acctDropped,
					"count", appDropped+acctDropped)
			}
		}
	}()

	// Public listener: customer traffic (spec §4.1). The handler is
	// wrapped in an apidProxy (M7.5, ADR-011, broadened in issue #85)
	// so the full apid public surface — /dashboard/*, /oauth/*,
	// /v1/*, /login*, /auth/verify, /logout, /status*, /healthz —
	// reverse-proxies to apid's loopback listener. Everything else
	// falls through to gateway.Handler's wake/proxy flow.
	//
	// Target resolution (issue #85): the TOML-loaded
	// cfg.APIDLoopback (threaded via deps.apidLoopback) wins;
	// FAAS_APID_LOOPBACK env is kept as a fallback so the e2e
	// harness (no TOML) keeps working.
	apidTarget := deps.apidLoopback
	if apidTarget == "" {
		apidTarget = os.Getenv("FAAS_APID_LOOPBACK")
	}
	if apidTarget == "" {
		apidTarget = "http://127.0.0.1:8081"
	}
	// Issue #254 / Move 4 PR-2: the AppLogsHandler claims the
	// customer-facing `GET /v1/apps/{slug}/logs` route. gatewayd
	// already imports pkg/scheddgrpc (ADR-018) so the schedd dial
	// stays in one daemon (depguard apid-control-plane-only blocks
	// cmd/apid from importing pkg/scheddgrpc). The auth chain
	// (bearer / session / MFA / scope / IDOR-safe LoadApp) is
	// shared with cmd/apid via pkg/auth.Middleware (ADR-046), so
	// the spec §11 10/min/IP rule covers both gateways.
	//
	// The handler is mounted on a tiny *http.ServeMux so the
	// `{slug}` path parameter binds (the AppLogsHandler reads
	// r.PathValue("slug")). The mux is wrapped by the apidProxy
	// carve-out (cmd/gatewayd/proxy.go::isApidLogsPath) so the
	// public listener dispatches the path straight to the handler
	// without consulting pkg/gateway.Handler's wake/proxy logic
	// (the route is hostname-agnostic — it owns /v1/apps/{slug}/logs
	// regardless of the Host header).
	//
	// The handler is wired only when deps.authMw is non-nil — tests
	// default deps.authMw to nil and the carve-out is silently
	// disabled (the path falls through to apid, matching the
	// pre-PR-2 behaviour).
	var logsHandler http.Handler
	if deps.authMw != nil && deps.pgStore != nil && deps.scheddRouter != nil {
		logsMux := http.NewServeMux()
		logsMux.Handle("GET /v1/apps/{slug}/logs", &AppLogsHandler{
			Auth: deps.authMw,
			// Phase 2 / Gate A: dial the owner schedd via the
			// per-node router. The legacy scheddClient field is
			// retained for the warm-hint stream (single-stream
			// fallback); the log stream is the second fan-in
			// consumer and resolves per-app.
			ScheddFor: appLogsScheddResolver{store: deps.pgStore, router: deps.scheddRouter},
			Store:     deps.pgStore,
			Log:       log,
			Ops:       nil,
		})
		logsHandler = logsMux
	}
	apidHandler := newApidProxyWithLogs(apidTarget, handler, logsHandler, log)

	// Slice 7: githubd webhook HMAC-verify at the edge, then proxy
	// to githubd's loopback listener (ADR-012, §11 single-public-
	// listener invariant). githubd stays loopback-only so the
	// webhook secret is the only secret on this path that has to
	// leave githubd's own config (it doesn't — it lives in
	// gatewayd's env so the verify happens before the proxy hop).
	githubdTarget := os.Getenv("FAAS_GITHUBD_LOOPBACK")
	if githubdTarget == "" {
		githubdTarget = "http://127.0.0.1:8083"
	}
	githubdSecret := loadGithubWebhookSecret(osGetenv)
	// Issue #294: wire the githubd proxy with the dedupe check and
	// the audit emitter. The replay interface is satisfied by
	// *state.PgStore; the auditStore interface is also satisfied by
	// *state.PgStore (compile-time checked in audit.go). nil
	// `deps.pgStore` (tests) skips the replay check, matching the
	// pre-#294 behaviour.
	publicHandler := newGithubdProxy(githubdTarget, githubdSecret, apidHandler, log, newGatewaydAuditor(deps.pgStore, log))

	// Issue #249 / spec §11: mount security response headers at the
	// outermost wrapper of the public listener.
	//
	// httpsec.Static emits the five static headers (HSTS, X-Frame-
	// Options, X-Content-Type-Options, Referrer-Policy, Permissions-
	// Policy) on every response — customer apps, dashboard, API, even
	// githubd webhooks. Static headers are universally safe; the
	// customer's own CSP just sits on top.
	//
	// httpsec.Nonce gates Content-Security-Policy on isApidPath so it
	// only fires on URLs that hit apid. Customer-app responses are
	// proxied through our wake/proxy path — those apps govern their
	// own CSP, and injecting a nonce-locked policy here would break
	// every customer HTML page (issue #249 risk callout).
	// isApidPath lives in cmd/gatewayd/proxy.go (path-string
	// predicate); the httpsec.Nonce gate takes a *http.Request, so
	// the adapter below extracts r.URL.Path and forwards.
	//
	// The order is httpsec.Nonce outer / httpsec.Static inner so the
	// CSP can be set before Static runs (it doesn't matter for the
	// static headers, but keeps the middleware chain readable).
	publicHandler = httpsec.Static(httpsec.Nonce(
		func(r *http.Request) bool { return isApidPath(r.URL.Path) },
		publicHandler,
	))

	// Issue #249: HSTS gate. RFC 6797 §7.2 says UAs ignore HSTS on
	// plain HTTP, so on a dev plaintext listener this is cosmetic.
	// Production TLS listener always emits it (the default).
	httpsec.SetHSTSEnabled(httpsec.HSTSEnabledFromEnv(osGetenv))

	// Private listener: control plane only — never authenticated (it's on a
	// private bind), never reachable from the public-internet path.
	//
	// Issue #568 / ADR-070 (Tier A7 edge split): the pre-split daemon
	// wired `nil` here, which made /readyz return 200 unconditionally.
	// That was acceptable for single-box (no LB to drain) but fails
	// closed wrong after the split: a partial-boot daemon would
	// happily accept traffic even though the routing cache, the cert-
	// bundle, and the warm-hint subscription are not yet ready.
	//
	// The pre-split daemon's readiness contract is the loosest of the
	// three flavours (gatewayd-public, gatewayd-internal, legacy
	// gatewayd): "we have a Postgres connection OR we're a unit test
	// that intentionally skips one". The split daemons (tasks #17,
	// #18) wire the full ReadyzProbe from pkg/gateway/readiness.go
	// with hydration tracking, schedd-router readiness, and PG ping.
	// Here we wire the legacy one — "deps.pgStore != nil" — which is
	// the strongest signal the pre-split daemon can honestly assert
	// (the route cache is lazy-fill; the only "I cannot serve" state
	// the legacy code has is "PG connection missing").
	legacyReady := gateway.ReadyFunc(func() bool {
		return deps.pgStore != nil
	})
	controlMux := gateway.ControlMux(handler.Metrics(), legacyReady)
	// Finding 6 (issue #314): mount the dashboard quota endpoint on the
	// control mux so an in-box caller (operator's curl today, future
	// apid-side dial) can read per-app bucket state without going through
	// the public :443 listener — that path self-rate-limits. The handler
	// reads from the same *Limiter the public edge uses (handler.Limiter()
	// is the seam) so the snapshot agrees with what Allow consumed.
	controlMux.HandleFunc("/v1/internal/quota", internalQuotaHandler(handler, log))

	// Track every *http.Server we spin up so the shutdown path can drain
	// them in parallel. sslib guidance is "call Shutdown on each" rather
	// than Close: Shutdown lets in-flight requests finish; Close does not.
	errc := make(chan error, 4)
	var servers []*http.Server
	addSrv := func(s *http.Server) { servers = append(servers, s) }
	// Legacy plain-:8080 path. Existing e2e harness depends on this.
	// ADR-068 / Tier A7 split: TLS termination moved to gatewayd-public
	// (certmagic, httpsec, :443/:80 ACME mux). After PR-A this daemon no
	// longer builds a *gateway.TLSBundle; PR-B removes the public :8080
	// listener entirely when the unix-socket handler chain lands in
	// cmd/gatewayd-internal/.
	srv := deps.newSrv(listenAddr, publicHandler)
	public := srv
	public.Addr = listenAddr
	if public.ReadTimeout == 0 {
		public.ReadTimeout = 60 * time.Second
	}
	if public.WriteTimeout == 0 {
		// Issue #471 / ADR-047 (PR-A): honour the TOML override
		// (cfg.ResponseWriteTimeout, propagated via prodRunDeps). 0
		// means "use the spec §4.1 baseline" — see run() for the
		// precedence. PR-B lifts the Hobby+ per-plan cap to 900 s
		// via http.ResponseController and a per-request timeout
		// (http.Server.WriteTimeout is global, so the per-app
		// override can never land at this layer).
		public.WriteTimeout = writeTimeoutOrDefault(deps.writeTimeout)
	}
	addSrv(public)
	l, lerr := deps.listen("tcp", listenAddr)
	if lerr != nil {
		log.Error("gatewayd public listen failed", "addr", listenAddr, "err", lerr)
		return lerr
	}
	go func() {
		log.Info("gatewayd public listening", "addr", listenAddr)
		if err := public.Serve(l); err != nil && err != http.ErrServerClosed {
			errc <- err
		}
	}()
	ctrlAddr := controlAddr
	if deps.controlAddr != "" {
		ctrlAddr = deps.controlAddr
	}
	// PR #218 review finding: refuse non-loopback binds for the control
	// listener when the env knob was used. The control mux is
	// unauthenticated by design (operator-prometheus scrape), so an
	// accidental public bind would expose /metrics. The default address
	// is already loopback so this only fires for misconfigured overrides
	// (and silently no-ops for in-process tests that inject via
	// deps.controlAddr on a loopback listener).
	if os.Getenv("FAAS_GATEWAY_CONTROL_LISTEN") != "" {
		if err := assertLoopbackBind(ctrlAddr); err != nil {
			return fmt.Errorf("gatewayd: FAAS_GATEWAY_CONTROL_LISTEN must bind loopback: %w", err)
		}
	}
	go func() {
		log.Info("gatewayd control listening", "addr", ctrlAddr)
		errc <- gateway.RunControlServer(ctx, ctrlAddr, controlMux)
	}()

	// Internal unix-socket RPC for schedd's cron dispatch (spec §4.4,
	// M7). Best-effort: if the socket can't bind (e.g. /run/faas
	// doesn't exist on a dev box), log and continue — the public +
	// control listeners are still up.
	if deps.synth != nil {
		if err := deps.synth.Start(); err != nil {
			log.Warn("gatewayd synth listen failed; cron traffic will fail until restart",
				"socket", gatewaydInternalSocket, "err", err)
		}
	}

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		//nolint:contextcheck // shutdown ctx must outlive the cancelled caller ctx (net/http contract).
		// Best-effort shutdown of every listener we may have started.
		// Servers track themselves in `servers`; certmagic owns its renew loop
		// on a separate goroutine — we Close the bundle now that the public +
		// ACME servers have drained. The bundle's Close is a no-op against
		// certmagic v0.25 (no public Stop API) but the seam lets a future
		// upgrade wire real shutdown without touching call sites.
		for _, s := range servers {
			_ = s.Shutdown(shutdownCtx)
		}
		// ADR-068 / Tier A7 split: tlsBundle.Close + tlsCertExpiryCancel
		// moved to gatewayd-public's shutdown path (cmd/gatewayd-public/main.go).
		if deps.synth != nil {
			//nolint:contextcheck // same shutdown-ctx contract as public.Shutdown above.
			_ = deps.synth.Stop(shutdownCtx)
		}
		if deps.nodeCache != nil {
			// Closing every cached *grpc.ClientConn here means
			// in-flight ForwardHTTPStream RPCs see a "transport
			// closing" error → handler maps it to 502; the
			// listener is already draining so no new requests
			// land.
			_ = deps.nodeCache.Close()
		}
		return nil
	}
}

// unwiredBackend routes nothing; every request 404s until the Postgres routing
// cache and schedd wake path are connected in M5.
type unwiredBackend struct{}

func (unwiredBackend) Lookup(context.Context, string) (gateway.App, bool) {
	return gateway.App{}, false
}
func (unwiredBackend) Pick(string) (gateway.Target, bool) { return gateway.Target{}, false }
func (unwiredBackend) HealthyCount(string) int            { return 0 }
func (unwiredBackend) Admit(context.Context, string, int) (string, gateway.WakeMethod, bool, error) {
	return "", gateway.WakeMethodUnspecified, false, nil
}

// envOrGateway returns the value of env key, or fallback when unset/empty.
// Named with the daemon prefix to avoid a collision if two daemons are ever
// linked into the same test binary.
func envOrGateway(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// writeTimeoutOrDefault resolves the http.Server.WriteTimeout the
// gatewayd public listener binds to (issue #471 / ADR-047 PR-A).
// The precedence is:
//
//  1. cfg.ResponseWriteTimeout (TOML)        — wire via prodRunDeps.writeTimeout
//  2. api.ResponseWriteTimeoutDefault        — spec §4.1 baseline (300 s)
//  3. 0 (the go interface default)           — never observed by callers
//
// Steps 1+2 collapse to the same constant when the TOML key is missing,
// so the call sites read "use the runtime value or fall back to spec";
// the helper is the single seam that picks the spec baseline so a
// future drift between pkg/api and the gatewayd default surfaces in
// one place rather than at every WriteTimeout literal.
func writeTimeoutOrDefault(d time.Duration) time.Duration {
	if d > 0 {
		return d
	}
	return time.Duration(api.ResponseWriteTimeoutDefault) * time.Second
}

// assertLoopbackBind rejects non-loopback control-listener addresses.
// The check accepts explicit 127.0.0.0/8 and ::1 forms plus the
// "localhost" hostname. Bare ":port" (empty host) is rejected —
// Listen on a bare ":port" binds 0.0.0.0, which is exactly what this
// guard exists to prevent. The PR #218 controlAddr env override only
// lands in the harness today, but a future operator footgun (env in a
// dev systemd unit) would otherwise be a silent /metrics exposure.
func assertLoopbackBind(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("parse %q: %w", addr, err)
	}
	switch host {
	case "localhost", "127.0.0.1", "::1":
		return nil
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return nil
	}
	return fmt.Errorf("control listener %q is not loopback; bind 127.0.0.1:9090 (or ::1) only", addr)
}
