// PR-2 egress-byte poll adapter (ADR-046).
//
// This file owns the loop that reads the kernel byte counter at
// `/sys/class/net/<vethHost>/statistics/rx_bytes` for every live
// instance and feeds the per-instance deltas into
// pkg/fcvm/netstats.Cache. The cache's regression-safe semantics
// turn the raw cumulative counter into a per-tick delta that
// vmmd's Stats gRPC handler can serve on the wire without doing
// sysfs I/O on the hot path.
//
// Counter source (ADR-046 §1):
//
//	root ns:    br-tenants ── vethHost
//	netns:      vethPeer ── tap0 ── guest
//
// Customer egress traverses `tap0 → vethPeer → vethHost`. On
// root-side `vethHost` this is **RX**. Reading vethHost.tx_bytes
// would count gateway → guest (ingress), not customer egress. The
// kernel-counter file is
// `/sys/class/net/<vethHost>/statistics/rx_bytes` and it is the
// same counter the per-plan `tc tbf` qdisc reads, so the cap and
// the meter are consistent.
//
// # Design notes (mirror pkg/fcvm/cpustats and cmd/vmmd/cpu_poller.go)
//
//   - The poll cadence is 250 ms — matches the cpustats cache
//     cadence and the schedd instancestats.Poller cadence so
//     vmmd's per-tick deltas line up with schedd's per-tick
//     read window. A looser cadence widens the per-minute drift
//     tolerance; a tighter cadence doubles the sysfs I/O for no
//     observability gain.
//
//   - The SnapshotLive dependency is the Manager.SnapshotLive
//     accessor (ADR-046 / step 6): it returns a fresh
//     (instanceID → vethHost) map under m.mu, so a concurrent
//     Destroy / Wake cannot race the iteration. The map is owned
//     by the caller; the poller does NOT mutate it.
//
//   - The readRXBytes dependency is a function-typed seam so the
//     unit test can swap it for an in-memory stub without
//     touching sysfs. The default is netns.ReadVethRXBytes (a
//     small wrapper over os.ReadFile on the kernel counter
//     file); non-metal builds get a stub that returns 0.
//
//   - The cache handles regression on its own (veth recreation):
//     pkg/fcvm/netstats.Cache.Observe returns ok=false on
//     regression and resets the baseline. The poller just
//     records the observation; the cache decides whether to emit.
//
//   - Poll failures (per-instance sysfs read errors) are logged
//     at Warn and counted via ops.EgressSourceErrors. The cache
//     baseline for that instance stays put — the next successful
//     tick picks up where the failed one left off. A persistent
//     failure shows up in the counter as the alert source.
package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/onebox-faas/faas/pkg/fcvm"
	"github.com/onebox-faas/faas/pkg/fcvm/netstats"
	"github.com/onebox-faas/faas/pkg/netns"
	"github.com/onebox-faas/faas/pkg/wire"
)

// EgressBytesPollInterval is the cadence of the veth byte
// scrape loop. 250 ms matches the cpustats cache cadence and
// the schedd instancestats.Poller cadence.
const EgressBytesPollInterval = 250 * time.Millisecond

// snapshotLiveFunc is the Manager.SnapshotLive seam. Production
// passes mgr.SnapshotLive; tests inject a stub.
type snapshotLiveFunc func() map[string]string

// readRXBytesFunc is the per-instance sysfs read seam. Production
// passes netns.ReadVethRXBytes; tests inject a stub.
type readRXBytesFunc func(vethHost string) (uint64, error)

// runNetworkEgressPoll starts the per-instance veth byte poll
// goroutine and returns when ctx is cancelled. It is launched
// alongside runEgressPoll and runCPUSampleLoop from main.go.
//
// The snapshotLive parameter is the function-typed seam; pass
// nil to use the production mgr.SnapshotLive. The readRXBytes
// parameter is the per-instance sysfs read seam; pass nil to
// use the production netns.ReadVethRXBytes (added in step 6;
// the function pointer contract mirrors netns.PopCounters in
// cmd/vmmd/poller.go).
//
// The cache is mutated under its own mutex; the poller never
// holds any lock across a sysfs read. The order per tick is:
//  1. Snapshot the live instances (returns a fresh map copy).
//  2. For each (instanceID, vethHost), read rx_bytes.
//  3. Hand the reading to netstats.Cache.Observe, which
//     computes the per-instance delta and exposes it to the
//     Stats gRPC handler via Lookup.
//
// A successful poll emits no metric directly — the per-tick
// delta is read by the Stats handler when schedd asks. The
// only Prometheus hook in this loop is EgressSourceErrors on
// per-instance read failures.
func runNetworkEgressPoll(
	ctx context.Context,
	mgr *fcvm.Manager,
	cache *netstats.Cache,
	ops *wire.OpsMetrics,
	snapshotLive snapshotLiveFunc,
	readRXBytes readRXBytesFunc,
	interval time.Duration,
	log *slog.Logger,
) {
	if snapshotLive == nil && mgr != nil {
		snapshotLive = mgr.SnapshotLive
	}
	if readRXBytes == nil {
		readRXBytes = netns.ReadVethRXBytesForPoll
	}
	if interval <= 0 {
		interval = EgressBytesPollInterval
	}
	if cache == nil {
		log.Error("network egress poll: nil cache, exiting goroutine")
		return
	}
	if ops == nil {
		log.Error("network egress poll: nil ops, exiting goroutine")
		return
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if snapshotLive == nil {
				continue
			}
			now := time.Now()
			snapshot := snapshotLive()
			for instanceID, vethHost := range snapshot {
				if vethHost == "" {
					continue
				}
				rx, err := readRXBytes(vethHost)
				if err != nil {
					log.Warn("egress rx_bytes read failed",
						"instance", instanceID,
						"veth", vethHost,
						"err", err)
					if c := ops.EgressSourceErrors(); c != nil {
						c.Inc()
					}
					continue
				}
				cache.Observe(netstats.Observation{
					InstanceID: instanceID,
					RXBytes:    rx,
					At:         now,
				})
			}
		}
	}
}
