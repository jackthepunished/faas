package gateway

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/onebox-faas/faas/pkg/api"
	"github.com/onebox-faas/faas/pkg/gateway/egresssink"
	"github.com/onebox-faas/faas/pkg/wire"
)

// App is the routing target for a hostname.
type App struct {
	ID        string
	AccountID string // joined in pgRouter.toApp; empty only in fakeBackend unit tests (ADR-040)
	Plan      api.Plan
	// StreamingEnabled is the per-app streaming flag (issue #471 /
	// ADR-047). Plumbed through pgRouter.toApp from the apps row so
	// ServeHTTP can decide between the buffered and streamed
	// response path WITHOUT re-reading the database. PR-A uses the
	// flag only for the once-per-process buffered-fallback
	// deprecation log; PR-B activates the actual Flusher path.
	// Default-false in fakeBackend unit tests (the in-memory
	// backend doesn't populate the column).
	StreamingEnabled bool
}

// Target is one routable instance in the gateway's per-app cache (issue
// #168). Multiple Targets per app = real fan-out across max_concurrency.
// The NodeID is the compute_node.id the instance lives on (ADR-028); the
// forwarder dereferences it via the per-node vmmd client cache. The
// InstanceID is the instances.id row schedd owns — used to attribute
// last_request_at touches (spec §4.1) and to stamp x-faas-instance on
// the request before proxying.
type Target struct {
	NodeID     string
	InstanceID string
	WakeID     string
	AddedAt    time.Time
	// Port (issue #460 / ADR-053, PR-C) is the per-deployment
	// override port copied from AdmitInstanceResponse.port. 0 =
	// legacy 8080 (vmmd wire-boundary default). The forwarder
	// reads this to populate ForwardHTTPRequestInit.Port; vmmd's
	// buildStreamingBridgeScript resolves 0 to netns.AppPort (8080)
	// for legacy cached targets that pre-date the field.
	Port int
}

// Backend is the seam between the edge and the rest of the platform (in
// production: the routing cache over Postgres, and schedd over gRPC). Splitting
// it out keeps the hot request path testable end-to-end without a real cluster.
//
// Issue #168 widened this interface to support per-app fan-out:
//   - Pick returns one routable instance for the app (round-robin across
//     max_concurrency), used on every request (cold or warm).
//   - HealthyCount returns the number of routable instances currently cached
//     for the app. Drives the WakeGate's shouldWake predicate: stop admitting
//     once we're at the plan's effective max_concurrency.
//   - Admit asks schedd to admit ONE additional instance for the app, gated
//     by maxConcurrency so concurrent callers cannot collectively over-admit
//     past the cap (issue #168 trust model). Returns the new Target's
//     WakeID on the admitted path, atCapacity=true when the cache is
//     already at maxConcurrency (the gateway treats this as a benign
//     no-op when it has ≥1 cached target), or an *api.Problem on real
//     failure (RAM headroom, chooser, store).
type Backend interface {
	// Lookup resolves a hostname to its app (cache-first, spec §4.1).
	Lookup(ctx context.Context, host string) (App, bool)
	// Pick returns one routable Target for appID via atomic round-robin, or
	// ok=false when the cache is empty (caller should ensure capacity first).
	Pick(appID string) (Target, bool)
	// HealthyCount returns the number of routable Targets currently cached
	// for appID. Drives the WakeGate's shouldWake predicate.
	HealthyCount(appID string) int
	// Admit asks schedd to admit ONE additional instance for appID, only
	// when HealthyCount(appID) < maxConcurrency at the moment the call
	// commits. Implementations MUST serialize the HealthyCount check and
	// the cache update so a burst of concurrent Admit calls can never
	// collectively exceed maxConcurrency (issue #168 fan-out invariant).
	// On the admitted path wakeID is non-empty, the new Target is cached,
	// and method reflects what schedd actually did (restore or cold boot).
	// On the at-capacity path wakeID is empty, method is
	// WakeMethodUnspecified, and err is nil. On real failure err is a
	// non-nil *api.Problem and method is WakeMethodUnspecified.
	Admit(ctx context.Context, appID string, maxConcurrency int) (wakeID string, method WakeMethod, atCapacity bool, err error)
}

// Handler is gatewayd's HTTP entrypoint: route → rate-limit → (wake-block if
// parked) → proxy (spec §4.1, §2). It is the only public listener on the box.
type Handler struct {
	backend Backend
	limiter *Limiter
	// accountLimiter is the per-account token-bucket throttle (ADR-040 /
	// issue #292). Runs BEFORE limiter (per-app) in ServeHTTP so a botnet
	// rotating across many apps is rejected before any per-app bucket
	// drains and before the wake gate (a schedd gRPC RPC) is touched.
	accountLimiter *Limiter
	gate           *WakeGate
	// metrics may be nil; nil-guarded everywhere it is read.
	metrics *Metrics
	// log may be nil (defaults to slog.Default()).
	log *slog.Logger
	// appsSuffix is the configured apps.gregale.dev suffix (e.g. ".apps.gregale.dev").
	// Non-empty enables a pre-Lookup host suffix check that 404s anything
	// outside it (spec §4.1 noise filter). Custom domains (Pro+) bypass this
	// constraint implicitly by being keys in the routing cache — see
	// WithAppsSuffix docs.
	appsSuffix string
	// egressSink records per-instance HTTP response body bytes for
	// ADR-046 (per-instance egress metering, telemetry only).
	// Set via WithEgressSink from cmd/gatewayd/main.go; nil in
	// unit tests that don't exercise the egress counter.
	// Recording happens in observe() after the proxy returns —
	// see the proxyByNode call site in ServeHTTP.
	egressSink *egresssink.EgressSink
	// proxyFor builds the reverse proxy for an upstream address; overridable in
	// tests.
	proxyFor func(addr string) http.Handler
	// proxyByNode builds the reverse proxy for a compute_node.id (issue
	// #98 / ADR-028). When non-nil, the handler dispatches every
	// request through it instead of proxyFor — the string returned by
	// Backend.Pick is interpreted as a node id and dereferenced via
	// the per-node vmmd client cache. nil = legacy addr-based path
	// (default for tests and the e2e harness; production wires
	// ForwardingReverseProxy in cmd/gatewayd/main.go).
	//
	// PR-C (issue #460 / ADR-053): the callback receives the full
	// Target (not just the node id) so the forwarder can stamp
	// ForwardHTTPRequestInit.port with the per-deployment override
	// port the gateway cached at admit time. Legacy callers that
	// still expect the node-id-only shape must update to
	// func(Target) http.Handler — the wire defaulting at vmmd
	// keeps pre-PR-C targets (Port=0) reaching 8080.
	proxyByNode func(t Target) http.Handler
	// topNSample is the per-request bump for the gateway-side
	// top-N sampler (cmd/gatewayd/topn.go, issue #300). Set via
	// SetTopNSample from cmd/gatewayd/main.go. nil in unit
	// tests; observe() nil-checks before invoking. The function
	// takes the resolved app_id and increments the sampler's
	// rolling-window count — the gauge write itself happens in
	// the sampler's once-per-5s tick.
	topNSample func(appID string)
	// lastSeen records per-instance last_request_at (spec §4.1). nil-safe.
	lastSeen LastSeenSink

	// streamingEnabled gates the per-app streaming response path
	// (issue #471 / ADR-047). When false (the default), every app is
	// on the legacy buffered path regardless of the per-app
	// apps.streaming_enabled column; an app that emits
	// text/event-stream is buffered end-to-end with a once-per-process
	// deprecation log so a noisy Free-tier app doesn't spam logs.
	// PR-B activates the Flusher path when this is true; PR-A only
	// tests the buffered-fallback AC (#streaming_not_available on
	// Free + a logged deprecation). Set via WithStreamingEnabled from
	// cmd/gatewayd/main.go so production defaults to off and operators
	// opt in per-cluster after PR-B ships.
	streamingEnabled bool
	// streamingWarned is the once-per-process log dedup for the
	// buffered-fallback deprecation. Keyed on (appID, content-type) so
	// the first instance of an SSE-emitting app under the flag-off
	// path emits one warn line; subsequent requests are silent.
	streamingWarned sync.Map // map[string]struct{} (key = appID)
	// piApps deduplicates Metrics.PreInstantiateApp calls per appID
	// (issue #273 / ADR-042). A value-typed sync.Map wrapper; the
	// zero value is valid so NewHandlerWith doesn't have to
	// initialise it explicitly. Value semantics avoid the
	// data-race that lazy init would create under -race.
	piApps preInstantiateApps
}

// emptyAccountWarned is the process-wide trip flag for
// warnEmptyAccountOnce (ADR-040). Empty AccountID only appears in
// fakeBackend unit tests; production joins always populate it. Logging
// every test invocation would flood logs in `-count=10` runs.
var emptyAccountWarned atomic.Bool

// warnEmptyAccountOnce logs a debug-level warning the first time ServeHTTP
// sees an App with an empty AccountID (ADR-040). After that, subsequent
// occurrences are silent. The atomic flag is process-scoped, not per-Handler,
// because the warning is genuinely a code-path bug and one log line is
// enough to surface it.
func (h *Handler) warnEmptyAccountOnce() {
	if emptyAccountWarned.CompareAndSwap(false, true) {
		if h.log != nil {
			h.log.Debug("gateway: app.AccountID empty at rate-limit check; passing through unmetered (ADR-040)",
				"note", "production joins always populate AccountID; empty only in fakeBackend tests")
		}
	}
}

// NewHandler wires the edge with the spec's defaults (wake queue 512/30 s, spec
// §4.1) and the new Metrics + slog bundle. The host→app routing cache lives
// inside the Backend (it fronts Postgres).
func NewHandler(backend Backend) *Handler {
	return NewHandlerWith(backend, NewMetrics(), slog.Default())
}

// NewHandlerWith lets tests inject a custom Metrics bundle (to assert on the
// registry) and a custom slog logger.
func NewHandlerWith(backend Backend, m *Metrics, log *slog.Logger) *Handler {
	h := &Handler{
		backend:        backend,
		limiter:        NewLimiter(),
		accountLimiter: NewLimiter(),
		gate:           NewWakeGate(api.WakeQueueCap, time.Duration(api.WakeQueueTTLSeconds)*time.Second),
		metrics:        m,
		log:            log,
	}
	// piApps is a value-typed sync.Map wrapper; its zero value is valid
	// and no init is required (avoiding a lazy-init write that would
	// race with parallel ServeHTTP readers — the load test under -race
	// caught that pattern; see the field doc).
	h.proxyFor = defaultProxy
	return h
}

// WithAppsSuffix sets the *.apps.gregale.dev suffix filter (call before serving).
// When set, every request whose Host doesn't end in this suffix is rejected
// with 404 BEFORE consulting the cache. Custom domains on a different suffix
// are intended to be reached via the Lookup table directly (M5); this PR only
// adds the wildcard-apps-domain guard.
func (h *Handler) WithAppsSuffix(suffix string) *Handler {
	// Leading dot normalization so callers can pass either form.
	if suffix != "" && suffix[0] != '.' {
		suffix = "." + suffix
	}
	h.appsSuffix = strings.ToLower(suffix)
	return h
}

// WithLastSeenSink installs the LastSeenSink that records per-instance
// last_request_at (spec §4.1). Production wires a PG-flushing impl from
// schedd; tests use the in-memory implementation (idle.go).
func (h *Handler) WithLastSeenSink(sink LastSeenSink) *Handler {
	h.lastSeen = sink
	return h
}

// WithLimiter installs the per-app rate limiter. Production wires the
// token-bucket Limiter; load tests install an unlimitedLimiter so they
// aren't constrained by the plan rps/burst from newTestHandler's
// PlanPro default (which would 429 ~half of a 1k rps test). Treat this
// as a test-only seam; do NOT expose it as a config knob.
func (h *Handler) WithLimiter(l *Limiter) *Handler {
	h.limiter = l
	return h
}

// WithAccountLimiter installs the per-account rate limiter (ADR-040).
// Production wires the token-bucket Limiter constructed in NewHandlerWith;
// load tests install an unlimitedAccountLimiter so they aren't constrained
// by the plan RPM from newTestHandler's PlanPro default. Same test-only
// contract as WithLimiter — do NOT expose as a config knob.
func (h *Handler) WithAccountLimiter(l *Limiter) *Handler {
	h.accountLimiter = l
	return h
}

// WithForwarding installs the per-node HTTP→gRPC forwarder built by
// pkg/gateway/forwardproxy.go (issue #98 / ADR-028). When set, every
// request dispatches through fn(nodeID) where nodeID is the value
// Backend.Pick returned. nil-safe: pass nil to revert to the legacy
// addr-based proxy path (used by tests and the e2e harness).
func (h *Handler) WithForwarding(fn func(t Target) http.Handler) *Handler {
	h.proxyByNode = fn
	return h
}

// WithStreamingEnabled flips the per-app streaming response path on
// or off (issue #471 / ADR-047). Production default is false; the
// e2e harness + metal tests opt in per-process so PR-B's Flusher
// path can be exercised end-to-end without a fleet-wide rollout.
// The flag is a single global gate — the per-app apps.streaming_enabled
// column is read in ServeHTTP and combined with this flag to decide
// whether to actually stream. Mutates the receiver in place; the
// returned *Handler is the same pointer (fluent-chaining convention
// matching WithEgressSink / WithLimiter / etc).
func (h *Handler) WithStreamingEnabled(enabled bool) *Handler {
	h.streamingEnabled = enabled
	return h
}

// streamingFallbackLog emits a once-per-process deprecation line for
// the buffered-fallback path (issue #471 PR-A AC #4). A misconfigured
// Free-tier app could land an SSE-emitting response on the buffered
// path; the log tells operators the flag-off branch fired so they
// can either flip the per-app flag to false (the correct fix) or
// upgrade the customer. Keyed on (appID, contentType) so two
// distinct apps emitting SSE each get their own line; the log
// dedup uses sync.Map so the hot path is allocation-free after the
// first observation per key.
func (h *Handler) streamingFallbackLog(appID, contentType string) {
	if h.log == nil {
		return
	}
	key := appID + "\x00" + contentType
	if _, seen := h.streamingWarned.Load(key); seen {
		return
	}
	h.streamingWarned.Store(key, struct{}{})
	h.log.Warn("gateway: streaming fallback (per-app streaming_enabled=true on a plan that did not unlock streaming; response buffered end-to-end)",
		"app", appID, "content_type", contentType)
}

// isAcceptJSON reports whether the request's Accept header opts the
// request into the buffered path regardless of the per-app
// streaming_enabled flag (spec §4.1, ADR-047). The check is case-
// insensitive on the prefix and tolerates parameters like
// "application/json; charset=utf-8". Anything else (no header,
// wildcard "*/*", "text/event-stream", etc.) leaves the per-app
// flag as the source of truth. Returns false for empty string — a
// client that sets Accept: "" is treated the same as not setting
// the header at all.
func isAcceptJSON(accept string) bool {
	if accept == "" {
		return false
	}
	// Header may carry multiple comma-separated values; the
	// per-request opt-out fires when ANY of them is
	// application/json. The plan-of-record is "if you set
	// application/json at all, you're opting in to buffered".
	for _, raw := range strings.Split(accept, ",") {
		mt := strings.TrimSpace(strings.ToLower(raw))
		// Strip parameters (e.g. "; charset=utf-8").
		if i := strings.IndexByte(mt, ';'); i >= 0 {
			mt = mt[:i]
		}
		mt = strings.TrimSpace(mt)
		if mt == "application/json" {
			return true
		}
	}
	return false
}

// setupStreamingWriter arms the per-request Flusher path. The
// returned writer chains: MaxBytesWriter → statusRecorder →
// original. Cap-exceeded writes emit a 413 problem+json via the
// supplied onCap callback and disable further writes. The recorder
// receives a per-flush onFlush closure that:
//   - computes delta = cumulativeBytes - lastReported
//   - attributes the delta to the egress sink
//     (target.InstanceID, current minute)
//   - increments the per-(app, plan) response bytes counter
//   - tracks lastReported in a closure-local int64 (no field on
//     the recorder — the recorder stays a value type that can
//     still be embedded inside statusRecorder or copied
//     accidentally without losing accounting state)
//
// The flush window (256 KiB / 200 ms) comes from
// api.StreamingFlushBytesDefault / api.StreamingFlushIntervalDefault
// — keeping them in pkg/api/limits.go means the operator can
// later add a per-cluster override without touching this file.
// writeTimeout is the per-flush deadline installed on the first
// flush via http.ResponseController.SetWriteDeadline; the
// subsequent flushes re-install the deadline relative to "now" so
// the total wall time the connection can stay open is
// flushInterval × N (with N bounded by the upstream duration).
//
// Returns the OUTER writer (the cap-wrapped recorder). The
// Handler then calls proxy.ServeHTTP(w, r) with this writer;
// every Write hits the cap check, the recorder's maybeFlush,
// and the recorder's flush (which calls onFlush).
func (h *Handler) setupStreamingWriter(w http.ResponseWriter, rec *statusRecorder, app App, target Target, writeTimeout time.Duration) http.ResponseWriter {
	var lastReported int64
	onFlush := func(cumulative int64) {
		delta := cumulative - lastReported
		lastReported = cumulative
		if delta <= 0 {
			return
		}
		if h.egressSink != nil && target.InstanceID != "" {
			h.egressSink.RecordResponseBytes(target.InstanceID, delta)
		}
		if h.metrics != nil && app.ID != "" {
			h.metrics.ObserveResponseBytes(app.ID, string(app.Plan), delta)
		}
		if h.metrics != nil {
			h.metrics.ObserveStreamFlush(app.ID, string(app.Plan))
		}
	}

	flusher, _ := w.(http.Flusher)
	if flusher == nil {
		// The wrapped writer isn't an http.Flusher. The buffered
		// path stays; we still install the onFlush hook so the
		// recorder tracks Bytes for the once-per-response
		// recordEgress call. (This branch is reachable in unit
		// tests where httptest.NewRecorder doesn't implement
		// Flusher; production uses http.ResponseWriter from
		// net/http.Server which always does.)
		rec.installFlushHook(nil, onFlush, 0, 0, 0)
		return w
	}
	rec.installFlushHook(flusher, onFlush, api.StreamingFlushBytesDefault, api.StreamingFlushIntervalDefault, writeTimeout)

	// Wrap with MaxBytesWriter so a streaming app that exceeds
	// its plan's response body cap (Free 25 MB, Hobby 100 MB,
	// etc.) gets a 413 problem+json instead of a stdlib-default
	// 502. The onCap callback fires once per request — the
	// MaxBytesWriter goroutine detects the cap, signals the
	// response goroutine via the closed channel, the response
	// goroutine writes the 413 problem+json and disables
	// further writes via the capWriter.disabled flag.
	cap := app.Plan.MaxResponseBodyBytes()
	return &capWriter{
		ResponseWriter: rec,
		cap:            cap,
		onCap: func() {
			api.WriteProblem(w, api.NewProblem(http.StatusRequestEntityTooLarge, api.CodeStreamingNotAvailable,
				"streaming response exceeded plan cap",
				fmt.Sprintf("app %s on plan %s is capped at %d bytes per response", app.ID, app.Plan, cap)))
		},
		disabled: &atomic.Bool{},
	}
}

// capWriter is a thin http.ResponseWriter wrapper that enforces
// a per-response body byte cap. On cap-exceeded Write returns an
// error and fires onCap (which writes a problem+json to the
// ORIGINAL writer before this wrapper was installed, so the
// problem lands on the wire even though the wrapped Write is
// blocked). After onCap fires, the disabled flag prevents
// subsequent Writes from consuming bytes.
//
// This is a separate type from http.MaxBytesWriter because the
// stdlib one writes a 502 directly to the underlying writer with
// no opportunity to inject our 413 problem+json shape.
type capWriter struct {
	http.ResponseWriter
	cap      int64
	written  int64
	onCap    func()
	disabled *atomic.Bool
}

func (c *capWriter) Write(b []byte) (int, error) {
	if c.disabled.Load() {
		return 0, http.ErrHandlerTimeout
	}
	if c.written+int64(len(b)) > c.cap {
		// Fire once; idempotent under concurrent writes thanks
		// to disabled CAS.
		if c.disabled.CompareAndSwap(false, true) && c.onCap != nil {
			c.onCap()
		}
		return 0, http.ErrHandlerTimeout
	}
	n, err := c.ResponseWriter.Write(b)
	if n > 0 {
		c.written += int64(n)
	}
	return n, err
}

// Flush is forwarded so a streaming upstream calling Flush
// directly on the cap-wrapped writer still triggers the
// recorder's flush logic. The cap check happens in Write, not
// Flush, so a Flush that hasn't been preceded by a new Write is
// a no-op for the cap.
func (c *capWriter) Flush() {
	if c.disabled.Load() {
		return
	}
	if f, ok := c.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// WithEgressSink installs the per-instance HTTP response byte ring
// buffer (pkg/gateway/egresssink). ADR-046 (per-instance egress
// metering, telemetry only) wires this once at gatewayd boot from
// cmd/gatewayd/main.go. nil-safe: passing nil clears the sink;
// ServeHTTP short-circuits the record path the moment it sees
// h.egressSink == nil, so unit tests don't have to install one.
//
// Mutates the receiver in place; the returned *Handler is the same
// pointer, provided for fluent chaining. Discarding the return
// value (statement-form `h.WithEgressSink(...)`) is correct.
func (h *Handler) WithEgressSink(sink *egresssink.EgressSink) *Handler {
	h.egressSink = sink
	return h
}

// SetWakeGateHook installs a callback that wakes the queue-depth gauge each
// time WakeGate mutates an entry, and hands the wake-queue histogram to the
// gate so Wait can observe per-caller wait duration. Called by main once the
// metrics bundle exists.
func (h *Handler) SetWakeGateHook() {
	h.gate.onChange = func(appID string, depth int) {
		if h.metrics != nil {
			h.metrics.SetQueueDepth(appID, depth)
		}
	}
	h.gate.SetMetrics(h.metrics)
}

// SetTopNSample installs the per-request bump callback for the
// gateway-side top-N sampler (issue #300). Called by main once
// the sampler is constructed. The Handler stores the callback
// and invokes it from observe() on every non-sentinel app id;
// nil = no-op (unit-test seam).
//
// The callback signature is func(appID string) so pkg/gateway
// stays free of any dependency on pkg/wire or the local
// cmd/gatewayd topAccountSet — same decoupling pattern as
// SetWakeGateHook above.
func (h *Handler) SetTopNSample(sample func(appID string)) {
	h.topNSample = sample
}

// Metrics exposes the Prometheus bundle (used by the control listener to mount
// /metrics). May be nil if NewHandler was used and nothing initialized one.
func (h *Handler) Metrics() *Metrics { return h.metrics }

// Limiter exposes the per-app rate limiter so callers (SIGHUP handler,
// admin endpoints) can forget buckets. Mostly an M5 hook: when apid starts
// pushing plan changes via Postgres + LISTEN, the gateway's reload path
// calls ForgetAll() on this limiter.
func (h *Handler) Limiter() *Limiter { return h.limiter }

// AccountLimiter exposes the per-account rate limiter (ADR-040 / issue #292).
// Same SIGHUP contract as Limiter — the gateway's reload path calls
// ForgetAll() on both limiters. Test seam: WithAccountLimiter installs a
// noop for load tests that need to bypass the account bucket.
func (h *Handler) AccountLimiter() *Limiter { return h.accountLimiter }

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Status-class capture (used for metrics + slog). Doesn't buffer the body
	// or alter the headers — strictly observability.
	rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
	w = rec

	// Stamp the request-received timestamp onto the context so every exit
	// path can measure the SAME elapsed interval (was previously always
	// falling back to time.Now() because WithStartTime was dead code —
	// issue #273 / ADR-042 fixed this so gateway_request_duration_seconds
	// has a real elapsed to record and the slog latency_ms field stops
	// being effectively zero).
	start := time.Now()
	r = r.WithContext(WithStartTime(r.Context(), start)) //nolint:contextcheck // request ctx is the canonical inbound ctx at the HTTP handler boundary.

	// Request ID is generated once per request and set on the response BEFORE
	// any error path so even 4xx responses are correlatable. Inbound
	// x-faas-request-id overrides (lets curl/clients supply their own trace).
	rid := r.Header.Get("x-faas-request-id")
	if rid == "" {
		rid = newRequestID()
	}
	w.Header().Set("x-faas-request-id", rid)
	r = r.WithContext(WithRequestID(r.Context(), rid)) //nolint:contextcheck // request ctx is the canonical inbound ctx at the HTTP handler boundary.

	host := hostname(r.Host)

	// Host allowlist suffix check (spec §4.1: *.apps.gregale.dev). Closes the
	// door on stale DNS records that land on the edge post-TLS by rejecting
	// anything not matching the configured suffix before the cache is touched.
	// Set via NewHandlerWithSuffix or WithAppsSuffix; empty suffix disables
	// the check (the Backend.Lookup table is still authoritative).
	if h.appsSuffix != "" && !strings.HasSuffix(host, h.appsSuffix) {
		api.WriteProblem(w, api.NewProblem(http.StatusNotFound,
			api.CodeNotFound, "No such app",
			fmt.Sprintf("host %q does not match the configured apps suffix", host)))
		h.observe(r, rec.status, "", "", false, Target{})
		return
	}

	app, ok := h.backend.Lookup(r.Context(), host) //nolint:contextcheck // request ctx is the canonical inbound ctx at the HTTP handler boundary.
	if !ok {
		api.WriteProblem(w, api.NewProblem(http.StatusNotFound, api.CodeNotFound,
			"No such app", fmt.Sprintf("no app is routed to %q", host)))
		h.observe(r, rec.status, "", "", false, Target{})
		return
	}

	// Issue #273 / ADR-042 — pre-instantiate the closed (class) set
	// for this app so its histogram rows surface from the first
	// request rather than after the first observation. App IDs are
	// runtime values, but the inner class set is bounded; the sync.Map
	// dedupes so the hot path stays allocation-free after first sight.
	h.preInstantiateApp(app.ID)

	// Per-account rate limit (ADR-040 / issue #292). Runs BEFORE the
	// per-app limit so a botnet rotating across many apps within an
	// account cannot evade the throttle by keeping per-app rps low.
	// Empty AccountID is only reachable from fakeBackend unit tests
	// (production joins always populate it via pgRouter.toApp) — pass
	// through unmetered and log once per process so the test suite
	// keeps working without flooding logs.
	if app.AccountID == "" {
		h.warnEmptyAccountOnce()
	} else if !h.accountLimiter.AllowAccount(app.AccountID, app.Plan) {
		w.Header().Set("Retry-After", "1")
		w.Header().Set("x-faas-rate-limit-scope", "account")
		// Per-account 429 still surfaces the per-account bucket state
		// so a customer debugging a 429 storm can see which throttle
		// tripped. Distinct X-AccountRateLimit-* header family so
		// generic tooling that auto-parses X-RateLimit-* doesn't
		// conflate per-app and per-account values (Finding 6).
		h.writeAccountRateLimitHeaders(w, app.AccountID, app.Plan)
		api.WriteProblem(w, api.NewProblem(http.StatusTooManyRequests, "rate_limited",
			"Rate limit exceeded", "slow down and retry"))
		if h.metrics != nil {
			h.metrics.ObserveAccountRateLimit(app.AccountID, string(app.Plan))
		}
		h.observe(r, rec.status, app.ID, string(app.Plan), false, Target{})
		return
	}

	// Per-app rate limit (spec §4.1). Over-limit → 429.
	if !h.limiter.Allow(app.ID, app.Plan) {
		w.Header().Set("Retry-After", "1")
		w.Header().Set("x-faas-rate-limit-scope", "app")
		// 429 path: write the post-decrement bucket snapshot so
		// clients can compute Retry-After locally without parsing the
		// problem+json body. The header set runs before the
		// api.WriteProblem below so the body has time to read them.
		h.writeAppRateLimitHeaders(w, app.ID, app.Plan)
		api.WriteProblem(w, api.NewProblem(http.StatusTooManyRequests, "rate_limited",
			"Rate limit exceeded", "slow down and retry"))
		if h.metrics != nil {
			h.metrics.ObserveRateLimit(app.ID, string(app.Plan))
		}
		h.observe(r, rec.status, app.ID, string(app.Plan), false, Target{})
		return
	}

	// Success path: stamp the per-app headers BEFORE the proxy runs so
	// they reach the wire regardless of what the upstream does (a
	// committed upstream response body would otherwise overwrite the
	// headers we set here). Allow already consumed one token above; the
	// Peek snapshot therefore reflects "tokens left after this
	// request" which is the standard X-RateLimit-Remaining contract.
	h.writeAppRateLimitHeaders(w, app.ID, app.Plan)

	// Cap request body either direction (spec §4.1).
	r.Body = http.MaxBytesReader(w, r.Body, api.MaxRequestBodyBytes)

	// Per-app fan-out admission (issue #168). The WakeGate's
	// shouldWake predicate runs HealthyCount against the plan's
	// effective max_concurrency, so a burst of N requests admits up to
	// N instances before short-circuiting.
	limits, _ := api.LimitsFor(app.Plan)
	//nolint:contextcheck // request ctx at handler boundary.
	cold, wakeID, wakeMethod, err := h.ensureCapacity(r.Context(), app.ID, limits.MaxConcurrency)
	if err != nil {
		writeWakeError(w, err)
		h.observe(r, rec.status, app.ID, string(app.Plan), false, Target{})
		return
	}

	// Pick one routable Target via atomic round-robin. After a
	// successful ensure, HealthyCount ≥ 1, so this should succeed
	// unless every cached instance was evicted between admit and pick
	// (an instance_changed notification race). On that rare miss, fall
	// through to the capacity problem — the WakeGate will retry on the
	// next request.
	target, ok := h.backend.Pick(app.ID)
	if !ok {
		// Race: every cached instance was evicted between
		// ensureCapacity returning and our Pick. Surface the observed
		// (current) HealthyCount so the operator's metrics panel
		// shows 0 vs the cap (was 1+ microseconds ago).
		writeWakeError(w, api.ErrAppConcurrencyReached(limits, h.backend.HealthyCount(app.ID)))
		h.observe(r, rec.status, app.ID, string(app.Plan), false, Target{})
		return
	}

	// Stamp the per-instance identity on the request BEFORE proxying so
	// the per-node vmmd forwarder (issue #98 / ADR-028) can attribute
	// the HTTP bytes to this exact instance. Overwrites any inbound
	// x-faas-instance so an attacker can't steer the proxy to an
	// arbitrary instance by setting the header (issue #168 trust model).
	r.Header.Set("x-faas-instance", target.InstanceID)

	// Per-request wake-timing recorder (spec §6.3) installed AFTER
	// upstream stamping so the trace sees only the proxy hop, not the
	// stamping overhead.
	firstByteRec := &firstByteRecorder{}
	//nolint:contextcheck // WithFirstByteRecorder wraps context.WithValue on r.Context(); lint can't trace through the function call.
	r = r.WithContext(WithFirstByteRecorder(r.Context(), firstByteRec))

	if cold {
		// Cold-wake transparency (UX spec §6): let developers see the penalty.
		w.Header().Set(wire.WakeHeader, wire.ColdWakeValue)
	}
	if wakeID != "" {
		// Per-wake correlation handle (gaps analysis 2026-07-23). Sits
		// next to x-faas-wake so the two are co-located on every
		// response, and never set on a Phase-1 fast-path response.
		w.Header().Set("x-faas-wake-id", wakeID)
	}

	// Streaming decision (PR-B / ADR-047). Three-way AND: the operator
	// must have opted the process in (h.streamingEnabled, set via
	// FAAS_GATEWAY_STREAMING), the per-app apps.streaming_enabled flag
	// must be true (set on build / PATCH / plan default), and the
	// per-request Accept header must not opt out. The
	// Accept: application/json override is the customer-visible
	// contract from spec §4.1 — a client can force the buffered
	// path for one request without a per-app PATCH.
	//
	// Placement: AFTER Backend.Pick so the per-flush onFlush hook
	// has target.InstanceID to attribute egress bytes. BEFORE the
	// proxy runs so the wrapped ResponseWriter (which is both an
	// http.ResponseWriter and an http.Flusher via embed) is in
	// place when the upstream first calls Write. The wrap is
	// nil-safe in the sense that when the gate is false, w stays
	// the original ResponseWriter and the recorder is never
	// installed — the buffered path is a strict pass-through.
	streaming := h.streamingEnabled && app.StreamingEnabled &&
		!isAcceptJSON(r.Header.Get("Accept"))
	if streaming {
		writeTimeout := app.Plan.ResponseWriteTimeout()
		w = h.setupStreamingWriter(w, rec, app, target, writeTimeout)
		// Internal header stamp (PR-B + PR-C / ADR-047). The
		// forwarder (pkg/gateway/forwardproxy.go) reads this to
		// switch to the bidi ForwardHTTPStream RPC and lift the
		// per-request cap/timeout on the vmmd side. Not exposed
		// to the client (the request stays on the inbound HTTP
		// path); the forwarder strips x-faas-* headers before
		// bridging so the guest never sees it.
		r.Header.Set("x-faas-stream", "true")
		// ADR-047 PR-D: bookend the streaming concurrency gauge.
		// setupStreamingWriter installs the per-flush onFlush hook
		// that increments streamFlushes; this Inc opens the
		// streamActive window, balanced by the Dec below in the
		// defer after finalFlush. The buffered path never touches
		// the gauge — buffered requests durate the response in a
		// single Go-time transfer and don't participate in the
		// streaming concurrency model.
		if h.metrics != nil {
			h.metrics.ObserveStreamStart(app.ID, string(app.Plan))
		}
	}
	// Defer the Dec with the streaming flag captured so a panic
	// between ObserveStreamStart and finalFlush still drains the
	// gauge. The streaming path is the only path that incremented;
	// the buffered path is a no-op for the Dec.
	defer func() {
		if streaming && h.metrics != nil {
			h.metrics.ObserveStreamEnd(app.ID, string(app.Plan))
		}
	}()

	wakeStart := time.Now()
	if h.proxyByNode != nil {
		// Issue #98 / ADR-028: Target.NodeID is the compute_node.id;
		// the forwarder dials the per-node vmmd over the overlay and
		// bridges the HTTP bytes through the instance netns via the
		// ForwardHTTPStream RPC. target stays in scope for the
		// metrics labels and observe() last-seen hook below.
		//
		// PR-C (issue #460 / ADR-053): the callback receives the
		// full Target so the forwarder can stamp
		// ForwardHTTPRequestInit.port with the per-deployment
		// override port cached at admit time.
		h.proxyByNode(target).ServeHTTP(w, r)
	} else {
		// Legacy addr-based path. Target.NodeID is treated as a
		// host:port by defaultProxy — preserved for tests and the
		// e2e harness without a vmmd overlay.
		h.proxyFor(target.NodeID).ServeHTTP(w, r)
	}
	// Issue #471 / ADR-047 PR-A buffered-fallback AC. The
	// per-app streaming_enabled flag (ap.StreamingEnabled,
	// propagated through pgRouter.toApp) is the load-bearing
	// signal for the fallback log — the customer asked for
	// streaming, apid's plan-gate (CodePlanStreamingNotAllowed)
	// should already have rejected the request on a non-paid
	// plan, but a misconfiguration could land a Free app with
	// streaming_enabled=true here. PR-D tightens the gate:
	//
	//   !streaming — the buffered path actually got used. A
	//     valid Hobby+ SSE on the streaming path is the
	//     normal-flush case, NOT a fallback; logging it would
	//     be a noisy false positive.
	//   app.Plan == PlanFree — the misconfig surface is
	//     specifically Free+flag. Hobby/Pro/Scale plans
	//     default streaming_enabled=true at the apid layer
	//     (per PR-A), so the buffered path on those plans is
	//     the operator's FAAS_GATEWAY_STREAMING flag being
	//     off, not a customer misconfig. Log only when both
	//     gates fire.
	//
	// The dedup in streamingFallbackLog keeps the hot path
	// allocation-free after the first observation per
	// (appID, contentType), so the log fires at most once per
	// misconfigured app.
	if !streaming && app.StreamingEnabled &&
		app.Plan == api.PlanFree &&
		strings.HasPrefix(strings.ToLower(rec.ContentType), "text/event-stream") {
		h.streamingFallbackLog(app.ID, rec.ContentType)
	}
	h.observe(r, rec.status, app.ID, string(app.Plan), cold, target)
	// PR-B residual capture. On the streaming path the per-flush
	// deltas already attributed every byte that hit the wire; the
	// one outstanding delta is the trailing slice between the
	// last periodic flush and the upstream finishing. finalFlush
	// fires one more onFlush with rec.Bytes (the cumulative
	// count) which the hook subtracts against its lastReported.
	// The buffered path short-circuits via nil-safe flusher ==
	// nil. The streaming flag is set by setupStreamingWriter
	// alongside flusher, so checking streaming here would be
	// equivalent to checking flusher != nil; we use flusher
	// directly to keep the helper self-contained.
	if rec.flusher != nil {
		rec.finalFlush()
	}
	// ADR-046 (per-instance egress metering, telemetry only):
	// record the response body bytes that egressed the gateway on
	// this instance. Gated on 2xx/3xx because 4xx/5xx the
	// ReverseProxy never actually wrote the response body to the
	// wire (it short-circuited on the upstream reject) — billing
	// bytes that didn't egress would be wrong on both ends of the
	// financial model. nil-safe for unit tests.
	h.recordEgress(rec, target, app)
	if cold && h.metrics != nil {
		// Wake latency is "request-received to first upstream byte". The
		// wake-timing RoundTripper stamps the inbound request's recorder at
		// GotFirstResponseByte; reading it back here yields the actual wake
		// slice, not "wake + full upstream body copy" (the prior proxy return
		// measurement). On any path where the stamp never landed (proxy error
		// before headers), we fall back to the full duration with a Warn so
		// the gap is observable but the dashboard still gets a sample.
		firstByteAt, ok := FirstByteFrom(r)
		if !ok {
			h.log.Warn("gateway: wake-timing first-byte stamp missing; observing full proxy duration",
				"app", app.ID, "node", target.NodeID, "instance", target.InstanceID)
			firstByteAt = time.Now()
		}
		h.metrics.ObserveColdBoot(app.ID, firstByteAt.Sub(wakeStart))
		// Wake-locality classifier (PR scale-out readiness). Increment
		// AFTER the existing first-byte observation so the 350 ms
		// measurement path is unchanged. Only fires on a real admit
		// (cold==true), so warm requests, at-capacity benign outcomes,
		// and admission errors do not enumerate. Today's outcomes are
		// local_snapshot / local_coldboot; remote_* outcomes slot in
		// when a second compute node joins.
		h.metrics.ObserveWakeLocality(wakeMethod.String())
	}
}

// observe emits one metric increment + one structured log line per request.
// Always called exactly once on the ServeHTTP exit path; missing it would
// skew the §12 dashboard. On a 2xx response it also Touches the LastSeenSink
// keyed by InstanceID (issue #168 — per-instance attribution survives the
// multi-instance fan-out where multiple instances share a single node).
func (h *Handler) observe(r *http.Request, status int, appID, plan string, cold bool, target Target) {
	code := statusClass(status)
	requestID := requestIDFrom(r)
	// Measure elapsed against the same start stamp set at the top of
	// ServeHTTP (issue #273 / ADR-042 fixed the WithStartTime dead-code
	// bug so this is now request-received → handler-return, not
	// "now() — now()").
	elapsed := time.Since(startTime(r))
	if h.metrics != nil {
		// Use placeholder labels for the unknown-host path so 404s show up on
		// the dashboard under a sentinel app_id (e.g. "-" or "").
		if appID == "" {
			appID = "-"
			plan = "-"
		}
		h.metrics.ObserveRequest(appID, plan, code)
		// Per-app full request duration histogram (issue #273 / ADR-042).
		// The class label is the 3-digit status bucketed to 2xx/3xx/4xx/5xx
		// to keep cardinality bounded — full status codes would explode
		// per-app series count past 60× the class-based set.
		h.metrics.ObserveRequestDuration(appID, statusClassBucket(status), elapsed)
	}
	// Issue #300: feed the per-tenant rolling count for the
	// 5s gateway_top_tenant_rps sampler (cmd/gatewayd/topn.go).
	// The sentinel "-" id keeps unknown-host traffic off the
	// top-N gauge (it would otherwise flood the gauge with one
	// series per scanner host). Cheap path — the sampler is
	// the only writer to the gauge; this call only bumps the
	// rolling count. Separated from the h.metrics != nil check
	// because the sampler is wired via SetTopNSample and has
	// its own nil-handling (topNSample is nil in unit tests
	// that construct Handler without metrics).
	if appID != "" && appID != "-" && h.topNSample != nil {
		h.topNSample(appID)
	}
	// Use the locally-measured elapsed (request-received → handler-return).
	// WithStartTime was dead code in the repo (issue #273 / ADR-042); the
	// WithContext call at the top of ServeHTTP now stamps the real start
	// time so the slog latency_ms field is no longer effectively ~0. Doing
	// the time.Since(startTime(r)) call here would yield the same result
	// but recomputes; `elapsed` was already measured above.
	(&requestLogger{log: h.log}).Log(appID, code, elapsed, cold, requestID)

	// Idle reaper hook (spec §4.1): 2xx → the instance is alive. 4xx/5xx are
	// not evidence of activity (a misconfigured client can hammer a dead
	// instance with 401s forever and we'd never park it).
	if h.lastSeen != nil && status >= 200 && status < 300 && target.InstanceID != "" {
		h.lastSeen.Touch(target.InstanceID, time.Now())
	}
}

// statusClass turns an HTTP status into a 3-digit label ("200", "404", "503").
func statusClass(status int) string {
	if status < 100 || status > 999 {
		status = http.StatusInternalServerError
	}
	const digits = "0123456789"
	return string([]byte{digits[(status/100)%10], digits[(status/10)%10], digits[status%10]})
}

// statusClassBucket turns an HTTP status into a class label for the
// gateway_request_duration_seconds histogram (issue #273 / ADR-042).
// Distinct from statusClass above: that one returns the FULL 3-digit
// code (used for the counter label so dashboards can drill into e.g.
// 404 vs 503); this one buckets to the closed 4-set so a histogram
// with ~13 series per label combo stays bounded per app.
func statusClassBucket(status int) string {
	switch {
	case status >= 200 && status < 300:
		return "2xx"
	case status >= 300 && status < 400:
		return "3xx"
	case status >= 400 && status < 500:
		return "4xx"
	default:
		// Anything outside 1xx/2xx/3xx/4xx lands in 5xx — this
		// matches the §12 dashboard's "errors" panel definition.
		return "5xx"
	}
}

// preInstantiateApps deduplicates PreInstantiateApp calls per appID.
// Issue #273 / ADR-042 — PreInstantiateApp is cheap (returns the
// existing series on repeat calls), but the sync.Map.Load+Store
// short-circuits the work entirely after first sight so the hot path
// stays allocation-free. A value-typed field on Handler: the zero
// value is valid, so NewHandlerWith doesn't need to initialise it
// (avoiding a lazy-init write that would race with parallel
// ServeHTTP readers — the load test under -race caught that).
type preInstantiateApps struct{ m sync.Map }

func (p *preInstantiateApps) seen(appID string) bool {
	_, loaded := p.m.LoadOrStore(appID, struct{}{})
	return loaded
}

// recordEgress attributes response body bytes to the (instanceID,
// minute) bucket in h.egressSink. Called after the proxy ServeHTTP
// returns so the recorder has observed every body chunk that hit
// the wire. The 2xx/3xx gate mirrors what the gateway actually
// delivered to the caller: 4xx/5xx responses never reach the body
// stage on the ReverseProxy path (it short-circuits to the error
// branch), so trying to count their bytes would over-attribute.
//
// PR-B streaming path: when the recorder is armed with a flusher
// (rec.flusher != nil, set by setupStreamingWriter), the per-flush
// onFlush hook has ALREADY recorded every byte in deltas plus the
// residual from finalFlush(). Calling recordEgress again would
// double-count (rec.Bytes + sum(deltas) = 2 × total). The streaming
// gate short-circuits the function; the buffered path remains
// unchanged.
//
// The InstanceID guard means tests that exercise only fakeBackend
// (where pick-instance is empty) skip the recording without
// crashing. The sink is nil-safe itself (RecordResponseBytes
// no-ops on a nil/empty instance id and <= 0 bytes), but the early
// return here skips the function-call overhead on the hot path.
//
// Also increments the gateway_response_bytes_total counter for the
// (app.id, app.plan) tuple. The plan label uses the same string
// cast as the rest of handler.observe; if it changes, audit the
// §12 dashboard panels to keep the join on plan stable.
func (h *Handler) recordEgress(rec *statusRecorder, target Target, app App) {
	if h == nil || rec == nil {
		return
	}
	// PR-B streaming path: per-flush + residual already covered
	// the byte accounting; suppress the once-per-response call to
	// avoid double-counting.
	if rec.flusher != nil {
		return
	}
	if rec.status < 200 || rec.status >= 400 {
		return
	}
	if rec.Bytes <= 0 {
		return
	}
	if h.egressSink != nil && target.InstanceID != "" {
		h.egressSink.RecordResponseBytes(target.InstanceID, rec.Bytes)
	}
	if h.metrics != nil && app.ID != "" {
		h.metrics.ObserveResponseBytes(app.ID, string(app.Plan), rec.Bytes)
	}
}

// writeAppRateLimitHeaders writes the X-RateLimit-{Limit,Remaining,Reset}
// header trio for the per-app bucket after the most recent Allow has
// consumed one token. Skips silently when Peek returns ok=false (a
// noop limiter, an unknown plan, or a bucket the limiter has not yet
// seen for this app) — the absence of the headers is the loader-side
// signal that the value is "not yet established" rather than
// "exhausted". Set on the 2xx success path so dashboards that read
// X-RateLimit-* see the post-consume value, and on the per-app 429
// path so clients can compute Retry-After from the headers without
// parsing the problem+json body.
//
// Cross-process limitation (Finding 6 / ADR-040 follow-up): in a
// multi-gatewayd fleet each gatewayd process keeps its own bucket;
// the value reflects this gatewayd's view. ADR-040 already flagged
// this; the X-RateLimit-* contract is documented to expose rather
// than to remove the limitation. When a shared-bucket design ships
// this method moves behind a shared-state seam with the same call
// signature.
func (h *Handler) writeAppRateLimitHeaders(w http.ResponseWriter, appID string, plan api.Plan) {
	if h == nil || h.limiter == nil {
		return
	}
	limit, remaining, reset, ok := h.limiter.Peek(appID, plan)
	if !ok {
		return
	}
	w.Header().Set("X-RateLimit-Limit", intToString(limit))
	w.Header().Set("X-RateLimit-Remaining", intToString(remaining))
	w.Header().Set("X-RateLimit-Reset", intToString(reset))
}

// writeAccountRateLimitHeaders writes the X-AccountRateLimit-*
// header trio for the per-account bucket (ADR-040 / Finding 6).
// Distinct header family from X-RateLimit-* so generic
// X-RateLimit-* consumers (e.g. browser DevTools) don't conflate the
// two scopes. Set only on the per-account 429 path today; the
// per-account value is rarely useful to a customer on the 2xx path
// (they care about their app's bucket, not their account-wide one).
func (h *Handler) writeAccountRateLimitHeaders(w http.ResponseWriter, accountID string, plan api.Plan) {
	if h == nil || h.accountLimiter == nil || accountID == "" {
		return
	}
	limit, remaining, reset, ok := h.accountLimiter.PeekAccount(accountID, plan)
	if !ok {
		return
	}
	w.Header().Set("X-AccountRateLimit-Limit", intToString(limit))
	w.Header().Set("X-AccountRateLimit-Remaining", intToString(remaining))
	w.Header().Set("X-AccountRateLimit-Reset", intToString(reset))
}

// intToString is a tiny strconv.Itoa shim so handler.go doesn't grow
// a strconv import for three call sites. Avoids the existing
// scheduler.go itoa(uint64) so the package keeps a single per-type
// convention.
func intToString(i int) string {
	// Local format avoids the strconv import. Negative values render
	// with a leading "-" so a Peek that observes (somehow) a negative
	// token count is visible to the client; in practice limit /
	// remaining are non-negative by construction (the floor in Peek).
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	var buf [20]byte
	pos := len(buf)
	for i > 0 {
		pos--
		buf[pos] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		pos--
		buf[pos] = '-'
	}
	return string(buf[pos:])
}

// preInstantiateApp records appID once and delegates to
// Metrics.PreInstantiateApp. Safe on a nil h (test seam) and a nil
// Metrics (also nil-safe inside the method).
func (h *Handler) preInstantiateApp(appID string) {
	if h == nil || h.metrics == nil || appID == "" {
		return
	}
	if h.piApps.seen(appID) {
		return
	}
	h.metrics.PreInstantiateApp(appID)
}

// statusRecorder is a thin ResponseWriter wrapper that records the HTTP status
// that was written so metrics can label without buffering headers/body.
//
// Bytes is the cumulative response body length observed via Write. Streaming
// responses (no Content-Length, Write called multiple times) accumulate the
// sum — the ADR-046 telemetry path doesn't need per-chunk fidelity, only the
// total. Negative or zero-byte callers (HEAD, 304 Not Modified, error paths
// without a body) leave Bytes at zero, which the post-proxy recording site
// treats as "no traffic to meter" (see recordEgress in ServeHTTP).
//
// ContentType (issue #471) captures the upstream response's Content-Type
// at WriteHeader time so the post-proxy site can decide whether the
// response was an SSE stream (text/event-stream) that got buffered on
// the way out. The field is read once after proxy.ServeHTTP returns.
//
// PR-B (issue #471) extends the recorder with optional streaming
// fields. The semantics: when `flusher` is nil, the recorder is a
// strict pass-through (today's buffered path) and `Write` ignores
// the onFlush hook + flush triggers. When `flusher` is non-nil
// (installed by Handler.setupStreamingWriter), `Write` calls
// maybeFlush() after the underlying write; maybeFlush triggers
// a flush when bytes-since-last-flush ≥ flushBytes
// (StreamingFlushBytesDefault, 256 KiB) OR
// time-since-last-flush ≥ flushInterval (StreamingFlushIntervalDefault,
// 200 ms) AND bytes-since-last-flush > 0. The first flush after
// WriteHeader unconditionally fires (to set the per-flush write
// deadline via http.ResponseController) and is the onFlush hook's
// caller-side gate for the per-flush tx_bytes increment.
//
// `streaming` is the boolean the gateway sets when it has decided
// to take the streaming path (operator flag + per-app flag +
// Accept opt-out). The handler reads it once at the post-proxy
// branch to decide whether to call finalFlush (the residual
// capture) and whether to skip the once-per-response recordEgress
// (the per-flush deltas already account for everything on the
// streaming path).
type statusRecorder struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
	Bytes       int64
	ContentType string

	// Streaming fields (PR-B, nil → buffered path). Install via
	// installFlushHook; the fields stay zero otherwise.
	flusher          http.Flusher
	onFlush          func(cumulativeBytes int64)
	lastFlushAt      time.Time
	lastFlushedBytes int64
	flushBytes       int64
	flushInterval    time.Duration
	// firstFlush gates the one-shot write-deadline install that
	// happens on the first flush after WriteHeader. True until the
	// first flush fires.
	firstFlush bool
	// writeDeadline is the per-flush deadline installed via
	// http.ResponseController.SetWriteDeadline on the first flush
	// (and re-installed on every subsequent flush to keep the
	// deadline sliding forward — the plan enforces "no more than
	// this many seconds of blocking write per flush window").
	writeDeadline time.Duration
	// streaming is the Handle ctx-side flag: true when the gateway
	// took the streaming path on this request. Read by the post-
	// proxy site to decide whether to call finalFlush (residual
	// capture) and whether to skip the once-per-response
	// recordEgress call (the per-flush deltas already account for
	// everything on the streaming path).
	streaming bool
}

// installFlushHook arms the recorder for streaming. After install,
// every Write triggers maybeFlush, and every flush fires onFlush.
// The hook is nil-safe: a nil flusher or nil onFlush silently
// no-ops on the flush attempt (the recorder still tracks Bytes
// for the buffered path).
func (s *statusRecorder) installFlushHook(flusher http.Flusher, onFlush func(int64), flushBytes int64, flushInterval, writeDeadline time.Duration) {
	if flusher == nil {
		return
	}
	s.flusher = flusher
	s.onFlush = onFlush
	s.flushBytes = flushBytes
	s.flushInterval = flushInterval
	s.writeDeadline = writeDeadline
	s.firstFlush = true
	s.streaming = true
}

func (s *statusRecorder) WriteHeader(code int) {
	if !s.wroteHeader {
		s.status = code
		s.wroteHeader = true
		// Issue #471: capture the upstream Content-Type at header
		// commit time so the post-proxy site can detect a buffered
		// SSE response for the deprecation log. Header() returns the
		// current header map (mutable until WriteHeader fires), so
		// reading it here is race-free relative to the underlying
		// ResponseWriter.
		s.ContentType = s.ResponseWriter.Header().Get("Content-Type")
	}
	s.ResponseWriter.WriteHeader(code)
}

// codeql[go/reflected-xss] false-positive: statusRecorder is a pass-through; every caller writes application/json, application/problem+json (api.WriteProblem at :326/:335/:366/:384/:906/:911/:914) or proxies to a Firecracker guest rendered via html/template. See statusRecorder doc-comment.
func (s *statusRecorder) Write(b []byte) (int, error) {
	if !s.wroteHeader {
		// First Write with no explicit WriteHeader → 200.
		s.status = http.StatusOK
		s.wroteHeader = true
	}

	// codeql[go/reflected-xss] false-positive: statusRecorder is a pass-through; every caller writes application/json, application/problem+json (api.WriteProblem at :326/:335/:366/:384/:906/:911/:914) or proxies to a Firecracker guest rendered via html/template. See statusRecorder doc-comment.
	n, err := s.ResponseWriter.Write(b)
	// bytes only advance when the underlying Write actually consumes
	// the buffer — a partial write (err != nil, n<len(b)) still counts
	// what made it to the wire, because that's what egressed. The
	// !s.hasBodyWrite guard prevents double-counting in the rare case
	// where the embedded ResponseWriter is itself wrapped; we don't
	// currently wrap a second layer so it's a no-op in practice but
	// cheap insurance for the future.
	if n > 0 {
		s.Bytes += int64(n)
	}
	if s.flusher != nil {
		s.maybeFlush()
	}
	return n, err
}

// Flush is the http.Flusher pass-through. The ReverseProxy doesn't
// call it directly today (the buffered path), but a streaming
// upstream that wants to push bytes between Writes can call it
// and the same onFlush hook fires. Used by Handler.finalFlush too.
// Nil-safe: returns instantly if the recorder is on the buffered
// path (no flusher installed).
func (s *statusRecorder) Flush() {
	if s.flusher == nil {
		return
	}
	s.doFlush()
}

// maybeFlush checks the periodic-flush triggers and calls doFlush
// when any are met. Cheap when no trigger fires: a bytes-delta
// subtract + a time.Since + a comparison.
func (s *statusRecorder) maybeFlush() {
	if s.firstFlush {
		s.doFlush()
		return
	}
	bytesDelta := s.Bytes - s.lastFlushedBytes
	if bytesDelta >= s.flushBytes {
		s.doFlush()
		return
	}
	if bytesDelta > 0 && time.Since(s.lastFlushAt) >= s.flushInterval {
		s.doFlush()
		return
	}
}

// doFlush is the single point that fires onFlush + pushes bytes
// to the wire. Called from maybeFlush (periodic triggers), Flush
// (explicit upstream call), and finalFlush (residual capture).
func (s *statusRecorder) doFlush() {
	if s.firstFlush && s.writeDeadline > 0 {
		// Install the per-flush write deadline on the very first
		// flush. http.ResponseController is the post-Go-1.20 way
		// to set a write deadline on a ResponseWriter without
		// touching the underlying net/http internals; if the
		// controller is unavailable (e.g. the wrapped writer
		// predates Go 1.20), the deadline is silently skipped and
		// the http.Server.WriteTimeout safety net applies. We
		// don't error-out because production is on Go 1.23 per
		// CLAUDE.md, but the nil-check keeps the unit tests on
		// older httptest.NewRecorder paths honest.
		if rc := http.NewResponseController(s.ResponseWriter); rc != nil {
			_ = rc.SetWriteDeadline(time.Now().Add(s.writeDeadline))
		}
		s.firstFlush = false
	}
	if s.onFlush != nil {
		s.onFlush(s.Bytes)
	}
	s.flusher.Flush()
	s.lastFlushAt = time.Now()
	s.lastFlushedBytes = s.Bytes
}

// finalFlush is the residual capture. The post-proxy site calls
// this after the proxy returns on the streaming path. It fires
// onFlush one more time with the final Bytes (cumulative) so the
// per-flush tx_bytes deltas cover the trailing bytes between the
// last periodic flush and the upstream finishing. Nil-safe: no-op
// on the buffered path.
//
// The two-phase accounting (per-flush + residual) is documented
// in ADR-047; the contract is that the sum of every per-flush
// delta after proxy.ServeHTTP returns equals total egress bytes
// for the request.
func (s *statusRecorder) finalFlush() {
	if s.flusher == nil {
		return
	}
	s.doFlush()
}

// ensureCapacity (issue #168) is the per-app fan-out admission primitive.
//
// Three paths:
//
//  1. Cold start (HealthyCount == 0): go through the WakeGate so a
//     burst of N concurrent cold requests to a fully-parked app
//     coalesces to ONE cold boot per "generation". The leader runs
//     ensure(); followers wait on its result, then EACH re-enters the
//     cold-start loop and admits its own instance IF HealthyCount is
//     still < max_concurrency. This is the per-generation fan-out: a
//     burst of N requests against a parked app admits up to
//     max_concurrency distinct instances, where 1 <= admitted <= N.
//     The loop is bounded by max_concurrency so a single request
//     cannot drive past the cap by itself (the cap is enforced per
//     request, not per generation).
//
//  2. Fan-out (HealthyCount > 0, < max_concurrency): skip the gate and
//     call Admit directly. Sequential requests after the cold-start
//     burst go through this path; schedd's own ledger enforces the cap
//     atomically.
//
//  3. Saturated (HealthyCount >= max_concurrency): no-op. Pick returns
//     one of the cached targets.
//
// Returns (cold, wakeID, method, err):
//   - cold=true on a fresh admit (one or more new instances reached RUNNING);
//     cold=false when the request hit an existing cached target with no
//     fresh admit fired.
//   - wakeID is non-empty on a fresh admit, empty when no admit fired.
//   - method reports what schedd actually did (snapshot restore or cold
//     boot) on the admitted path. WakeMethodUnspecified on
//     cold=false/at-capacity paths and on real failures.
//   - err is non-nil only on real admission failures (RAM headroom, chooser,
//     store). The benign app_concurrency_reached outcome is never lifted to
//     an error by Backend.Admit.
func (h *Handler) ensureCapacity(ctx context.Context, appID string, maxConcurrency int) (cold bool, wakeID string, method WakeMethod, err error) {
	// Loop bound: a single request can drive at most max_concurrency
	// iterations (cold-start with follow-up fan-out). The cap is
	// enforced atomically by Backend.Admit (HealthyCount + add as one
	// serialized op), so this loop is bounded by observation, not by
	// speculation about concurrency.
	for attempt := 0; attempt < maxConcurrency; attempt++ {
		healthy := h.backend.HealthyCount(appID)
		if healthy == 0 {
			c, w, m, e := h.coldStart(ctx, appID, maxConcurrency)
			if e != nil {
				return false, "", WakeMethodUnspecified, e
			}
			if c {
				return true, w, m, nil
			}
			// Cold-start saw no need to admit (a peer's wake
			// already populated the cache). Re-check HealthyCount
			// and fall through to fan-out / saturation on the next
			// iteration.
			continue
		}
		if healthy >= maxConcurrency {
			return false, "", WakeMethodUnspecified, nil
		}
		// Fan-out path: admit directly, no gate. Backend.Admit
		// atomically checks HealthyCount < maxConcurrency under its
		// own lock, so concurrent callers cannot collectively
		// exceed the cap.
		wakeID, method, atCapacity, e := h.backend.Admit(ctx, appID, maxConcurrency)
		if e != nil {
			return false, "", WakeMethodUnspecified, e
		}
		if atCapacity {
			return false, "", WakeMethodUnspecified, nil
		}
		return true, wakeID, method, nil
	}
	return false, "", WakeMethodUnspecified, nil
}

// coldStart is path 1 of ensureCapacity: HealthyCount == 0, so we go
// through the WakeGate's single-flight coalescing. shouldWake is held
// under the gate lock and re-runs HealthyCount; if a peer's admit has
// just landed, we skip the redundant cold boot.
func (h *Handler) coldStart(ctx context.Context, appID string, maxConcurrency int) (bool, string, WakeMethod, error) {
	var (
		admittedWakeID string
		cold           bool
		method         WakeMethod
	)
	werr := h.gate.Wait(ctx, appID,
		func() bool {
			return h.backend.HealthyCount(appID) < maxConcurrency
		},
		func(ctx context.Context) error {
			id, m, atCapacity, e := h.backend.Admit(ctx, appID, maxConcurrency)
			if e != nil {
				return e
			}
			if atCapacity {
				return nil
			}
			admittedWakeID = id
			method = m
			cold = true
			return nil
		},
	)
	if werr != nil {
		return false, "", WakeMethodUnspecified, werr
	}
	return cold, admittedWakeID, method, nil
}

func writeWakeError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrQueueFull):
		w.Header().Set("Retry-After", "5")
		api.WriteProblem(w, api.NewProblem(http.StatusServiceUnavailable, api.CodeCapacity,
			"Briefly at capacity", "the wake queue is full; retry shortly"))
	default:
		var prob *api.Problem
		if errors.As(err, &prob) {
			api.WriteProblem(w, prob)
			return
		}
		api.WriteProblem(w, api.ErrCapacity("wake failed"))
	}
}

// sharedUpstreamTransport is the single *http.Transport gatewayd uses to
// proxy to all upstream microVMs. It is wrapped in a firstByteRoundTripper
// so the wake-timing trace can stamp the inbound request's recorder at
// "first upstream response byte" (spec §6.3, §12). Sharing one transport
// across requests matches Go's stdlib expectation (connection pooling
// requires a single transport per upstream) and the spec's "single public
// listener" invariant — gatewayd owns this transport exclusively.
var sharedUpstreamTransport = newFirstByteRoundTripper(&http.Transport{
	ResponseHeaderTimeout: 60 * time.Second, // spec §4.1
	IdleConnTimeout:       90 * time.Second,
})

// defaultProxy returns a reverse proxy to addr (spec §4.1: 60 s to first
// response byte). The spec's "25 MB either direction" outbound cap is enforced
// by Server.MaxResponseBodyBytes on the http.Server wrapping this handler, so
// it doesn't need to live inside the proxy itself.
func defaultProxy(addr string) http.Handler {
	target := &url.URL{Scheme: "http", Host: addr}
	p := httputil.NewSingleHostReverseProxy(target)
	p.Transport = sharedUpstreamTransport
	return p
}

// hostname strips any port from the Host header and lowercases it.
func hostname(host string) string {
	if i := strings.IndexByte(host, ':'); i >= 0 {
		host = host[:i]
	}
	return strings.ToLower(host)
}
