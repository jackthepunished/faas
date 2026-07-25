// PR-E egress-deny counter poll adapter.
//
// This file owns the loop that reads the nftables named counters
// (`nft -j list counters`) and emits the per-counter delta to the
// vmmd Prometheus registry as <daemon>_egress_deny_total{cidr,family}.
//
// Design notes (see plan: docs/faas_implementation_spec.md §11 + §12 + ADR-034):
//
//   - The poll cadence is 15s — matches the typical Prometheus scrape
//     interval (and the operator's mental "is there a drop storm?"
//     question). A tighter cadence wastes exec.CommandContext calls;
//     a looser cadence delays the alert.
//
//   - The PopCounters dependency is a function-typed seam so the
//     unit test in cmd/vmmd/main_test.go can swap it for a stub
//     without shelling out to nft. The default is
//     netns.PopCounters; non-metal builds get a stub that returns
//     an empty map, so unit tests run on a vanilla dev box.
//
//   - The last-seen map is local to the goroutine and never
//     escapes. The first poll populates lastSeen without emitting
//     (so we don't surface a one-time "0 → 5" deluge when vmmd
//     starts up against a host that's been dropping packets for
//     days). Subsequent polls emit (curr - lastSeen[name]).
//
//   - Poll failures are logged at Warn and skipped — the metric
//     stays at the last-known value, which is the right behaviour
//     for an observability hook (the deny rules themselves still
//     fire; only the *measurement* pauses). The next successful
//     poll re-syncs lastSeen and resumes delta emission.
//
//   - ctx.Done() is the shutdown signal — a graceful shutdown
//     stops the goroutine before the gRPC server GracefulStop,
//     which is the right ordering (the registry stops accepting
//     new samples while in-flight goroutines finish).
package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/onebox-faas/faas/pkg/netns"
	"github.com/onebox-faas/faas/pkg/wire"
)

// EgressPollInterval is the cadence of the nft-counter scrape loop.
// 15 s matches the conventional Prometheus scrape interval and keeps
// the per-tenant alert latency under one minute (alert rule uses
// rate() over 1m).
const EgressPollInterval = 15 * time.Second

// popCountersFunc is the test seam: production code calls netns.PopCounters,
// unit tests inject a stub. The signature mirrors netns.PopCounters so
// the function pointer is interchangeable at the call site.
type popCountersFunc func(ctx context.Context) (map[string]uint64, error)

// runEgressPoll starts the egress-deny counter poll goroutine and
// returns when ctx is cancelled. It is intended to be launched
// with `go runEgressPoll(ctx, ops, nil, EgressPollInterval, log)` from main.go.
//
// The popCounters parameter is the function-typed seam; pass nil
// to use the production netns.PopCounters. Unit tests inject a
// stub via the explicit override. The interval is parameterised
// so a unit test can drive the loop at sub-second cadence; the
// production caller passes EgressPollInterval (15 s).
//
// On a non-metal build (the default for unit tests on a dev box),
// netns.PopCounters is a stub that returns an empty map and a nil
// error — the loop runs but emits nothing, which is the right idle
// behaviour (no drops surfaced, no lastSeen drift, no log spam).
func runEgressPoll(ctx context.Context, ops *wire.OpsMetrics, popCounters popCountersFunc, interval time.Duration, log *slog.Logger) {
	if popCounters == nil {
		popCounters = netns.PopCounters
	}
	if interval <= 0 {
		interval = EgressPollInterval
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	// lastSeen[name] = the counter value observed on the previous
	// successful poll. Initialised lazily on the first poll so the
	// first tick never emits a delta (which would be a misleading
	// "host has been silently dropping for hours" spike right after
	// vmmd starts).
	lastSeen := make(map[string]uint64)
	primed := false
	// Closed (cidr, family) set walked once per poll — small
	// (~12 v4 + ~7 v6 = 19 entries per PR-E plan). The catalog is
	// stable for the lifetime of the daemon; if it ever becomes
	// dynamic, re-walk per-poll.
	catalog := netns.NewDefaultDenySet().Entries
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			values, err := popCounters(ctx)
			if err != nil {
				log.Warn("egress poll failed", "err", err)
				continue
			}
			if !primed {
				// First poll: seed lastSeen only, do not emit. Avoids
				// a single "0 → curr" spike on every vmmd restart.
				for _, e := range catalog {
					lastSeen[e.CounterName] = values[e.CounterName]
				}
				primed = true
				continue
			}
			for _, e := range catalog {
				curr := values[e.CounterName]
				prev := lastSeen[e.CounterName]
				if curr < prev {
					// Counter reset (nftables table flush, snapshot
					// resume, manual `nft reset counters`). Reset
					// lastSeen and continue without emitting a
					// negative delta — Prometheus counters are
					// monotonic, and Add(-x) on a CounterVec panics.
					log.Debug("egress counter reset", "name", e.CounterName, "prev", prev, "curr", curr)
					lastSeen[e.CounterName] = curr
					continue
				}
				delta := curr - prev
				lastSeen[e.CounterName] = curr
				if delta == 0 {
					continue
				}
				ops.EgressDeny(e.CounterName, e.Family.String()).Add(float64(delta))
			}
		}
	}
}
