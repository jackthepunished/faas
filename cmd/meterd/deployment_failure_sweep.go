// cmd/meterd/deployment_failure_sweep.go — ADR-123 alert-preset
// signal-feeding goroutine for the `deploy_failed` preset (issue
// #1233, PR-B). Mirrors the existing CertExpiryRefresherLoop /
// AccountSpendAggregatorLoop pattern in alert_presets_ticks.go.
//
// PR-B adds the operator-visible Prometheus mirror
// apid_deployment_failed_total{account_id, app_id} = delta counter
// of deployments whose status transitioned to 'failed' since the
// previous sweep. The evaluator at pkg/alerts/evaluator.go keeps
// reading CountFailedDeploymentsSince inline; this counter is the
// observability mirror that surfaces a stuck/dead deploy pipeline
// on /metrics without scraping the alert rule set.
//
// Walk pattern:
//  1. Maintain lastSweepTime across ticks (boot seeds it to
//     now()-Interval so the first sweep captures the most recent
//     window; on restart the over-count is bounded to one
//     Interval of failures).
//  2. ListAllAccounts → for each account, ListApps(accountID)
//  3. For each (accountID, app.ID), call
//     state.CountFailedDeploymentsSince(ctx, accountID, appID, lastSweepTime)
//  4. counter.WithLabelValues(accountID, appID).Add(count)
//  5. lastSweepTime = now
//
// Cardinality: bounded by per-plan app count (Hobby=5, Pro=25,
// Scale=100). Same envelope as api_reachable_sweep.
//
// Why a delta-counter not a cumulative gauge: the alert preset's
// evaluation window (15 min default per the catalog seed) rolls
// the counter back through rate(); a cumulative gauge would need
// rate() + the restart reset documented as a known operator
// caveat, which the delta shape avoids at the cost of the bounded
// over-count on restart.
package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/onebox-faas/faas/pkg/meter"
	"github.com/onebox-faas/faas/pkg/state"
	"github.com/onebox-faas/faas/pkg/wire"
)

// DeploymentFailureSweepParams is the param bundle for
// DeploymentFailureSweepLoop. Store is required; Log and Ops are
// nil-coerced to the package-level defaults.
type DeploymentFailureSweepParams struct {
	Store    state.Store
	Log      *slog.Logger
	Ops      *wire.OpsMetrics
	Interval time.Duration
	Now      func() time.Time // injectable for tests; nil falls back to time.Now
}

// DeploymentFailureSweepLoop walks every (account_id, app_id) pair
// every Interval, queries
// state.CountFailedDeploymentsSince(ctx, accountID, appID, lastSweepTime),
// and increments apid_deployment_failed_total{account_id, app_id}
// by the delta. lastSweepTime is closed-over across ticks so the
// delta contract holds across the loop lifetime.
//
// Boot behaviour: lastSweepTime is initialised to
// nowFn().Add(-Interval), so the first sweep captures any
// failures in the trailing window. On restart, the same seed
// applies — the over-count is bounded to one Interval of failures
// (60 s default). This is the documented trade-off in the
// ADR-123 PR-B risk register; the alert preset evaluates the
// counter via rate(), which dampens the one-shot over-count.
//
// Returns when ctx is cancelled.
func DeploymentFailureSweepLoop(ctx context.Context, p DeploymentFailureSweepParams) {
	if p.Store == nil {
		return
	}
	if p.Interval <= 0 {
		p.Interval = meter.DefaultDeploymentFailureSweepInterval
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

	// Seed lastSweepTime to "one interval ago" so the first sweep
	// captures any failures in the trailing window. See the
	// package comment for the over-count trade-off on restart.
	lastSweepTime := nowFn().Add(-p.Interval)

	do := func() {
		walkCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
		defer cancel()
		accounts, err := p.Store.ListAllAccounts(walkCtx)
		if err != nil {
			p.Log.Warn("meterd: list accounts (deployment failure sweep) failed", "err", err)
			return
		}
		counter := p.Ops.ApidDeploymentFailedTotal()
		if counter == nil {
			return
		}
		now := nowFn()
		totalDelta := 0
		for _, a := range accounts {
			apps, listErr := p.Store.ListApps(walkCtx, a.ID)
			if listErr != nil {
				p.Log.Warn("meterd: list apps (deployment failure sweep) failed", "account_id", a.ID, "err", listErr)
				continue
			}
			for _, app := range apps {
				n, countErr := p.Store.CountFailedDeploymentsSince(walkCtx, a.ID, app.ID, lastSweepTime)
				if countErr != nil {
					p.Log.Warn("meterd: count failed deployments since failed", "account_id", a.ID, "app_id", app.ID, "since", lastSweepTime, "err", countErr)
					continue
				}
				if n > 0 {
					counter.WithLabelValues(a.ID, app.ID).Add(float64(n))
				}
				totalDelta += n
			}
		}
		lastSweepTime = now
		p.Log.Info("meterd: deployment failure sweep tick ok", "accounts", len(accounts), "delta", totalDelta)
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
