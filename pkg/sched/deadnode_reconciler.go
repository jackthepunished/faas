// deadnode_reconciler.go — ticker-driven sweep that closes the
// dead-node billing leak.
//
// Background. schedd's heartbeat goroutine (heartbeat.go) pings every
// registered vmmd and calls Store.MarkComputeNodeInactive when a node
// stops answering. That UPDATE touches only `compute_nodes` — it
// deliberately does not rewrite instance rows, because placement reads
// node state rather than mirroring it onto instances.
//
// The consequence nobody wired: meterd's sampler bills every instance
// whose State.CountsForRAM() is true (pkg/meter/sampler.go) with no
// node-liveness cross-check. A vmmd that dies without transitioning
// its rows therefore leaves them RUNNING indefinitely, and the
// customer keeps paying for a VM that no longer exists. The §6.1
// watchdog does not cover this: it sweeps WAKING / COLD_BOOTING /
// SNAPSHOTTING only. The idle reaper does not cover it either — it
// keys on last_request_at and its terminal action is a park, which
// cannot succeed when there is no live vmmd to snapshot the VM.
//
// This sweeper is the missing backstop. It runs on its own cadence
// (api.DeadNodeReconcilerIntervalSeconds) rather than inside the 1s
// §6.1 watchdog tick, because the staleness window it enforces is
// 120s — ticking it every second would issue ~120 no-op queries per
// node death before the first row is even eligible.
//
// The type mirrors MigratingWatchdog's shape (ticker + handle +
// minimal logger). Those two are close enough that a future PR could
// collapse them into one generic `periodicReconciler`; that rename
// touches every ADR-067 call site and reference, so it is deliberately
// left out of this fix.

package sched

import (
	"context"
	"time"
)

// DeadNodeReconcilerHandle is the per-tick work function. Production
// wiring passes Engine.ReconcileDeadNodeInstances; tests pass a stub
// that records invocations. Returns (reconciled, err) — the same
// contract as the other reconcile handles in this package.
type DeadNodeReconcilerHandle func(ctx context.Context) (int, error)

// DeadNodeReconcilerLogger is the minimal slog surface this sweeper
// needs. Tests may pass nil, in which case the sweeper logs nothing.
type DeadNodeReconcilerLogger interface {
	Warn(msg string, args ...any)
	Info(msg string, args ...any)
}

// DeadNodeReconciler is the ticker-driven loop that terminates RUNNING
// instances stranded on a dead compute_node. Per-instance outcomes
// (failed / conflict / error) are reported by the metric inside the
// handle (<daemon>_dead_node_reconcile_total); this loop only logs the
// batch-level result.
type DeadNodeReconciler struct {
	handle   DeadNodeReconcilerHandle
	interval time.Duration
	log      DeadNodeReconcilerLogger
}

// NewDeadNodeReconciler wires the sweeper. handle MUST be non-nil: a
// nil handle would make the loop dead-air at every tick while looking
// perfectly healthy, and the failure mode it guards (silent
// over-billing) is itself invisible — so a missed wiring must surface
// at startup, not in a customer's invoice. interval must be > 0.
func NewDeadNodeReconciler(handle DeadNodeReconcilerHandle, interval time.Duration, log DeadNodeReconcilerLogger) *DeadNodeReconciler {
	if handle == nil {
		panic("sched: NewDeadNodeReconciler: handle is nil (dead-node reconciler will dead-air at every tick)")
	}
	if interval <= 0 {
		panic("sched: NewDeadNodeReconciler: interval must be > 0")
	}
	return &DeadNodeReconciler{handle: handle, interval: interval, log: log}
}

// Run drives the ticker until ctx is cancelled, returning ctx.Err().
// Per-tick errors are logged at Warn and never propagate: a transient
// PG blip must not stop the loop, because stopping the loop silently
// re-opens the billing leak.
func (r *DeadNodeReconciler) Run(ctx context.Context) error {
	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			reconciled, err := r.handle(ctx)
			if err != nil {
				if r.log != nil {
					r.log.Warn("sched: dead-node reconciler tick failed", "err", err)
				}
				continue
			}
			if reconciled > 0 && r.log != nil {
				// Warn, not Info: a non-zero count means a vmmd died
				// without transitioning its rows and customers were
				// being billed for VMs that no longer exist. That is
				// an incident signal, not routine background repair.
				r.log.Warn("sched: dead-node reconciler terminated orphaned instances",
					"reconciled", reconciled)
			}
		}
	}
}
