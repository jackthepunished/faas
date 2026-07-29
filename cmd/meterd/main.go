// Command meterd — metering, billing, and quota enforcement (spec §4.7).
//
// meterd owns three timers that share one Postgres-backed state.Store:
//
//   - sample tick: every 60 s, walks every app's live instances and writes
//     one minute of billable usage (plan RAM + 8 MB) to usage_minutes.
//     The billable unit is the admission-time RAM, not the cgroup RSS —
//     spec §4.7 / CLAUDE.md invariant.
//   - quota tick: every 60 s, walks every account and applies the
//     per-plan ladder: Free at ≥100 % flips the account to suspended
//     and parks every live instance; paid plans emit a one-shot
//     quota_warning and accrue overage.
//   - stripe tick: every 24 h, pushes the past day's billable
//     mb_seconds to Stripe as a metered usage record with an
//     integer-arithmetic wire quantity (spec §4.7, ADR-010). The
//     per-day aggregate is the M7 §14 fix for the per-hour fractional
//     truncation that accumulated to ~0.3 % of the customer's bill —
//     above the spec's 0.1 % acceptance delta.
//
// meterd is the ONLY writer that triggers Free-tier hard stops — apid's
// auth gate and schedd's ledger just observe the resulting status.
package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"sync"
	"time"

	"filippo.io/age"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/onebox-faas/faas/pkg/alerts"
	"github.com/onebox-faas/faas/pkg/appmetrics"
	"github.com/onebox-faas/faas/pkg/audit"
	"github.com/onebox-faas/faas/pkg/billing"
	billingloader "github.com/onebox-faas/faas/pkg/billing/loader"
	"github.com/onebox-faas/faas/pkg/db"
	"github.com/onebox-faas/faas/pkg/mail"
	"github.com/onebox-faas/faas/pkg/meter"
	"github.com/onebox-faas/faas/pkg/promql"
	"github.com/onebox-faas/faas/pkg/scheddgrpc"
	"github.com/onebox-faas/faas/pkg/secretbox"
	"github.com/onebox-faas/faas/pkg/state"
	"github.com/onebox-faas/faas/pkg/webhookout"
	"github.com/onebox-faas/faas/pkg/wire"
)

// scheddCPUAdapter exposes the schedd gRPC client as a
// meter.CPUSource. The adapter is local to cmd/meterd so pkg/meter
// stays decoupled from pkg/scheddgrpc (one-way dependency the other
// way), and so a test fake can swap the client without touching
// pkg/meter's source. Issue #279 / PR-B.
//
// The adapter refreshes the per-instance snapshot on every call.
// meterd's sampler walks ~max_concurrency instances per minute, so
// the per-call cost is one gRPC ListInstanceStats round trip
// returning a slice of ~max_concurrency rows. The gRPC socket is
// on the box's local unix socket (ADR-015), so the cost is
// negligible vs. the 1-minute sampler cadence.
type scheddCPUAdapter struct {
	parker  parkInstanceParker
	now     func() time.Time
	mu      sync.Mutex
	rows    map[string]scheddgrpc.InstanceStatsRow
	fetched time.Time
}

const scheddCPUAdapterTTL = 30 * time.Second

func (a *scheddCPUAdapter) CPUUsageUsec(instanceID string) (uint64, bool) {
	a.refresh()
	a.mu.Lock()
	defer a.mu.Unlock()
	row, ok := a.rows[instanceID]
	if !ok {
		return 0, false
	}
	// CpuValid mirrors instancestats.Validity: 0 = Valid,
	// 1 = Unknown, 2 = Stale. The meterd sampler must NOT
	// treat the raw counter as a baseline on a non-Valid row
	// — the vmmd cpustats.Cache already absorbed the
	// regression (Unknown: it dropped the baseline) or
	// freshness budget exceeded (Stale: the reading is
	// older than the snapshot's freshness SLA). In either
	// case the next valid sample picks up from the new
	// counter; the meterd side just returns ok=false so
	// AppendUsage writes 0 cpu_usec for that minute.
	if row.CPUValid != 0 {
		return 0, false
	}
	return row.CPUUsageUsec, true
}

// refresh refreshes the in-memory snapshot if the last fetch is
// older than scheddCPUAdapterTTL. The cost is one gRPC round trip
// per minute per sampler iteration; the TTL bounds the staleness
// without forcing a fetch per instance.
func (a *scheddCPUAdapter) refresh() {
	a.mu.Lock()
	last := a.fetched
	a.mu.Unlock()
	if !last.IsZero() && a.now().Sub(last) < scheddCPUAdapterTTL {
		return
	}
	rows, err := a.parker.ListInstanceStats(context.Background())
	if err != nil {
		// Preserve the previous snapshot on error so a transient
		// gRPC failure doesn't drop the CPU data for the rest of
		// the minute. The next sample retries.
		//
		// If schedd is down for longer than ~one minute, the
		// snapshot is stale by a wider margin: the per-instance
		// CPU counters on the schedd side are advancing (or
		// wrapping on regression) but the adapter keeps
		// returning the last-known values. The per-minute
		// AppendUsage write will silently under-count until the
		// next successful refresh. This is a known
		// silent-under-count; the operator can see it via the
		// schedd /metrics and the alert pipeline (M8 row 2
		// will add a `schedd_instance_stats_collect_failures_total`
		// tripwire).
		return
	}
	m := make(map[string]scheddgrpc.InstanceStatsRow, len(rows))
	for _, r := range rows {
		m[r.InstanceID] = r
	}
	a.mu.Lock()
	a.rows = m
	a.fetched = a.now()
	a.mu.Unlock()
}

// parkInstanceParker is the slice of scheddgrpc.Client meterd actually
// uses. Slice 4 adds ParkInstance to scheddgrpc; in tests we inject a
// recording stub. Defining the interface here keeps meterd independent
// of pkg/scheddgrpc until the surface exists (ADR-019).
type parkInstanceParker interface {
	ParkInstance(ctx context.Context, instanceID, reason string) error
	// ListInstanceStats is the per-instance CPU-µs snapshot the
	// meterd sampler reads once per minute. Issue #279 / PR-B.
	// Returns an empty slice when schedd has no rows for this
	// tick (boot, between ticks); the sampler treats that as
	// "no CPU data this minute" and writes 0. ADR-046 (PR-2)
	// extends the returned row with NetTxBytes + TxValid so
	// the scheddEgressAdapter below can read the net_tx_bytes
	// value alongside cpu_usec on the same gRPC round trip.
	ListInstanceStats(ctx context.Context) ([]scheddgrpc.InstanceStatsRow, error)
}

// scheddEgressAdapter (ADR-046, step 8) exposes the schedd gRPC
// client as a meter.EgressSource for the net_tx_bytes column
// (root-side vethHost.rx_bytes). It reuses the scheddCPUAdapter
// 's snapshot machinery so the egress and CPU readings share a
// single gRPC round trip and refresh cadence.
//
// The tx_bytes column (gateway response bytes) is sourced from
// gatewayEgressAdapter below — the two columns are NOT the
// same data and the schedd wire only carries net_tx_bytes
// (vmmd is the canonical producer for that column; the
// gateway is the canonical producer for tx_bytes).
type scheddEgressAdapter struct {
	cpu *scheddCPUAdapter
}

func (a *scheddEgressAdapter) EgressBytes(instanceID string) (uint64, uint64, bool) {
	if a == nil || a.cpu == nil {
		return 0, 0, false
	}
	a.cpu.refresh()
	a.cpu.mu.Lock()
	defer a.cpu.mu.Unlock()
	row, ok := a.cpu.rows[instanceID]
	if !ok {
		return 0, 0, false
	}
	// TxValid mirrors instancestats.Validity: 0 = Valid, 1 =
	// Unknown (first sample / regression / netstats cache
	// miss). The meterd sampler must NOT treat a non-Valid
	// row as a baseline — the vmmd netstats.Cache already
	// absorbed the regression (Unknown: it dropped the
	// baseline). In either case the next valid sample picks
	// up from the new counter; the meterd side returns
	// ok=false so AppendUsage writes 0 net_tx_bytes for
	// that minute (mirrors the cpu path's contract above).
	if row.TxValid != 0 {
		return 0, 0, false
	}
	// txBytes = 0 (gateway column is NOT sourced from schedd;
	// gatewayEgressAdapter owns it). netTxBytes = the schedd
	// value. ok = true signals "I have a row" so the
	// sampler stamps the row's netTxBytes even when
	// netTxBytes is 0 (zero egress in this tick is a real
	// value, distinct from "no source wired").
	return 0, row.NetTxBytes, true
}

// gatewayEgressAdapter (ADR-046, step 8) is the meterd-side
// source for usage_minutes.tx_bytes (gateway HTTP response
// bytes). Production wires this against the gatewayd's
// EgressTxService gRPC stream (PR-2, gateway side); the
// adapter maintains a per-instance ring buffer and the
// Drain method returns the per-(instance, minute) delta.
//
// In PR-1 (no gateway stream yet), this adapter returns
// 0 for tx_bytes unconditionally — the schema layer is
// landed without the gateway-side producer, and the
// append-time semantics are correct (tx_bytes stays at 0).
// PR-2 wires the gRPC client.
type gatewayEgressAdapter struct {
	// reserved for PR-2 — populated with the gRPC stream
	// client + ring buffer drainer.
}

func (a *gatewayEgressAdapter) EgressBytes(instanceID string) (uint64, uint64, bool) {
	// PR-1: tx_bytes is always 0 (no gateway stream wired).
	// netTxBytes is also 0 here — scheddEgressAdapter owns
	// that column. ok=true would be misleading (we have no
	// row); return ok=false so the sampler falls through to
	// its no-source branch and writes 0 to BOTH egress
	// columns. PR-2 re-implements this method to read the
	// gateway ring buffer and return (tx, 0, true) when a
	// row is present.
	_ = instanceID
	return 0, 0, false
}

// egressAggregator combines scheddEgressAdapter (net_tx_bytes)
// and gatewayEgressAdapter (tx_bytes) into a single
// meter.EgressSource. The two columns are sourced from
// independent producers (ADR-046 §2) so a single tick may
// have one or both; the aggregator ORs ok from the underlying
// adapters and zeros out the column the underlying adapter
// didn't report.
type egressAggregator struct {
	schedd *scheddEgressAdapter
	gw     *gatewayEgressAdapter
}

func (a *egressAggregator) EgressBytes(instanceID string) (uint64, uint64, bool) {
	// PR-1: both adapters return ok=false. Aggregator
	// also returns ok=false so the sampler writes 0 to
	// both columns. PR-2 swaps the bodies for real
	// adapter calls.
	_ = instanceID
	return 0, 0, false
}

func main() {
	wire.Daemon("meterd", run)
}

// runDeps is the dependency-injection seam for tests.
type runDeps struct {
	configPath string
	openDB     func(context.Context, string) (*pgxpool.Pool, error)
	migrate    func(context.Context, *pgxpool.Pool) error
	loadMeter  func(*Config) (*meter.Config, error)
	// getenv is the env reader the wire-up uses (FAAS_SCHEDD_ADDR,
	// FAAS_BILLING_PROVIDER, FAAS_QUOTA_INTERVAL, ...). Tests can stub it.
	// Mirrors cmd/apid/main.go's getenv on its runDeps.
	getenv func(string) string
	// dialSchedd is the constructor for the schedd gRPC client. nil in
	// production (defaultDeps wires scheddgrpc.DialContext); tests
	// inject a fake to avoid touching the unix socket. Issue #95:
	// signature takes ctx + tls config so the dial participates in the
	// daemon's lifecycle cancellation and can dial a TLS-wrapped remote
	// schedd once the control plane is decoupled.
	dialSchedd func(ctx context.Context, target string, tlsCfg *tls.Config) (parkInstanceParker, error)
	// loadBillingProvider constructs the billing.Provider the pusher
	// loop dispatches through (ADR-025 / PR #3). nil in production
	// (defaultDeps wires billingloader.LoadProviderForMeterd); tests
	// inject a stub that returns a no-op Provider so the loop body
	// runs without touching Stripe/Paddle. Mirrors the test-double
	// pattern at cmd/apid/main.go.
	loadBillingProvider func(env func(string) string, store state.Store, log *slog.Logger) (billing.Provider, string, error)
	// The two collaborators are wired in production by runWithDeps
	// after the pool is open; tests can pre-populate via the fields.
	parker parkInstanceParker
	pusher billing.Provider
	// mailer is the dunning-timer's outbound email. Wired via
	// mail.SenderFromEnv in defaultDeps so the FAAS_MAIL_TRANSPORT
	// knob is honored (default: log). Tests can inject a noop.
	mailer mail.Sender
	now    func() time.Time
	// metricsListenAndServe returns a fully-built *http.Server bound to a
	// fresh net.Listener on addr (or the error from net.Listen). The caller
	// invokes `srv.Serve(ln)` on a goroutine and `srv.Shutdown(stopCtx)`
	// during graceful drain — the same server owns both halves, so the
	// pair stays in lockstep (no possibility of one server's Serve
	// outliving another's Shutdown). Mirrors cmd/schedd/main.go:151-158.
	// Tests inject a stub that returns a nop server (without binding).
	metricsListenAndServe func(addr string, h http.Handler) (*http.Server, error)
}

func defaultDeps() runDeps {
	return runDeps{
		configPath: "/etc/faas/meterd.toml",
		openDB:     db.Open,
		migrate:    db.MigrateUp,
		loadMeter:  func(c *Config) (*meter.Config, error) { return c.Meter, nil },
		getenv:     os.Getenv,
		dialSchedd: func(ctx context.Context, target string, tlsCfg *tls.Config) (parkInstanceParker, error) {
			c, err := scheddgrpc.DialContext(ctx, target, tlsCfg)
			if err != nil {
				return nil, err
			}
			return c, nil
		},
		loadBillingProvider: func(env func(string) string, store state.Store, log *slog.Logger) (billing.Provider, string, error) {
			return billingloader.LoadProviderForMeterd(env, store, store, log)
		},
		mailer: nil, // populated lazily in runWithDeps via mail.SenderFromEnv
		now:    time.Now,
		metricsListenAndServe: func(addr string, h http.Handler) (*http.Server, error) {
			ln, err := net.Listen("tcp", addr)
			if err != nil {
				return nil, err
			}
			srv := &http.Server{Handler: h, ReadHeaderTimeout: 10 * time.Second}
			// Serve in a goroutine; the daemon keeps `srv` and calls
			// Shutdown on it during drain. Pairing Serve/Shutdown on the
			// same *http.Server avoids the dual-server asymmetry the
			// factory's previous shape allowed (PR #75 review finding).
			// Errors are logged via the package-level slog.Default here
			// because defaultDeps is built before runWithDeps wires the
			// daemon's *slog.Logger.
			go func() {
				if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
					slog.Default().Error("meterd: metrics http", "err", err)
				}
			}()
			return srv, nil
		},
	}
}

func run(ctx context.Context, log *slog.Logger) error {
	return runWithDeps(ctx, log, defaultDeps())
}

func runWithDeps(ctx context.Context, log *slog.Logger, deps runDeps) error {
	cfg, err := LoadConfig(deps.configPath)
	if err != nil {
		return err
	}
	mc, err := deps.loadMeter(cfg)
	if err != nil {
		return err
	}
	mc.Defaults()

	pool, err := deps.openDB(ctx, cfg.DBURL)
	if err != nil {
		return err
	}
	defer pool.Close()
	if err := deps.migrate(ctx, pool); err != nil {
		return err
	}

	store := state.NewPgStore(pool)
	pn := db.PoolNotifier{Pool: pool}

	// Resolve the schedd socket: env wins over the TOML default so the
	// e2e harness can dial a per-test socket without rewriting the unit
	// file. Both empty is the strict-exit failure case (issue #52
	// acceptance — refuse to start rather than run unbounded).
	scheddAddr := deps.getenv("FAAS_SCHEDD_ADDR")
	if scheddAddr == "" {
		scheddAddr = cfg.SocketPath
	}
	if scheddAddr == "" {
		return fmt.Errorf("meterd: FAAS_SCHEDD_ADDR (or socket_path in meterd.toml) is required")
	}
	parker := deps.parker
	if parker == nil {
		if deps.dialSchedd == nil {
			return fmt.Errorf("meterd: nil dialSchedd and nil parker (refusing to start unbounded)")
		}
		c, err := deps.dialSchedd(ctx, scheddAddr, nil)
		if err != nil {
			return fmt.Errorf("meterd: dial schedd %q: %w", scheddAddr, err)
		}
		parker = c
	}

	pusher := deps.pusher
	if pusher == nil {
		if deps.loadBillingProvider == nil {
			return fmt.Errorf("meterd: nil loadBillingProvider and nil pusher (refusing to start unbounded)")
		}
		var provName string
		var err error
		pusher, provName, err = deps.loadBillingProvider(deps.getenv, store, log)
		if err != nil {
			return fmt.Errorf("meterd: load billing provider: %w", err)
		}
		// Empty STRIPE_API_KEY on a Stripe box is a soft-warn today
		// (pushUsageRecordSDKSum returns an error per call, the loop
		// logs and skips); with the Paddle provider, FAAS_PADDLE_API_KEY
		// must be set or the SDK refuses to initialize. Surface the
		// provider name so an operator can match the warning to the
		// right env var.
		if provName == "stripe" && deps.getenv("STRIPE_API_KEY") == "" {
			log.Warn("STRIPE_API_KEY is empty — daily Stripe push will no-op (pushUsageRecordSDKSum returns an error without a key)",
				"provider", provName)
		}
		if provName == "paddle" && deps.getenv("FAAS_PADDLE_API_KEY") == "" {
			log.Warn("FAAS_PADDLE_API_KEY is empty — daily Paddle push will no-op",
				"provider", provName)
		}
		log.Info("meterd billing provider loaded", "provider", provName)
	}

	// Mailer: defaults to mail.SenderFromEnv so FAAS_MAIL_TRANSPORT
	// selects the transport (resend/postmark/log/noop). The dunning
	// timer needs this for its transition emails.
	mailer := deps.mailer
	if mailer == nil {
		mailer = mail.SenderFromEnv(deps.getenv, log)
	}

	// FAAS_QUOTA_INTERVAL / FAAS_SAMPLE_INTERVAL / FAAS_STRIPE_INTERVAL /
	// FAAS_DUNNING_INTERVAL / FAAS_RESIDENCY_INTERVAL let the e2e test
	// shrink the timer cadences to sub-second for the "transition
	// within one tick" acceptance. A bad parse logs and falls through
	// to mc.Defaults() rather than crashing the daemon.
	applyEnvTick("FAAS_SAMPLE_INTERVAL", &mc.SampleInterval, deps.getenv, log)
	applyEnvTick("FAAS_QUOTA_INTERVAL", &mc.QuotaInterval, deps.getenv, log)
	applyEnvTick("FAAS_STRIPE_INTERVAL", &mc.StripeInterval, deps.getenv, log)
	applyEnvTick("FAAS_DUNNING_INTERVAL", &mc.DunningInterval, deps.getenv, log)
	applyEnvTick("FAAS_RESIDENCY_INTERVAL", &mc.ResidencyInterval, deps.getenv, log)
	applyEnvTick("FAAS_ALERT_EVAL_INTERVAL", &mc.AlertEvalInterval, deps.getenv, log)

	// Dunning timer: drives the 7-day past_due → suspended and 21-day
	// suspended → deleted_pending transitions (spec §4.7, §17). Wired
	// into the loop alongside sample/quota/stripe so all five timers
	// share the same ctx-cancel lifecycle.
	dunning := meter.NewDunning(meter.DunningParams{
		Store:  store,
		Parker: parker,
		Mailer: mailer,
		Notif:  pn,
		Log:    log,
	})

	// Per-daemon Prometheus registry (ADR-015) — built unconditionally
	// so the Loop has it from the first tick. meter.NewLoop accepts nil
	// and coerces to a fresh test registry; here we hand it the real one.
	ops := wire.NewOpsMetrics("meterd")

	// Residency timer: emits the §12 "Resident GB per paying customer"
	// gauge (ADR-031, PR #141). Wired into the loop alongside
	// sample/quota/stripe/dunning so all five timers share the same
	// ctx-cancel lifecycle. ops is the per-daemon registry above;
	// residency.SetResidentGBPerCustomer is nil-safe so a later ops
	// swap doesn't take the gauge down with it.
	residency := meter.NewResidency(store, deps.now, log, ops)

	// The five timers run in goroutines; the cancel-watcher below picks
	// up the first error and returns. meterd has no inbound gRPC — the
	// public listener is gatewayd's (spec §Component ownership).
	//
	// Issue #279 / PR-B: the cpu adapter lets the sampler read the
	// per-instance CPU-µs snapshot the schedd's instancestats.Poller
	// maintains. The adapter dials the schedd gRPC socket on the same
	// box (ADR-015) and refreshes the snapshot at most once per 30 s
	// — bounded staleness without forcing a gRPC round trip per
	// instance.
	cpu := &scheddCPUAdapter{parker: parker, now: deps.now}
	// ADR-046 (PR-1 + PR-2): wire the egress adapters so the
	// sampler can append tx_bytes + net_tx_bytes to
	// usage_minutes. PR-1 leaves the gateway adapter as a
	// no-op (the ring-buffer producer lands in PR-2) so the
	// aggregator returns ok=false from gatewayEgressAdapter;
	// the schedd adapter is real (reads NetTxBytes from the
	// existing schedd gRPC round trip on the same rows map
	// cpu already fetched). The aggregator stays in place
	// across PR-1 and PR-2; PR-2 replaces gatewayEgressAdapter's
	// body without changing the wiring. WithEgress passes the
	// aggregator into NewLoop so the loop's sampler uses the
	// 4-arg NewSamplerWithEgress instead of the legacy
	// 3-arg NewSampler. Loop.WithEgress is nil-safe so a
	// future test harness can omit the egress wire without
	// touching the constructor.
	scheddEgress := &scheddEgressAdapter{cpu: cpu}
	gwEgress := &gatewayEgressAdapter{}
	egress := &egressAggregator{schedd: scheddEgress, gw: gwEgress}
	// Issue #396 / ADR-045 PR 4: instantiate the alert evaluator and
	// hand it to the loop. The evaluator is nil-coerced below when
	// neither FAAS_PROMETHEUS_URL nor FAAS_HOST_AGE_IDENTITY_PATH is
	// configured — the dev loop runs five ticks on a stripped-down box
	// where Prometheus isn't reachable and host age isn't loaded.
	// The single meterd process today has exactly one evaluator; the
	// loop's contract is "at most one", matching the design note at
	// pkg/alerts/evaluator.go.
	evaluator := buildAlertEvaluator(deps, store, log, ops)
	loop := meter.NewLoop(store, cpu, parker, pusher, pn, mailer, dunning, residency, evaluator, deps.now, log, mc, ops).
		WithEgress(egress)
	errc := make(chan error, 1)
	go func() { errc <- loop.Run(ctx) }()

	// Metrics + healthz listener. Mirrors cmd/schedd/main.go:143-158 —
	// per-daemon Prometheus registry (ADR-015), mux at /metrics +
	// /healthz, 5s graceful shutdown on drain. Empty cfg.MetricsAddr
	// disables both endpoints (the production default in
	// deploy/etc/meterd.toml.example).
	const metricsPath = "/metrics"
	var metricsSrv *http.Server
	if cfg.MetricsAddr != "" {
		if deps.metricsListenAndServe == nil {
			return fmt.Errorf("meterd: nil metricsListenAndServe (refusing to start with MetricsAddr set)")
		}
		mux := http.NewServeMux()
		mux.Handle(metricsPath, ops.Handler())
		// /healthz — 200 when every tracked timer (sample / quota /
		// stripe / dunning) has fired within
		// meter.StaleAfterMultiplier × its interval (spec §14 M7,
		// "meterd healthy iff sampled within 3 minutes"); 503 with a
		// JSON body listing the stale tick names otherwise. The body
		// always includes a per-tick last-fire wall clock so an
		// operator can diagnose without grepping journald.
		mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
			status := loop.Health(time.Now())
			w.Header().Set("Content-Type", "application/json")
			code := http.StatusOK
			if !status.Healthy {
				code = http.StatusServiceUnavailable
			}
			w.WriteHeader(code)
			_ = json.NewEncoder(w).Encode(status)
		})
		srv, err := deps.metricsListenAndServe(cfg.MetricsAddr, mux)
		if err != nil {
			return fmt.Errorf("meterd: metrics listen %q: %w", cfg.MetricsAddr, err)
		}
		metricsSrv = srv
		log.Info("meterd metrics listening", "addr", cfg.MetricsAddr)
	}

	select {
	case <-ctx.Done():
		log.Info("meterd draining")
	case err := <-errc:
		if err != nil && !errors.Is(err, context.Canceled) {
			return err
		}
	}

	// Graceful shutdown: detach a context from the already-cancelled caller
	// ctx (net/http Shutdown requires a non-Done parent). 5s matches the
	// schedd/vmmd/builderd shutdown deadline.
	stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if metricsSrv != nil {
		//nolint:contextcheck // shutdown ctx must outlive the already-cancelled caller ctx per net/http contract.
		_ = metricsSrv.Shutdown(stopCtx)
	}
	return nil
}

// applyEnvTick parses FAAS_*_INTERVAL on top of mc.Defaults(). Mirrors
// cmd/apid/main.go::graceIntervalFromEnv; kept local so meterd stays
// in one file.
func applyEnvTick(key string, dst *time.Duration, getenv func(string) string, log *slog.Logger) {
	v := getenv(key)
	if v == "" {
		return
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		log.Warn("unparseable interval; using default", "env", key, "got", v, "err", err)
		return
	}
	*dst = d
}

// buildAlertEvaluator wires the alert-evaluator (issue #396 /
// ADR-045 PR 4). Returns nil if neither FAAS_PROMETHEUS_URL nor
// FAAS_HOST_AGE_IDENTITY_PATH is configured — the dev loop runs
// five ticks on a stripped-down box where Prometheus isn't
// reachable and host age isn't loaded. The single meterd process
// today has exactly one evaluator; the loop's contract is "at most
// one", matching the design note at pkg/alerts/evaluator.go.
//
// Both env vars are read fresh on each call (cmd/meterd runs this
// helper once at startup, not on each tick). The Prometheus URL is
// used to build a pkg/promql.Client — empty URL means nil PromQL,
// which the evaluator treats as a "degraded: prometheus not
// configured" source for every rule. The identity path is loaded
// strictly; a 0o400 file-mode check (pkg/secretbox.LoadHostKey) is
// the load-bearing detail for the §11 tripwire.
func buildAlertEvaluator(deps runDeps, store state.Store, log *slog.Logger, ops *wire.OpsMetrics) *alerts.Evaluator {
	promURL := deps.getenv("FAAS_PROMETHEUS_URL")
	identityPath := deps.getenv("FAAS_HOST_AGE_IDENTITY_PATH")
	if promURL == "" && identityPath == "" {
		log.Warn("meterd: alert evaluator disabled — both FAAS_PROMETHEUS_URL and FAAS_HOST_AGE_IDENTITY_PATH unset; running with five ticks")
		return nil
	}

	var promClient appmetrics.PromQL
	if promURL != "" {
		// pkg/promql.NewClient takes an HTTPDoer for testability;
		// nil resolves to http.DefaultClient. PerAttempt timeout is
		// applied by pkg/webhookout's dispatcher, not the
		// evaluator (the evaluator's PromQL calls have their own
		// per-query deadline via the caller's context).
		promClient = promql.NewClient(promURL, nil)
	}

	var identityLoader func() *age.X25519Identity
	if identityPath != "" {
		ident, err := secretbox.LoadHostKey(identityPath)
		if err != nil {
			// A failure to load the identity is fatal for the
			// alert evaluator — without it we cannot unseal any
			// webhook_secret, so every dispatch would be a no-op.
			// Log loudly and skip the evaluator (the daemon
			// stays up and the other five ticks run).
			log.Error("meterd: load host age identity; alert evaluator disabled",
				"path", identityPath, "err", err)
			return nil
		}
		log.Info("meterd: host age identity loaded for alert evaluator",
			"path", identityPath)
		identityLoader = func() *age.X25519Identity { return ident }
	}

	dispatcher := webhookout.NewDispatcher(webhookout.DispatcherOptions{})
	auditor := audit.New(store, log, ops, "meterd")
	return alerts.NewEvaluator(alerts.EvaluatorOptions{
		Store:      store,
		PromQL:     promClient,
		Audit:      auditor,
		Identity:   identityLoader,
		Dispatcher: dispatcher,
		Log:        log,
		Ops:        ops,
	})
}
