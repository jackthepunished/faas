// cmd/meterd/api_reachability_sweep.go — ADR-123 alert-preset
// signal-feeding goroutine for the `api_down` preset (issue #1233,
// PR-B). Mirrors the existing CertExpiryRefresherLoop /
// AccountSpendAggregatorLoop pattern in alert_presets_ticks.go
// (same package, free-function goroutine, ctx-cancelled, nil-coerced
// log + ops).
//
// The api_up alert preset has historically been computed inline by
// pkg/alerts/evaluator.go via state.WasInvokedSuccessfullySince on a
// per-rule basis. PR-B adds the operator-visible Prometheus mirror
// meterd_api_reachable{account_id, app_id} = 1.0 if the app served
// a successful invocation in the last 5 minutes, 0.0 otherwise. The
// evaluator keeps reading its inline path; the gauge is purely the
// "is the signal actually flowing" observability mirror — without
// it, an alert rule that stops firing on a real outage looks
// identical to one whose metric series is silently absent.
//
// Walk pattern:
//  1. ListAllAccounts → for each account, ListApps(accountID)
//  2. For each (accountID, app.ID), call
//     state.WasInvokedSuccessfullySince(ctx, accountID, appID, now-5min)
//  3. Stamp gauge.WithLabelValues(accountID, appID).Set(1.0|0.0)
//
// Cardinality: bounded by per-plan app count (Hobby=5, Pro=25,
// Scale=100). A 1000-account Scale fleet produces ~100k series
// worst case, well under Prometheus's per-target series ceiling.
//
// Memory model: no per-pair state is held across ticks; the gauge
// label set is recreated on every sweep (Prometheus handles
// Add-on-existing-label as a no-op), and a new app that surfaces
// between ticks starts stamping from its next sweep.
package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/onebox-faas/faas/pkg/meter"
	"github.com/onebox-faas/faas/pkg/state"
	"github.com/onebox-faas/faas/pkg/wire"
)

// APIReachabilitySweepParams is the param bundle for
// APIReachabilitySweepLoop. Store is required; Log and Ops are
// nil-coerced to the package-level defaults.
//
// ReachWindow is the look-back window for the
// WasInvokedSuccessfullySince EXISTS scan. Zero falls back to the
// gauge's natural reachability window (5 min, the same window the
// gauge encodes). Caller-supplied windows smaller than Interval
// risk the gauge flipping 1.0 → 0.0 within the same sweep; caller-
// supplied windows larger than Interval risk the gauge lagging
// actual reachability. The default is the documented "alert preset
// api_down" contract.
type APIReachabilitySweepParams struct {
	Store       state.Store
	Log         *slog.Logger
	Ops         *wire.OpsMetrics
	Interval    time.Duration
	ReachWindow time.Duration
	Now         func() time.Time // injectable for tests; nil falls back to time.Now
}

// APIReachabilitySweepLoop walks every (account_id, app_id) pair
// every Interval, calls
// state.WasInvokedSuccessfullySince(ctx, accountID, appID, now-ReachWindow)
// for each, and stamps meterd_api_reachable{account_id, app_id} to
// 1.0 (recent successful invocation) or 0.0 (none in the window).
// The first tick runs immediately so a freshly-restarted meterd
// stamps the gauge without waiting Interval.
//
// Pair walk is best-effort: a transient ListAllAccounts or ListApps
// error logs and skips the rest of the walk — the gauge keeps its
// previous values until the next tick. A persistent failure shows
// up as a stale "1.0" gauge that the alert evaluator's
// degraded-source branch (mirroring pkg/alerts/evaluator.go:505)
// catches.
//
// Returns when ctx is cancelled.
func APIReachabilitySweepLoop(ctx context.Context, p APIReachabilitySweepParams) {
	if p.Store == nil {
		return
	}
	if p.Interval <= 0 {
		p.Interval = meter.DefaultAPIReachabilitySweepInterval
	}
	if p.ReachWindow <= 0 {
		p.ReachWindow = 5 * time.Minute
	}
	if p.Log == nil {
		p.Log = slog.Default()
	}
	if p.Ops == nil {
		p.Ops = wire.NewOpsMetrics("meterd")
	}
	nowFn := p.Now
	if nowFn == nil {
		nowFn = time.Now
	}

	do := func() {
		walkCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
		defer cancel()
		accounts, err := p.Store.ListAllAccounts(walkCtx)
		if err != nil {
			p.Log.Warn("meterd: list accounts (api reachability sweep) failed", "err", err)
			return
		}
		gauge := p.Ops.MeterdAPIReachable()
		if gauge == nil {
			return
		}
		now := nowFn()
		since := now.Add(-p.ReachWindow)
		stamped := 0
		for _, a := range accounts {
			apps, listErr := p.Store.ListApps(walkCtx, a.ID)
			if listErr != nil {
				p.Log.Warn("meterd: list apps (api reachability sweep) failed", "account_id", a.ID, "err", listErr)
				continue
			}
			for _, app := range apps {
				ok, invErr := p.Store.WasInvokedSuccessfullySince(walkCtx, a.ID, app.ID, since)
				if invErr != nil {
					p.Log.Warn("meterd: was-invoked-successfully-since failed", "account_id", a.ID, "app_id", app.ID, "err", invErr)
					continue
				}
				if ok {
					gauge.WithLabelValues(a.ID, app.ID).Set(1.0)
				} else {
					gauge.WithLabelValues(a.ID, app.ID).Set(0.0)
				}
				stamped++
			}
		}
		p.Log.Info("meterd: api reachability sweep tick ok", "accounts", len(accounts), "stamped", stamped)
	}
	do()
	t := time.NewTicker(p.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			do()
		}
	}
}
