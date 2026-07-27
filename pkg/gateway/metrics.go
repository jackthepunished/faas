// Prometheus instrumentation for gatewayd (spec §4.1, §12). The metric names
// here are dashboard dependencies — DO NOT rename without coordinating with
// the dashboards in deploy/grafana/. We register on a per-Handler registry
// (not the global default) so concurrent tests don't collide.
//
// Emitted series:
//   - gateway_requests_total{app, plan, code}        counter
//   - gateway_wake_latency_seconds                    histogram
//   - gateway_wake_queue_wait_seconds                 histogram (M8 §12 dashboard)
//   - gateway_queue_depth{app}                       gauge (set/cleared by
//     WakeGate.SetGaugeSink)
//   - gateway_rate_limited_total{app, plan}          counter
//   - gateway_cold_wake_total{app}                   counter
//   - gateway_tls_cert_expiry_seconds                gauge (ADR-024 H3, closed
//     in PR #345; refreshed every 5 min by StartCertExpiryRefresher; smallest
//     remaining lifetime across cached certs on disk; negative when a cert
//     is already expired — the page rule fires regardless)
//   - gateway_tls_on_demand_denied_total{reason}     counter (ADR-024 H3, closed
//     in PR #345; reason ∈ {allowlist, dns01, token} — only allowlist is wired
//     today; dns01 + token pre-instantiate at 0 so the panel surfaces from
//     boot and a missing wire-incrementation is visible as a frozen zero.
//     ADR-024 H3.b is the still-open follow-up to bridge the certmagic zap
//     logger into this counter)
package gateway

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/onebox-faas/faas/pkg/logsanitize"
)

// Metrics is the gatewayd Prometheus bundle. Construct once per Handler via
// NewMetrics and pass into NewHandlerWith.
type Metrics struct {
	registry *prometheus.Registry

	requests      *prometheus.CounterVec
	wakeLatency   prometheus.Histogram
	wakeQueueWait prometheus.Histogram
	queueDepth    *prometheus.GaugeVec
	rateLimited   *prometheus.CounterVec
	// accountRateLimited backs the per-account throttling introduced by
	// ADR-040 (issue #292). Labels: account_id, plan. Pre-instantiates
	// the four plan rows under the `__other__` placeholder so the §12
	// dashboard panel never shows "no data" before the first 429.
	accountRateLimited *prometheus.CounterVec
	coldWake           *prometheus.CounterVec
	// ADR-024 H3 (closed in PR #345): TLS observability closures. The
	// counter is incremented from pkg/gateway/tls_wire.go's
	// allowlistToDecisionFunc on a denied mint (today only the
	// allowlist branch is wired; the dns01 + token branches
	// pre-instantiate at 0 and gain their wire-incrementation in the
	// ADR-024 H3.b follow-up). The gauge is refreshed every 5 min by
	// StartCertExpiryRefresher (see pkg/gateway/cert_expiry.go) — it
	// reports the smallest remaining lifetime across cached certs on
	// disk so the §12 panels can surface both "expires soon" (warn at
	// 30 d) and "about to expire" (page at 14 d) without a per-host
	// fan-out. A negative value means a cert on disk is already past
	// its NotAfter; the page rule fires regardless.
	tlsCertExpiry     prometheus.Gauge
	tlsOnDemandDenied *prometheus.CounterVec
}

func NewMetrics() *Metrics {
	reg := prometheus.NewRegistry()
	m := &Metrics{
		registry: reg,
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gateway_requests_total",
			Help: "Total gateway requests, labelled by app, plan, and HTTP status class.",
		}, []string{"app", "plan", "code"}),
		wakeLatency: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name: "gateway_wake_latency_seconds",
			Help: "End-to-end latency from request received to first upstream byte after a cold wake.",
			// Buckets target the §12 SLO: p50 ≤ 0.35 s, p95 ≤ 0.8 s, page > 1.5 s.
			Buckets: []float64{
				0.05, 0.1, 0.2, 0.3, 0.35, 0.5, 0.8, 1.0, 1.5, 3.0, 5.0, 10.0,
			},
		}),
		// Spec §12 row "wake queue wait p95". Observed by WakeGate.Wait
		// on every caller that joins a single-flight coalescing wake. The
		// leader (the request that actually triggers the wake) reads near
		// zero; followers (peer requests parked while the leader's restore
		// runs) read close to the restore latency.
		//
		// Buckets skew toward the wake-completion window (50ms..2s) so
		// the histogram exposes the p50/p95 cleanly; the long tail
		// (5s, 10s) catches pathological stalls where the gate's 30s TTL
		// is approaching.
		wakeQueueWait: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name: "gateway_wake_queue_wait_seconds",
			Help: "Time spent in the per-app wake queue (single-flight coalescing) before the request was released to upstream. Spec §12 row 'wake queue wait p95'.",
			Buckets: []float64{
				0.005, 0.01, 0.025, 0.05, 0.1, 0.2, 0.35, 0.5, 1.0, 2.0, 5.0, 10.0, 30.0,
			},
		}),
		queueDepth: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "gateway_queue_depth",
			Help: "Current number of waiters per app's wake queue (sampled).",
		}, []string{"app"}),
		rateLimited: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gateway_rate_limited_total",
			Help: "Requests rejected by the per-app rate limiter.",
		}, []string{"app", "plan"}),
		// ADR-040 / issue #292. account_id label has cardinality O(active
		// accounts × 4 plans). The bounded admission lives in the
		// alert + runbook audit path (max ~10k customers on the one-box
		// box today); raw labels are surfaced only on first 429 to avoid
		// pre-instantiating 40k zero-valued series. Pre-instantiation
		// only touches the closed (plan) set under the "__other__"
		// placeholder so the §12 panel surfaces from boot.
		accountRateLimited: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gateway_per_account_rate_limited_total",
			Help: "Requests rejected by the per-account rate limiter (ADR-040 / issue #292). Labelled by account_id, plan. account_id=\"__other__\" is the bounded admission overflow placeholder for the closed (plan) set.",
		}, []string{"account_id", "plan"}),
		coldWake: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gateway_cold_wake_total",
			Help: "Requests that triggered a cold wake for an app.",
		}, []string{"app"}),
		// ADR-024 H3 (closed in PR #345). Gauge starts unset (NaN at
		// scrape time — Prometheus drops NaN series, so an idle daemon
		// emits no series at all); the page rule's `<` then returns
		// false and the alert stays silent until a real cert has been
		// minted. SetTLSCertExpiry may emit a negative value when a
		// cert on disk is past its NotAfter — that's intentional, the
		// page rule fires regardless.
		tlsCertExpiry: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "gateway_tls_cert_expiry_seconds",
			Help: "Smallest remaining lifetime across cached certs on disk (cfg.StorageDir). ADR-024 H3 (closed). Page at ≤14 d; warn at ≤30 d. Gauge is unset (no series) before the first cert is minted; the `<` alert expression handles a missing series correctly. A negative value means a cert on disk is already past its NotAfter — the page rule fires regardless.",
		}),
		// ADR-024 H3 (closed in PR #345). The reason label set is closed
		// and pre-instantiated below so every reason series surfaces in
		// /metrics from boot. Only `allowlist` is wired today (from the
		// on-demand DecisionFunc); dns01 + token gain their wire-
		// incrementation in the still-open H3.b follow-up. The frozen-
		// zero is the visibility for that follow-up — operators see
		// the panel exist and a stuck-at-zero signals the follow-up
		// is unmerged.
		tlsOnDemandDenied: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gateway_tls_on_demand_denied_total",
			Help: "On-demand cert mint denials, labelled by reason. ADR-024 H3 (closed in PR #345); H3.b is the still-open follow-up. reason=allowlist is incremented from pkg/gateway/tls_wire.go's allowlistToDecisionFunc today. reason=dns01 and reason=token are reserved for the H3.b follow-up that bridges the certmagic ACME-issuer logger through this counter; the series are pre-instantiated at 0 so the dashboard panel surfaces from boot and a missing wire-incrementation is visible as a frozen zero.",
		}, []string{"reason"}),
	}
	// Pre-instantiate every closed (reason) label tuple so the counter's
	// HELP/TYPE and zero-valued series surface in /metrics from the moment
	// the daemon binds — same precedent as auditWriteFail / requestFailures
	// pre-instantiation above and the egress-deny / scale-decisions catalog
	// pre-instantiation in pkg/wire/metrics.go. Without this loop, the
	// `reason="dns01"` / `reason="token"` rows would only appear after
	// the first denial, hiding the "frozen zero = follow-up unmerged"
	// signal we depend on for the §12 dashboard panel. NewMetrics is
	// called exactly once per daemon (cmd/gatewayd/main.go:269), so each
	// daemon gets exactly one set of pre-instantiated series; if you
	// ever construct a second *Metrics, that's by design, not a bug.
	for _, reason := range []string{"allowlist", "dns01", "token"} {
		m.tlsOnDemandDenied.WithLabelValues(reason)
	}
	// ADR-040 / issue #292. Pre-instantiate the closed (plan) row set
	// under the "__other__" placeholder so the §12 dashboard panel
	// surfaces a zero-valued series from boot. Real account_id rows
	// appear on first 429 — bounded admission is the alert + runbook
	// concern, not the limiter's.
	for _, plan := range []string{"free", "hobby", "pro", "scale"} {
		m.accountRateLimited.WithLabelValues("__other__", plan)
	}
	reg.MustRegister(m.requests, m.wakeLatency, m.wakeQueueWait, m.queueDepth, m.rateLimited, m.accountRateLimited, m.coldWake, m.tlsCertExpiry, m.tlsOnDemandDenied)
	return m
}

// Registry returns the underlying *prometheus.Registry — pass to promhttp.
func (m *Metrics) Registry() *prometheus.Registry { return m.registry }

// Handler returns an http.Handler that serves the registry's metrics in the
// Prometheus text exposition format. Mount at /metrics on the control listener.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{Registry: m.registry})
}

// ObserveRequest records a completed request's outcome. code is the HTTP
// status class as a 3-digit string ("200", "404", "503"...).
func (m *Metrics) ObserveRequest(appID, plan, code string) {
	m.requests.WithLabelValues(appID, plan, code).Inc()
}

// ObserveRateLimit records a 429 outcome.
func (m *Metrics) ObserveRateLimit(appID, plan string) {
	m.rateLimited.WithLabelValues(appID, plan).Inc()
}

// ObserveAccountRateLimit records a 429 outcome from the per-account
// limiter (ADR-040 / issue #292). Nil-receiver-safe — mirrors the
// ObserveWakeQueueWait / ObserveTLSOnDemandDenied pattern, unlike
// ObserveRateLimit (which the call site guards with `if h.metrics != nil`).
// Per-account 429s are the dashboard's primary abuse signal so the
// call site shouldn't have to remember the nil guard.
func (m *Metrics) ObserveAccountRateLimit(accountID, plan string) {
	if m == nil {
		return
	}
	m.accountRateLimited.WithLabelValues(accountID, plan).Inc()
}

// ObserveColdWake records that this request caused a cold wake and observes
// the wake latency (request-received to first upstream byte).
func (m *Metrics) ObserveColdWake(appID string, latency time.Duration) {
	m.coldWake.WithLabelValues(appID).Inc()
	m.wakeLatency.Observe(latency.Seconds())
}

// ObserveWakeQueueWait records how long a request waited in the
// per-app wake queue before the gate released it (single-flight
// coalescing). Nil-safe so WakeGate can call it without branching.
func (m *Metrics) ObserveWakeQueueWait(d time.Duration) {
	if m == nil {
		return
	}
	m.wakeQueueWait.Observe(d.Seconds())
}

// SetQueueDepth records the current wake-queue depth for an app.
func (m *Metrics) SetQueueDepth(appID string, depth int) {
	m.queueDepth.WithLabelValues(appID).Set(float64(depth))
}

// ObserveTLSOnDemandDenied increments the per-reason counter that backs
// gateway_tls_on_demand_denied_total (ADR-024 H3). reason ∈ {allowlist,
// dns01, token}; unknown reasons fall through to the
// prometheus.NewCounterVec default behaviour (a new labelled series
// surfaces in /metrics but the operator panel never queries for them).
// Called from pkg/gateway/tls_wire.go's allowlistToDecisionFunc — today
// only with reason="allowlist"; the dns01 + token branches are wired in
// the H3.b follow-up that bridges certmagic's ACME-issuer logger. Safe
// on a nil receiver so callers running outside the daemon (tests with a
// stub Metrics) don't need a nil-check at every call site; matches the
// ObserveBuildCount / SetResidentGBPerCustomer nil-safe precedent.
func (m *Metrics) ObserveTLSOnDemandDenied(reason string) {
	if m == nil {
		return
	}
	m.tlsOnDemandDenied.WithLabelValues(reason).Inc()
}

// SetTLSCertExpiry writes the smallest remaining lifetime across cached
// certs on disk to the gateway_tls_cert_expiry_seconds gauge (ADR-024
// H3, closed in PR #345). d is the time delta to the soonest-expiring
// cert — positive when at least one cert is on disk and unexpired,
// negative when a cert is already past its NotAfter (the page rule
// fires regardless of sign). Callers must NOT touch the gauge when
// there are no certs; the prometheus.Gauge default is "no series",
// and Prometheus's `<` comparator against a missing series returns
// false (so the alert is silent pre-first-mint). Refreshed every 5 min
// by StartCertExpiryRefresher (see pkg/gateway/cert_expiry.go). Safe
// on a nil receiver.
func (m *Metrics) SetTLSCertExpiry(d time.Duration) {
	if m == nil {
		return
	}
	m.tlsCertExpiry.Set(d.Seconds())
}

// requestLogger is a one-line structured slog request logger used by Handler.
// Built as a type so tests can replace WithLogger.
type requestLogger struct{ log *slog.Logger }

func (l *requestLogger) Log(appID, code string, latency time.Duration, cold bool, requestID string) {
	if l == nil || l.log == nil {
		return
	}
	// requestID flows from the x-faas-request-id HTTP header (pkg/gateway/observability.go:requestIDFrom)
	// and is therefore attacker-controllable. Strip CR/LF/NUL/DEL before logging so a forged
	// header cannot smuggle a new log line into the stream. appID and code are server-generated
	// (UUIDs / HTTP status class digit) and need no sanitization.
	//
	// codeql[go/log-injection] false-positive: logsanitize.Field is not in CodeQL's sanitizer model
	// (the query only recognizes inline strings.ReplaceAll), but it does strip the injection bytes
	// at runtime — matching the defense-in-depth precedent set for the synth RPC (47d5531).
	l.log.Info("gateway_request",
		"app_id", appID,
		"code", code,
		"latency_ms", latency.Milliseconds(),
		"cold", cold,
		"request_id", logsanitize.Field(requestID),
	)
}
