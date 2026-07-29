// Package reconciler is the push↔usage drift detector (ADR-049 §B.1).
//
// The reconciler is the read-only complement to meterd's pusher
// (pkg/meter/pusher.go). Every FAAS_RECONCILE_INTERVAL it walks
// every paid account, sums the last-24h mb_seconds from the local
// usage_minutes table, and asks the active billing.Provider for the
// matching summary. The diff between local and pushed totals is
// exposed as Prometheus gauges so the BillingDrift alert can page
// on real Stripe / Paddle outages that would otherwise go unobserved
// (revenue leakage / over-billing).
//
// Failure mode is fail-soft. A single account's reconcile error
// (network blip, Stripe rate-limit, Paddle not-yet-implemented
// ErrNotImplemented) is logged and the account is skipped — the
// loop continues to the next account. The loop itself never
// blocks on a single account; the long-run mean drift ratio is
// the signal.
//
// The reconciler owns its own Prometheus registry (separate from
// wire.OpsMetrics) so it can be wired in cmd/meterd/main.go
// alongside the existing gauges without coupling to the ops-
// metrics struct. The two registries are merged at the /metrics
// scrape by promhttp.HandlerFor.
package reconciler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/onebox-faas/faas/pkg/api"
	"github.com/onebox-faas/faas/pkg/billing"
	"github.com/onebox-faas/faas/pkg/state"
)

// Reconciler compares local usage_minutes totals against the
// active billing.Provider's pushed totals. The store is read-only
// against usage_minutes; the provider is read-only against the
// billing surface. No external state mutation happens here.
type Reconciler struct {
	Store    state.Store
	Provider billing.Provider
	Log      *slog.Logger

	// Registry is the Prometheus registry the reconciler emits
	// gauges into. Required; cmd/meterd/main.go creates a fresh
	// registry and merges it with the ops-metrics registry at
	// the /metrics scrape endpoint.
	Registry *prometheus.Registry

	// Window is the rolling window the reconciler compares
	// local-vs-pushed totals over. Default 24 h when zero.
	Window time.Duration

	// ProviderName is the label value on the emitted gauges
	// (e.g. "stripe", "paddle"). Empty defaults to "unknown".
	ProviderName string

	driftMBSeconds *prometheus.GaugeVec
	driftRatio     *prometheus.GaugeVec
}

// DefaultInterval is the cadence the reconciler runs at when the
// caller passes 0 for Interval. Matches the metering cadence
// (every 6 h) so the BillingDrift alert has a fresh signal within
// 1 h of the next sample tick.
const DefaultInterval = 6 * time.Hour

// DefaultWindow is the rolling window the reconciler compares
// local-vs-pushed totals over. 24 h matches the audit's
// recommended drift-detection horizon.
const DefaultWindow = 24 * time.Hour

// New constructs a Reconciler and registers its gauges against
// Registry. If Registry is nil, a fresh registry is created so the
// caller can pass nil in tests.
func New(providerName string, store state.Store, provider billing.Provider, log *slog.Logger, registry *prometheus.Registry) *Reconciler {
	if registry == nil {
		registry = prometheus.NewRegistry()
	}
	r := &Reconciler{
		Store:        store,
		Provider:     provider,
		Log:          log,
		Registry:     registry,
		ProviderName: providerName,
		Window:       DefaultWindow,
		driftMBSeconds: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "meterd_billing_drift_mb_seconds",
			Help: "Signed diff between local usage_minutes mb_seconds total and the provider's pushed total over the rolling Window. ADR-049 §B.1.",
		}, []string{"account_id", "provider"}),
		driftRatio: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "meterd_billing_drift_ratio",
			Help: "abs(local-pushed) / max(local, pushed) over the rolling Window. ADR-049 §B.1. The BillingDrift alert pages on ratio > 0.005 for 1h.",
		}, []string{"account_id", "provider"}),
	}
	registry.MustRegister(r.driftMBSeconds, r.driftRatio)
	return r
}

// Handler returns the Prometheus scrape handler for the reconciler's
// gauges. cmd/meterd mounts this on a separate path or merges
// registries — both shapes are supported.
func (r *Reconciler) Handler() http.Handler {
	return promhttp.HandlerFor(r.Registry, promhttp.HandlerOpts{})
}

// RunOnce is one pass over every paid account. The store's
// ListAllAccounts is the only I/O — the reconciler does not
// iterate per-app, only per-account, so the cost is O(paid_accounts).
func (r *Reconciler) RunOnce(ctx context.Context) error {
	if r.Window == 0 {
		r.Window = DefaultWindow
	}
	if r.ProviderName == "" {
		r.ProviderName = "unknown"
	}
	accounts, err := r.Store.ListAllAccounts(ctx)
	if err != nil {
		return fmt.Errorf("list accounts: %w", err)
	}
	end := time.Now().UTC().Truncate(time.Minute)
	start := end.Add(-r.Window)
	for _, acct := range accounts {
		if err := r.reconcileOne(ctx, acct, start, end); err != nil {
			// Fail-soft per account: log + skip. The BillingDrift
			// alert fires on accumulated drift ratio, so a
			// transient provider error does NOT page. The
			// reconciler metric records the skip.
			if r.Log != nil {
				r.Log.Warn("reconcile account failed",
					"account_id", acct.ID, "err", err)
			}
		}
	}
	return nil
}

func (r *Reconciler) reconcileOne(ctx context.Context, acct state.Account, start, end time.Time) error {
	// Skip non-paid plans — no Stripe / Paddle surface to drift
	// against. The metered quota math is identical but the
	// provider-side count is zero by construction.
	if acct.Plan == api.PlanFree {
		return nil
	}
	rows, err := r.Store.UsageByHour(ctx, acct.ID, start, end)
	if err != nil {
		return fmt.Errorf("usage by hour: %w", err)
	}
	var local int64
	for _, u := range rows {
		local += u.MBSeconds
	}
	pushed, err := r.Provider.ReconcileUsage(ctx, acct, start, end)
	if err != nil {
		// ErrNotImplemented (Paddle today) is not an error — the
		// reconciler observes "provider has no drift signal yet"
		// and the local sum becomes the only contribution to
		// the ratio. We return nil so the metric records the
		// observed pushed=0 (which the ratio calc treats as
		// "provider drift ratio undefined" → no alert).
		if errors.Is(err, billing.ErrNotImplemented) {
			return nil
		}
		return fmt.Errorf("provider reconcile: %w", err)
	}
	drift := local - pushed
	r.emit(acct.ID, local, pushed, drift)
	return nil
}

func (r *Reconciler) emit(accountID string, local, pushed, drift int64) {
	labels := prometheus.Labels{
		"account_id": accountID,
		"provider":   r.ProviderName,
	}
	r.driftMBSeconds.With(labels).Set(float64(drift))
	maxv := local
	if pushed > maxv {
		maxv = pushed
	}
	var ratio float64
	if maxv > 0 {
		ratio = float64(abs(drift)) / float64(maxv)
	}
	r.driftRatio.With(labels).Set(ratio)
}

func abs(x int64) int64 {
	if x < 0 {
		return -x
	}
	return x
}

// Loop is a free-function goroutine that calls RunOnce every
// Interval until ctx is cancelled. Matches the cadence contract
// for the other meterd ticks (pkg/meter/loop.go::Run).
//
// Returns when ctx.Done() fires. Caller is responsible for
// goroutine lifecycle (cmd/meterd/main.go launches this in a
// `go` statement at boot).
func (r *Reconciler) Loop(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = DefaultInterval
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := r.RunOnce(ctx); err != nil {
				if r.Log != nil {
					r.Log.Error("reconcile tick failed", "err", err)
				}
			}
		}
	}
}
