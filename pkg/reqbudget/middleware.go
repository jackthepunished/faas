// Package reqbudget: middleware.go — the BudgetMiddleware that
// gatewayd-public and apid install at their public listener. The
// middleware stamps a fresh per-request Budget onto r.Context() and
// runs the inner handler under that budget. On deadline fire, the
// middleware intercepts the stdlib context.DeadlineExceeded, writes
// a 504 RFC 7807 problem+json response, and increments the
// exceeded counter against the hop that fired first.
//
// On a clean handler return the middleware records the remaining
// budget at attach time (a stable observation: attached total -
// observed at attach).
package reqbudget

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"
)

// MiddlewareConfig wires BudgetMiddleware. nil Metrics means
// observations are no-ops (tests + path that doesn't have a Prometheus
// registry yet — the gateway build wires metrics in via runDeps).
type MiddlewareConfig struct {
	// Default is the per-request budget installed when no edge-rule
	// kind=budget match overrides it. Must be > 0; validated at
	// construction time.
	Default time.Duration
	// Max is the absolute upper bound on any per-request budget. A
	// kind=budget rule larger than Max is clamped down to Max. Must
	// be >= Default; validated at construction.
	Max time.Duration
	// Route is the coarse route label ("forward", "admin", "invoke")
	// stamped onto every Budget + every metric. The endpoint label
	// comes from the matched route template at runtime; the
	// middleware cannot derive it from r.URL.Path alone (cardinality
	// blow-up risk).
	Route string
	// Endpoint is the per-request endpoint label. The middleware
	// derives this from the matched route after dispatch (see
	// MatchEndpointFunc). When empty, the middleware falls back to
	// "unknown".
	Endpoint string
	// Metrics is the reqbudget.M struct registered against the
	// daemon's Prometheus registry. nil means no-op observations.
	Metrics *M
	// Log is the structured logger; nil falls back to slog.Default().
	Log *slog.Logger
	// Now is the clock used to stamp Budget.Started at attach time.
	// nil → time.Now.
	Now func() time.Time
}

// NewMiddlewareConfig validates cfg and returns it. The constructor
// exists so a misconfigured budget can't be plumbed into a daemon at
// runtime — the validation lives in one place and the daemon boot
// path catches the error.
func NewMiddlewareConfig(cfg MiddlewareConfig) (MiddlewareConfig, error) {
	if cfg.Default <= 0 {
		return MiddlewareConfig{}, errors.New("reqbudget: Default must be > 0")
	}
	if cfg.Max <= 0 {
		cfg.Max = DefaultBudgetMax
	}
	if cfg.Max < cfg.Default {
		return MiddlewareConfig{}, errors.New("reqbudget: Max must be >= Default")
	}
	if cfg.Route == "" {
		return MiddlewareConfig{}, errors.New("reqbudget: Route must be set")
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	if cfg.Endpoint == "" {
		cfg.Endpoint = "unknown"
	}
	return cfg, nil
}

// Middleware returns the http.Handler middleware that wraps next
// with the per-request budget. On deadline fire the middleware
// writes a 504 problem+json response and aborts the inner handler
// chain (the wrapped writer swallows subsequent writes).
//
// Layout: one http.HandlerFunc, three branches:
//
//   - Pre-dispatch: install BudgetMiddleware + start a goroutine
//     that observes the deadline outcome and writes the 504 problem
//     on exceed.
//   - In-handler: standard ServeHTTP, no special handling. The inner
//     handler sees the budget-decorated ctx.
//   - Post-handler: snapshot the final remaining (if any) for the
//     metrics observation.
//
// The middleware never short-circuits a handler that finished
// successfully — it lets the response through and only writes 504
// when r.Context() is done with DeadlineExceeded AND the handler
// hasn't written a response body yet.
func (cfg MiddlewareConfig) Middleware(next http.Handler) http.Handler {
	if cfg.Max == 0 {
		cfg.Max = DefaultBudgetMax
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	if cfg.Endpoint == "" {
		cfg.Endpoint = "unknown"
	}
	if cfg.Route == "" {
		cfg.Route = "unknown"
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Per-request budget: clamp against Max and against any
		// earlier parent deadline (stdlib http.Server attaches
		// ReadTimeout / WriteTimeout on r.Context() at listener
		// dispatch). WithRemaining does both clampings.
		total := cfg.Default
		ctx, cancel, b := WithRemaining(r.Context(), total, cfg.Max, cfg.Route, cfg.Endpoint)
		defer cancel()

		// Wrap the writer so the middleware can tell whether the
		// inner handler wrote a body before r.Context() fired. If
		// not, the middleware writes the 504 problem.
		bw := &budgetWriter{ResponseWriter: w}

		// Set the ctx for the rest of the handler chain. We do this
		// by handing the wrapped ctx into a small ServeHTTP
		// indirection — r is shared with downstream middleware so
		// we cannot mutate r itself.
		next.ServeHTTP(bw, r.WithContext(ctx))

		// Snapshot the outcome.
		switch {
		case bw.wrote:
			// Inner handler wrote a response. We're done; record
			// remaining-at-attach (positive observation) under
			// outcome=set.
			cfg.observe(b, "set")
		case errors.Is(ctx.Err(), context.DeadlineExceeded):
			// Inner handler hit the budget before it could write a
			// response. Write 504 + RFC 7807 envelope if no one
			// else has yet, then record outcome=exceeded.
			cfg.writeProblem(bw, w, b, "exceeded", "request_budget_exceeded")
			cfg.observe(b, "exceeded")
		case errors.Is(ctx.Err(), context.Canceled):
			cfg.observe(b, "cancelled")
		default:
			// Inner handler returned without writing and without a
			// deadline fire (e.g. panic, recovered at the handler).
			// Don't double-write — let the upstream recovery
			// middleware handle it.
			cfg.observe(b, "set")
		}
	})
}

// observe logs and records metrics for a budget outcome. Called
// once per request after the inner handler returns. outcome ∈
// {"set", "exceeded", "cancelled"}.
func (cfg MiddlewareConfig) observe(b Budget, outcome string) {
	if cfg.Metrics == nil || cfg.Metrics.RequestBudgetSeconds == nil {
		return
	}
	remaining := b.Remaining(time.Time{})
	// For exceeded/cancelled the remaining is ~0; for set it's the
	// leftover budget at handler completion. Histogram observation
	// is bounded by [0, b.Total] so we clamp.
	if remaining < 0 {
		remaining = 0
	}
	if remaining > b.Total {
		remaining = b.Total
	}
	cfg.Metrics.RequestBudgetSeconds.
		WithLabelValues(b.Route, b.Endpoint, outcome).
		Observe(remaining.Seconds())
}

// writeProblem writes a 504 RFC 7807 envelope when the inner
// handler hasn't yet committed a response. The middleware uses a
// fixed code (`request_budget_exceeded`) and a fixed docs URL —
// see ADR-093 for the docs location.
func (cfg MiddlewareConfig) writeProblem(bw *budgetWriter, w http.ResponseWriter, b Budget, outcome, code string) {
	if bw.wrote {
		return
	}
	// Hop name is the last entry on the audit trail, if any — that
	// names which downstream exceeded first.
	hop := "gateway"
	if len(b.Overheads) > 0 {
		hop = b.Overheads[len(b.Overheads)-1].Name
	}
	if cfg.Metrics != nil && cfg.Metrics.RequestBudgetExceededTotal != nil {
		cfg.Metrics.RequestBudgetExceededTotal.
			WithLabelValues(b.Route, b.Endpoint, hop).
			Inc()
	}
	cfg.Log.Info("budget_exceeded",
		"code", code,
		"route", b.Route,
		"endpoint", b.Endpoint,
		"budget_ms", b.Total.Milliseconds(),
		"hop", hop,
	)
	// Direct write to the wrapped w (not bw) so the budgetWriter's
	// wrote flag isn't double-tripped — bw is only consulted
	// post-handler.
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(http.StatusGatewayTimeout)
	_, _ = w.Write([]byte(`{"type":"about:blank","title":"request budget exceeded","status":504,"code":"request_budget_exceeded","limit":"` +
		b.Total.String() + `","docs_url":"https://docs.gregale.dev/errors/request-budget-exceeded"}`))
}

// budgetWriter is a tiny http.ResponseWriter wrapper that tracks
// whether the inner handler committed a response body. If not, the
// middleware writes its own 504 problem on top.
//
// The wrapper deliberately does NOT implement http.Flusher /
// http.Hijacker — those are power-user escapes the middleware is
// not in the path for today; the streaming-forwarder is one layer
// down where the per-flush write deadline is enforced separately.
type budgetWriter struct {
	http.ResponseWriter
	wrote bool
}

func (b *budgetWriter) WriteHeader(code int) {
	if b.wrote {
		return
	}
	b.wrote = true
	b.ResponseWriter.WriteHeader(code)
}

func (b *budgetWriter) Write(p []byte) (int, error) {
	if !b.wrote {
		b.wrote = true
	}
	return b.ResponseWriter.Write(p)
}
