// Package sched — daemon glue that translates pg_notify events into ledger
// updates and instance state writes. schedd is the sole writer to the
// instances table (spec §Component ownership); this file owns the loop that
// reacts to apid's notifications and drives the reaper tick. All instance
// mutation (create, transition, snapshot, destroy) goes through the Engine —
// the Loop is pure glue that decides *when* to act, not *how*.
package sched

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/onebox-faas/faas/pkg/api"
	"github.com/onebox-faas/faas/pkg/db"
	"github.com/onebox-faas/faas/pkg/sched/scaleup"
	"github.com/onebox-faas/faas/pkg/state"
)

// Loop subscribes to the pg_notify channels schedd cares about and reacts. It
// runs the idle reaper on a 10 s tick and cron on a 60 s tick (spec §4.3). The
// Engine holds the store, ledger, and vmmd client; the Loop only orchestrates.
type Loop struct {
	pool       *pgxpool.Pool
	engine     *Engine
	log        *slog.Logger
	gateway    GatewaySynth
	now        func() time.Time
	flowCounts FlowCounter
watchdog   *Watchdog           // §6.1 watchdog; nil means "no watchdog" (tests can opt out)
	retention  *Retention          // §17 retention sweep; nil means "no retention" (tests can opt out)
	heartbeat  *Heartbeat          // issue #97 / ADR-025 axis 3 (PR #114) per-node liveness; nil opts out
	instStats  InstanceStatsPoller // issue #170 / PR-A per-{app,node} metrics poller; nil opts out
	scaleup    *scaleup.Trigger    // issue #169 / #172 reactive scale-up trigger; nil opts out
}

func NewLoop(pool *pgxpool.Pool, engine *Engine, log *slog.Logger) *Loop {
	return &Loop{
		pool: pool, engine: engine, log: log,
		now:        time.Now,
		flowCounts: noopFlowCounter{},
	}
}

// WithWatchdog attaches the §6.1 watchdog (commit 3). Tests can skip
// it by not calling this; the watchdog field stays nil and Run's
// 4th ticker simply never fires a case. Production cmd/schedd wires
// the real Watchdog from the existing engine deps so the watchdog
// shares the same store / engine / clock as the rest of the loop.
func (l *Loop) WithWatchdog(w *Watchdog) *Loop {
	l.watchdog = w
	return l
}

// WithRetention attaches the §17 retention sweep (PR #74). Same opt-out
// shape as WithWatchdog: nil means no ticker fires the retention case.
// Production wires NewRetention(store, log); the default retention
// window + interval live in pkg/api/limits.
func (l *Loop) WithRetention(r *Retention) *Loop {
	l.retention = r
	return l
}

// WithHeartbeat attaches the per-node liveness sweep (issue #97 /
// ADR-025 axis 3, PR #114). Same nil-skip semantics as the
// watchdog + retention tickers — production cmd/schedd wires
// sched.NewHeartbeat(store, vmmRouter, log); tests inject a fake
// or skip. The interval lives on the Heartbeat itself
// (DefaultHeartbeatInterval = 30s; overridable for tests).
func (l *Loop) WithHeartbeat(h *Heartbeat) *Loop {
	l.heartbeat = h
	return l
}

// InstanceStatsPoller is the narrow interface the Loop's per-tick
// instance-stats worker exposes (issue #170 / PR-A). Production
// wires *instancestats.Poller; tests inject a fake. The interface
// lives here — not in pkg/sched/instancestats — to avoid the
// import cycle "instancestats → sched → instancestats" the
// existing flowcount pattern established (the interface is the
// contract the Loop reads; the concrete type lives behind it).
type InstanceStatsPoller interface {
	Tick(ctx context.Context) error
	TickInterval() time.Duration
}

// WithInstanceStats attaches the per-instance metrics poller
// (issue #170 / PR-A). Same nil-skip semantics as the heartbeat
// ticker — production wires instancestats.NewPoller(...); tests
// inject a fake or skip. The interval lives on the poller itself
// (instancestats.DefaultStatsInterval = 200 ms — 5 Hz, the 250 ms
// spike-capture acceptance gate). The reader the poller populates
// is the canonical seam #171 (reaper scale-down bias) and #169
// (reactive scale-up trigger) will read from.
func (l *Loop) WithInstanceStats(p InstanceStatsPoller) *Loop {
	l.instStats = p
	return l
}

// WithGatewaySynth wires the gateway-internal RPC client the cron
// dispatch loop uses. Production calls this from cmd/schedd after
// dialing the gateway socket; tests inject a recording stub.
func (l *Loop) WithGatewaySynth(g GatewaySynth) *Loop {
	l.gateway = g
	return l
}

// WithClock swaps the time source. Tests use it to advance through cron
// boundaries deterministically; production leaves the default.
func (l *Loop) WithClock(now func() time.Time) *Loop {
	if now != nil {
		l.now = now
	}
	return l
}

// FlowCounter is the slice of "open TCP flow count by instance" the
// reaper uses to gate idle parking (spec §17 G7). Production injects a
// conntrack reader in PR-B; the default noopFlowCounter returns 0 for
// every instance, preserving the prior LastRequest-only behaviour.
type FlowCounter interface {
	Open(ctx context.Context, instanceID string) (int64, error)
}

// noopFlowCounter is the default FlowCounter. Used until PR-B wires a
// real reader; keeps ReapIdle's G7 rule inert.
type noopFlowCounter struct{}

func (noopFlowCounter) Open(_ context.Context, _ string) (int64, error) { return 0, nil }

// WithFlowCounter wires the conntrack-derived "open flows per
// instance" source (spec §17 G7). Tests inject a fake to drive table
// cases for the reaper's skip-when-busy rule; production wires a
// real conntrack reader once that lands. Nil/inert callers leave
// the noop default in place.
func (l *Loop) WithFlowCounter(fc FlowCounter) *Loop {
	if fc != nil {
		l.flowCounts = fc
	}
	return l
}

// WithScaleUp attaches the per-app reactive scale-up trigger
// (issue #169 / #172, pkg/sched/scaleup). Nil opts out (the
// scaleupTick arm of Run's select never fires). Production wires
// the real trigger from cmd/schedd after the Engine + Store + (PR
// #205) instancestats.Reader are available; tests inject a manual
// ticker or skip via nil. The trigger's own Interval() governs the
// cadence — same opt-out shape as WithHeartbeat / WithWatchdog.
func (l *Loop) WithScaleUp(t *scaleup.Trigger) *Loop {
	l.scaleup = t
	return l
}

// Run blocks until ctx is cancelled. It owns three event sources: the LISTEN
// subscriber, the reaper tick, and the cron tick.
func (l *Loop) Run(ctx context.Context) error {
	// F-11: SubscribeWithReconnect wraps Subscribe with exponential backoff
	// (100ms → 5s cap) and re-acquires the LISTEN connection across pg
	// restarts. The outer channel never closes on conn drop — only ctx
	// cancel can stop this loop. Prior `notif, ok := <-` would have
	// exited cleanly the instant the LISTEN conn died, leaving the daemon
	// alive (systemd Restart=on-failure doesn't catch clean exits) but
	// inert. schedd is now a long-running aware subscriber.
	notif, err := db.SubscribeWithReconnect(ctx, l.pool, []string{
		db.NotifyAppChanged,
		db.NotifyDeploymentChanged,
		db.NotifySnapshotPrime,
	}, l.log)
	if err != nil {
		return err
	}
	// SubscribeWithReconnect owns its own cancel via the deferred
	// goroutine inside the wrapper; we close by ending ctx.

	reaperT := time.NewTicker(10 * time.Second)
	defer reaperT.Stop()
	cronT := time.NewTicker(60 * time.Second)
	defer cronT.Stop()
	// Watchdog ticker (commit 3, spec §6.1). 1s cadence matches the
	// spec's "per-second" granularity for catching stuck rows before
	// they pin a ledger reservation for the full 30s cold-boot
	// budget. nil watchdog skips this ticker entirely so the test
	// surface stays green without a watchdog dependency.
	var watchdogT *time.Ticker
	if l.watchdog != nil {
		watchdogT = time.NewTicker(DefaultWatchdogInterval)
		defer watchdogT.Stop()
	}
	// Retention ticker (PR #74, spec §17 follow-up). Default cadence
	// is hourly (pkg/api.DefaultRetentionInterval) — the sweep itself
	// reads now-30d, so hourly granularity means a row that crossed
	// the threshold gets DELETED within the next hour. nil retention
	// skips this ticker entirely.
	//
	// First-fire is intentionally DEFERRED one minute after startup
	// (retentionFirstFireDelay). A bare time.NewTicker fires once
	// immediately, which on a fresh deploy would race the §6.1
	// watchdog's first sweep and delete any rows the backfill
	// (migration 00017) anchored to a now()-based terminal_at before
	// the watchdog has had a chance to stamp its first batch.
	var retentionT *time.Ticker
	var retentionFirst <-chan time.Time
	if l.retention != nil {
		t := time.NewTicker(api.DefaultRetentionInterval)
		defer t.Stop()
		retentionT = t
		delay := time.NewTimer(retentionFirstFireDelay)
		defer delay.Stop()
		retentionFirst = delay.C
	}
	// Heartbeat ticker (issue #97 / ADR-025 axis 3, PR #114).
	// Per-node liveness sweep: ping each active compute_node,
	// stamp last_heartbeat_at on success or flip active=false
	// on failure. Default cadence DefaultHeartbeatInterval
	// (30s); production cmd/schedd wires NewHeartbeat with the
	// RoutedVMM, tests inject a fake or skip via nil. The ticker
	// fires immediately on construction — a freshly-started
	// schedd stamps the synthetic default-local row's heartbeat
	// without a 30s gap on cold start.
	var heartbeatT *time.Ticker
	if l.heartbeat != nil {
		interval := l.heartbeat.Interval
		if interval <= 0 {
			interval = DefaultHeartbeatInterval
		}
		heartbeatT = time.NewTicker(interval)
		defer heartbeatT.Stop()
	}
// Instance-stats poller ticker (issue #170 / PR-A). Per-Tick
	// sweep: enumerate live instances + active compute_nodes,
	// dial each node fresh, decode Stats, replace the Reader
	// snapshot, emit the wire rollup. Default cadence
	// instancestats.DefaultStatsInterval (200 ms — 5 Hz). The
	// ticker is constructed BEFORE the first Tick (below) so
	// the first sample lands at t=0 instead of t=Interval — a
	// documented correction to the heartbeat loop's
	// "first sample at t=Interval" behaviour. nil poller skips
	// the ticker entirely (tests + run-without-metrics mode).
	var instStatsT *time.Ticker
	if l.instStats != nil {
		interval := l.instStats.TickInterval()
		if interval <= 0 {
			// Defensive: the poller is contract-bound to
			// return a positive interval, but a test that
			// injects a stub returning 0 must not hang on
			// time.NewTicker(0). Fall back to the package
			// default — the same one the poller would use.
			interval = 200 * time.Millisecond
		}
		instStatsT = time.NewTicker(interval)
		defer instStatsT.Stop()
	}
	// First Tick at t=0 (issue #170 / PR-A). Heartbeat uses
	// time.NewTicker's "fires immediately on construction" property
	// to land its first sample at t=0; the stats poller can't rely
	// on the same — its 200 ms cadence is much shorter and a
	// spurious first fire would burn a dial cycle on every restart.
	// Call Tick directly so the first sample lands deterministically.
	if l.instStats != nil {
		l.runInstanceStats(ctx)
	}
	// Scale-up trigger ticker (issue #169 / #172).
	// Per-app reactive scale-up: every Interval() seconds, run
	// the trigger's Tick so a hot RPS / CPU signal can pre-empt
	// the request-driven wake path. Default cadence 1s
	// (api.ScaleUpDecisionIntervalSeconds); the trigger supervises
	// its own nil-safety so a nil trigger never fires the case.
	var scaleupT *time.Ticker
	if l.scaleup != nil {
		interval := l.scaleup.Interval()
		if interval <= 0 {
			interval = api.ScaleUpDecisionIntervalSeconds * time.Second
		}
		scaleupT = time.NewTicker(interval)
		defer scaleupT.Stop()
	}

	for {
		select {
		case <-ctx.Done():
			return nil
		case n, ok := <-notif:
			if !ok {
				// Defensive — wrapper guarantees open until ctx done.
				return nil
			}
			l.handleNotification(ctx, n)
		case <-reaperT.C:
			l.runReaper(ctx)
		case <-cronT.C:
			l.runCronTick(ctx)
		case <-watchdogTick(watchdogT):
			l.runWatchdog(ctx)
		case <-heartbeatTick(heartbeatT):
			l.runHeartbeat(ctx)
case <-instStatsTick(instStatsT):
			l.runInstanceStats(ctx)
		case <-scaleupTick(scaleupT):
			l.runScaleUp(ctx)
		case <-retentionFirst:
			// One-shot first fire (see retentionFirstFireDelay). After
			// this the channel is set to nil so subsequent ticks
			// exclusively come from retentionT (the 1h ticker).
			l.runRetention(ctx)
			retentionFirst = nil
		case <-retentionTick(retentionT):
			l.runRetention(ctx)
		}
	}
}

// watchdogTick is a helper that turns a nil-ticker's channel into a
// never-firing channel. It keeps the main select above free of
// per-iteration nil checks.
func watchdogTick(t *time.Ticker) <-chan time.Time {
	if t == nil {
		return nil
	}
	return t.C
}

// retentionTick is the same nil-safe pattern as watchdogTick, kept
// separate so each ticker type's name shows up in stack traces if
// a future regression corrupts the channel wiring.
func retentionTick(t *time.Ticker) <-chan time.Time {
	if t == nil {
		return nil
	}
	return t.C
}

// heartbeatTick is the per-node liveness ticker (PR #114). Same
// nil-safe shape as the watchdog/retention tickers: nil ticker ⇒
// nil channel, so the select case never fires.
func heartbeatTick(t *time.Ticker) <-chan time.Time {
	if t == nil {
		return nil
	}
	return t.C
}

// instStatsTick is the per-instance metrics ticker (issue #170 /
// PR-A). Same nil-safe shape as the heartbeat ticker: nil ticker
// ⇒ nil channel, so the select case never fires. Kept separate
// from heartbeatTick so each ticker type's name shows up in stack
// traces if a future regression corrupts the channel wiring.
func instStatsTick(t *time.Ticker) <-chan time.Time {
	if t == nil {
		return nil
	}
	return t.C
}

// scaleupTick is the reactive scale-up trigger ticker (issue #169 /
// #172). Same nil-safe shape as the heartbeat/retention tickers.
func scaleupTick(t *time.Ticker) <-chan time.Time {
	if t == nil {
		return nil
	}
	return t.C
}

// runHeartbeat dispatches one sweep of the per-node liveness
// ticker. Exported as a method so tests can drive a single tick
// without spinning up Run's goroutine. Tick errors are logged
// inside Heartbeat.Tick — Run never returns them so a transient
// DB blip can't tear down the loop.
func (l *Loop) runHeartbeat(ctx context.Context) {
	if err := l.heartbeat.Tick(ctx); err != nil && !errors.Is(err, context.Canceled) {
		l.log.Warn("heartbeat tick error", "err", err)
	}
}

// runInstanceStats dispatches one sweep of the per-instance
// metrics poller (issue #170 / PR-A). Same shape as runHeartbeat
// — exported as a method so tests drive a single tick without
// spinning up Run's goroutine. Tick errors are logged + swallowed
// (a partial sweep is still useful; the next tick has a fresh
// chance). The nil guard mirrors runHeartbeat's defensiveness
// even though Run's ticker construction gates instStatsT to nil
// when the poller is absent — keeping the helper panic-safe
// means tests can call it directly on a bare Loop without
// tripping an unhelpful nil pointer dereference.
func (l *Loop) runInstanceStats(ctx context.Context) {
	if l.instStats == nil {
		return
	}
	if err := l.instStats.Tick(ctx); err != nil && !errors.Is(err, context.Canceled) {
		l.log.Warn("instance stats tick error", "err", err)
	}
}

// runWatchdog dispatches one sweep of the §6.1 watchdog. Exported as a
// method so tests can drive a single tick without spinning up Run's
// goroutine.
func (l *Loop) runWatchdog(ctx context.Context) {
	l.watchdog.sweepRuns(ctx)
}

// runRetention dispatches one sweep of the §17 retention sweep. Same
// shape as runWatchdog — exported as a method so tests drive a single
// tick without spinning up Run. Errors from SweepOnce are logged +
// swallowed (the sweep itself is idempotent + redelivery-safe; an
// error means a transient store outage, not a permanent fault).
func (l *Loop) runRetention(ctx context.Context) {
	deleted, err := l.retention.SweepOnce(ctx)
	if err != nil {
		l.log.Warn("retention: sweep failed", "err", err)
		return
	}
	if deleted > 0 {
		l.log.Info("retention: swept", "deleted", deleted)
	}
}

// runScaleUp dispatches one tick of the per-app reactive scale-up
// trigger (issue #169 / #172). Same shape as runHeartbeat —
// exported as a method so tests drive a single tick without
// spinning up Run. Tick errors are logged inside the trigger; Run
// never returns them so a transient store / scraper blip can't
// tear down the loop.
func (l *Loop) runScaleUp(ctx context.Context) {
	if err := l.scaleup.Tick(ctx); err != nil && !errors.Is(err, context.Canceled) {
		l.log.Warn("scaleup tick error", "err", err)
	}
}

// handleNotification decodes the JSON payload and applies the policy.
//
//   - app_changed / deployment_changed: informational. Wake materialises an
//     instance on demand (first request), so no eager instance creation here.
//   - snapshot_prime: imaged finished building a deployment's layer; boot it
//     once, snapshot it, and park it (spec §5 step 6, ADR-018).
func (l *Loop) handleNotification(ctx context.Context, n db.Notification) {
	switch n.Channel {
	case db.NotifyAppChanged:
		l.log.Debug("app_changed", "payload", n.Payload)
	case db.NotifyDeploymentChanged:
		l.log.Debug("deployment_changed", "payload", n.Payload)
	case db.NotifySnapshotPrime:
		var p struct {
			AppID        string `json:"app_id"`
			DeploymentID string `json:"deployment_id"`
		}
		if err := json.Unmarshal([]byte(n.Payload), &p); err != nil {
			l.log.Warn("sched: bad snapshot_prime payload", "err", err)
			return
		}
		if p.AppID == "" || p.DeploymentID == "" {
			l.log.Warn("sched: snapshot_prime missing ids", "payload", n.Payload)
			return
		}
		if err := l.engine.Prime(ctx, p.AppID, p.DeploymentID); err != nil {
			l.log.Warn("sched: prime failed", "app", p.AppID, "deployment", p.DeploymentID, "err", err)
		}
	}
}

// runReaper builds a read-only snapshot of every instance and applies the idle /
// RAM-pressure selectors, delegating each action to the Engine:
//   - ReapIdle → Engine.Park (snapshot + park; snapshot reused on next wake).
//   - SelectEvictions → Engine.Evict (destroy; next wake cold-boots, ADR-005).
func (l *Loop) runReaper(ctx context.Context) {
	store := l.engine.Store()
	apps, err := store.ListAllApps(ctx)
	if err != nil {
		l.log.Warn("reaper: list apps", "err", err)
		return
	}
	// G7 conntrack warm (spec §17): if the FlowCounter is also a Warm-able
	// reader (the production flowcount.Reader is), feed it every live
	// instance up front so Open calls below are cheap map lookups. The
	// type assertion keeps the FlowCounter interface narrow — test mocks
	// that don't implement Warm are simply skipped, preserving the
	// existing test surface. Either failure falls through to
	// LastRequest-only reaping per the fail-open contract pinned by
	// TestRunReaperFlowCounterErrorFailsOpen.
	if warmer, ok := l.flowCounts.(interface {
		Warm(context.Context, []state.Instance) error
	}); ok {
		all, err := store.ListAllInstances(ctx)
		if err != nil {
			l.log.Warn("reaper: list all instances for warm", "err", err)
		} else if warmErr := warmer.Warm(ctx, all); warmErr != nil {
			l.log.Warn("reaper: warm flow reader", "err", warmErr)
		}
	}
	now := time.Now()
	var snapshot []InstanceInfo
	for _, a := range apps {
		plan := api.Plan("")
		if acct, err := store.AccountByID(ctx, a.AccountID); err == nil {
			plan = acct.Plan
		}
		instances, err := store.ListInstancesForApp(ctx, a.ID)
		if err != nil {
			continue
		}
		for _, ins := range instances {
			// G7 flow count (spec §17): the conntrack reader is the
			// production source; nil/error falls back to 0 so a flow-source
			// glitch fails open (LastRequest-only path; safe default).
			var open int64
			if l.flowCounts != nil {
				if v, err := l.flowCounts.Open(ctx, ins.ID); err == nil {
					open = v
				} else {
					l.log.Warn("reaper: flow count", "instance", ins.ID, "err", err)
				}
			}
			snapshot = append(snapshot, InstanceInfo{
				Instance:     ins.ID,
				AppID:        ins.AppID,
				Plan:         plan,
				State:        state.State(ins.State),
				RAMMB:        ins.RAMMB,
				LastRequest:  ins.LastRequestAt,
				Started:      ins.StartedAt,
				IdleTimeoutS: a.IdleTimeoutS,
				NodeID:       ins.NodeID,
				// ux_spec §6.5: per-app floor the reaper honors
				// when parking idle instances. Plan-tier-gated
				// upstream (apid updateApp handler), so the
				// value is always >= 0 here.
				MinInstances: a.MinInstances,
				OpenConns:    open,
			})
		}
	}
	resident := l.engine.Ledger().ResidentRAM()
	for _, id := range ReapIdle(now, snapshot) {
		if err := l.engine.Park(ctx, id); err != nil {
			l.log.Warn("reaper: idle park", "instance", id, "err", err)
		}
	}
	for _, id := range SelectEvictions(resident, now, snapshot) {
		if err := l.engine.Evict(ctx, id); err != nil {
			l.log.Warn("reaper: eviction", "instance", id, "err", err)
		}
	}
}

// GatewaySynth is the slice of the gateway-internal RPC the cron
// loop (and Move 1's drain) use to fire a synthetic request through
// gatewayd (so metering + rate-limit apply identically to user
// traffic). Defined as an interface here so the cron loop can be
// tested without a live gateway socket.
//
// SynthesizeRequest is the legacy no-payload path (back-pressure
// probe; deprecated — kept so old callers and tests don't break).
// Invoke is the Move 1 path: it carries an invocation row through
// the wake gate so cron / async / queue-pull / delayed-task traffic
// reaches the runner envelope unchanged.
type GatewaySynth interface {
	SynthesizeRequest(ctx context.Context, appID, method, path string) error
	Invoke(ctx context.Context, appID string, inv state.Invocation) (state.Invocation, error)
}

// httpGatewaySynth is the production GatewaySynth: a unix-socket HTTP
// client pointed at gatewayd's /v1/synthesize endpoint.
type httpGatewaySynth struct {
	client  *http.Client
	baseURL string
	log     *slog.Logger
}

// DialGatewaySynth opens an HTTP unix-socket client targeting
// gatewayd's internal listener. The client is stateless — the unix
// socket is opened per request by the transport — so dial failures
// surface on the first SynthesizeRequest call.
func DialGatewaySynth(socketPath string, log *slog.Logger) (GatewaySynth, error) {
	if socketPath == "" {
		return nil, errors.New("sched: gateway synth socket path is empty")
	}
	if log == nil {
		log = slog.Default()
	}
	tr := &http.Transport{
		DialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
			return net.Dial("unix", socketPath)
		},
	}
	c := &http.Client{Transport: tr, Timeout: 30 * time.Second}
	return &httpGatewaySynth{
		client:  c,
		baseURL: "http://unix/v1/synthesize",
		log:     log,
	}, nil
}

// SynthesizeRequest posts {app_id, method, path} to gatewayd's internal
// /v1/synthesize endpoint over the unix socket. The HTTP transport
// (DialContext) handles the dial; this method just shapes the request.
func (h *httpGatewaySynth) SynthesizeRequest(ctx context.Context, appID, method, path string) error {
	body, err := json.Marshal(map[string]string{
		"app_id": appID, "method": method, "path": path,
	})
	if err != nil {
		return fmt.Errorf("sched: synth marshal: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.baseURL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("sched: synth request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := h.client.Do(req)
	if err != nil {
		return fmt.Errorf("sched: synth do: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("sched: synth: gateway returned %d", resp.StatusCode)
	}
	return nil
}

// Invoke posts the Move 1 invocation envelope to gatewayd's
// /v1/invocations:dispatch route. The response carries the post-dispatch
// state (dispatched/completed) so the drain can call Store.CompleteInvocation
// with the result blob. Network errors bubble up so the drain can retry.
func (h *httpGatewaySynth) Invoke(ctx context.Context, appID string, inv state.Invocation) (state.Invocation, error) {
	body, err := json.Marshal(map[string]any{
		"invocation_id": inv.ID,
		"app_id":        appID,
		"source":        string(inv.Source),
		"method":        inv.Method,
		"path":          inv.Path,
	})
	if err != nil {
		return inv, fmt.Errorf("sched: invocation marshal: %w", err)
	}
	url := "http://unix/v1/invocations:dispatch"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return inv, fmt.Errorf("sched: invocation request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := h.client.Do(req)
	if err != nil {
		return inv, fmt.Errorf("sched: invocation do: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusBadGateway {
		return inv, fmt.Errorf("sched: invocation: gateway returned %d", resp.StatusCode)
	}
	// Reset state to dispatched; gateway-set state strings are
	// "dispatched" today. Persistent failure surfaces to the drain
	// via the 502 path above.
	inv.State = state.InvocationDispatching
	return inv, nil
}

// runCronTick walks every enabled cron and dispatches any whose
// next-fire boundary has passed. It does NOT compute next-fire from
// robfig itself — the customer's cron.Schedule lives on the crons row
// (Schedule field) and we parse it per-tick. The dispatch path:
//
//  1. Resolve the cron + app, ensure the account isn't suspended.
//  2. Parse the schedule with robfig/cron; if NextFireAt(lastFiredAt) is
//     not in the past, skip.
//  3. Wake the app via the engine (idempotent — already-running apps
//     return their current instance).
//  4. SynthesizeRequest through gatewayd so metering + rate limits apply.
//  5. MarkCronFired + emit NotifyCronFired for the dashboard.
//
// Step 3+4 are the load-bearing spec bits (M7); they route the
// synthetic request through the gateway's full path so the metering +
// quota pipeline can't tell cron traffic from user traffic apart.
func (l *Loop) runCronTick(ctx context.Context) {
	crons, err := l.engine.Store().ListEnabledCrons(ctx)
	if err != nil {
		l.log.Warn("cron: list", "err", err)
		return
	}
	now := l.now()
	for _, c := range crons {
		l.dispatchOneCron(ctx, c, now)
	}
}

// dispatchOneCron is the per-cron decision tree. Factored out so the
// test surface can drive one cron with a fake clock.
func (l *Loop) dispatchOneCron(ctx context.Context, c state.Cron, now time.Time) {
	sched, err := ParseSchedule(c.Schedule)
	if err != nil {
		l.log.Warn("cron: bad schedule", "cron_id", c.ID, "err", err)
		return
	}
	// Boundary guard: fire iff we've crossed the next-fire boundary
	// since LastFiredAt. robfig's NextFireAt(from) is exclusive — call
	// it with LastFiredAt to get the upcoming boundary; if that boundary
	// is in the future, we already fired in this window. If LastFiredAt
	// is zero, the CreatedAt-based boundary is the first-fire guard so
	// we don't double-fire a cron enabled mid-minute.
	var boundary time.Time
	if c.LastFiredAt.IsZero() {
		boundary = c.CreatedAt
	} else {
		boundary = c.LastFiredAt
	}
	if sched.NextFireAt(boundary).After(now) {
		// Already fired in the current window.
		return
	}
	app, err := l.engine.Store().AppByID(ctx, c.AppID)
	if err != nil {
		l.log.Warn("cron: app", "cron_id", c.ID, "err", err)
		return
	}
	acct, err := l.engine.Store().AccountByID(ctx, app.AccountID)
	if err != nil {
		l.log.Warn("cron: account", "cron_id", c.ID, "err", err)
		return
	}
	if !acct.Active() {
		// Suspended accounts don't get cron traffic (spec §11 abuse
		// guard). The meter hard-stop will park the live instance; we
		// just skip the synthetic request here.
		return
	}
	if _, err := l.engine.Wake(ctx, c.AppID); err != nil {
		l.log.Warn("cron: wake", "cron_id", c.ID, "err", err)
		return
	}
	// Move 1: write the cron row to invocations so it shows up in
	// /v1/invocations and the meter sees it. cron_id is stamped so
	// the unified history endpoint can join back to crons for
	// "last_fired_at" semantics (kept on the crons table; both
	// surfaces are still served per the chosen plan).
	cronID := c.ID
	inv := state.Invocation{
		AppID:     c.AppID,
		AccountID: acct.ID,
		Source:    state.InvocationCron,
		Method:    "POST",
		Path:      c.Path,
		CronID:    &cronID,
		Headers:   json.RawMessage(`{"x-faas-cron":"true"}`),
		DueAt:     now,
	}
	enq, err := l.engine.Store().EnqueueInvocation(ctx, inv)
	if err != nil {
		l.log.Warn("cron: enqueue invocation", "cron_id", c.ID, "err", err)
		// Continue past — legacy wake-only path is still safe.
	}
	// Walk the row through pending → dispatching BEFORE calling
	// Invoke. The store's Claim only accepts state=pending, and
	// StampInstanceInvocation only accepts state=dispatching — so
	// the lifecycle must mirror the drain's: claim → invoke → stamp
	// → complete. Doing the claim here also keeps the row out of
	// the drain's next tick (which filters state='pending').
	if enq.ID != "" {
		if _, err := l.engine.Store().ClaimInvocation(ctx, enq.ID, "", 60); err != nil {
			l.log.Warn("cron: claim invocation", "cron_id", c.ID, "err", err)
		}
	}
	if l.gateway != nil {
		// Invoke delivers the synthetic HTTP envelope through the
		// wake gate; the meter + the runner both see this as a
		// request with method+path+headers. The synth adapter
		// (cmd/gatewayd) does its own always-Wake internally and
		// returns the live instance id on the echoed Invocation.
		invokeOut, ierr := l.gateway.Invoke(ctx, c.AppID, inv)
		if ierr != nil {
			l.log.Warn("cron: invoke", "cron_id", c.ID, "err", ierr)
			// Fall through to legacy wake-only shape so this
			// doesn't silently drop. tests may rely on the
			// SynthesizeRequest call for back-compat assertions.
			if err := l.gateway.SynthesizeRequest(ctx, c.AppID, "POST", c.Path); err != nil {
				l.log.Warn("cron: synthesize (legacy)", "cron_id", c.ID, "err", err)
				return
			}
		} else if enq.ID != "" {
			// Stamp the live instance handle + complete the row
			// so the drain's per-tick (state='pending' filter)
			// never picks it up. The meter join counts this row
			// once, against the live instance.
			if err := l.engine.Store().StampInstanceInvocation(ctx, enq.ID, invokeOut.InstanceID); err != nil {
				l.log.Warn("cron: stamp instance", "cron_id", c.ID, "err", err)
			}
			if err := l.engine.Store().CompleteInvocation(ctx, enq.ID, nil); err != nil {
				l.log.Warn("cron: complete", "cron_id", c.ID, "err", err)
			}
		}
	}
	if err := l.engine.Store().MarkCronFired(ctx, c.ID, now); err != nil {
		l.log.Warn("cron: mark fired", "cron_id", c.ID, "err", err)
	}
	payload, _ := json.Marshal(map[string]any{
		"cron_id": c.ID, "app_id": c.AppID, "at": now.UTC().Format(time.RFC3339Nano),
	})
	if err := l.engine.Notifier().Notify(ctx, db.NotifyCronFired, string(payload)); err != nil {
		l.log.Warn("cron: notify cron_fired", "err", err)
	}
}
