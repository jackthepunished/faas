// pkg/sched/migrating_watchdog.go — Tier A6 (ADR-067) migrating-
// instance watchdog ticker.
//
// Drives Engine.ReconcileExpiredMigrations on a 1 s ticker.
// The watchdog is the ONLY writer that can move a row out of
// state='migrating' without a peer commit — every Phase-4 path
// (CancelInstanceMigration) requires a peer, and the peer is
// the very thing that's gone when the new owner vmmd dies
// mid-handoff. The watchdog catches the realistic operational
// failure mode for the four-phase handoff (ADR-066): the new
// owner dies silently, the row stays in 'migrating' forever,
// and the next live-migration attempt fails on the
// `state='running'` predicate because the row's current state
// is 'migrating'.
//
// Parallel to pkg/sched/rebalancer.go (Tier A4, parked-app
// rebalance) and pkg/sched/live_migrator.go (Tier A5, live-
// instance migration). The three watchers are deliberately
// separate because they have different failure modes,
// different metric labels, and different per-tick caps.
//
// Failure modes:
//   - ticker fires: call Engine.ReconcileExpiredMigrations,
//     log the per-batch result.
//   - handle returns err: log Warn, continue. A transient PG
//     blip must not stop the loop; the next tick retries.
//   - ctx cancel: drain ticker, return.

package sched

import (
	"context"
	"time"
)

// MigratingWatchdogHandle is the per-tick work function the
// migrating-instance watchdog invokes. The cmd/schedd wiring
// passes Engine.ReconcileExpiredMigrations. Tests pass a
// counter-recording stub.
type MigratingWatchdogHandle func(ctx context.Context) (int, error)

// MigratingWatchdogLogger is the minimal slog surface this
// watchdog needs. Tests pass nil; the watchdog logs nothing
// in that case.
type MigratingWatchdogLogger interface {
	Warn(msg string, args ...any)
	Info(msg string, args ...any)
}

// MigratingWatchdog is the ticker-driven loop that self-heals
// stuck state='migrating' rows. The per-instance outcome
// (reinvited / hard_deleted / conflict / error) is reported by
// the metric inside the handle (schedd_migrating_reconcile_total)
// — the watchdog itself only logs the batch-level result.
type MigratingWatchdog struct {
	handle   MigratingWatchdogHandle
	interval time.Duration
	log      MigratingWatchdogLogger
}

// NewMigratingWatchdog wires the watchdog. handle MUST be
// non-nil; the watchdog panics on a nil handle so a missed
// wiring surfaces at startup rather than as silent dead-air
// at every tick. interval is the per-tick cadence (the
// caller passes api.MigratingWatchdogIntervalSeconds or the
// engine-overridden value).
func NewMigratingWatchdog(handle MigratingWatchdogHandle, interval time.Duration, log MigratingWatchdogLogger) *MigratingWatchdog {
	if handle == nil {
		panic("sched: NewMigratingWatchdog: handle is nil (migrating watchdog will dead-air at every tick)")
	}
	if interval <= 0 {
		panic("sched: NewMigratingWatchdog: interval must be > 0")
	}
	return &MigratingWatchdog{handle: handle, interval: interval, log: log}
}

// Run drives the ticker until ctx is cancelled. Returns
// ctx.Err() on cancellation. Each tick reconciles up to
// api.MigratingWatchdogTickLimit wedged rows in a single batch;
// errors per batch are logged at Warn and never propagate so
// a transient PG blip cannot stop the loop.
func (w *MigratingWatchdog) Run(ctx context.Context) error {
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			reconciled, err := w.handle(ctx)
			if err != nil {
				if w.log != nil {
					w.log.Warn("sched: migrating watchdog: tick failed",
						"err", err)
				}
				continue
			}
			if w.log != nil && reconciled > 0 {
				w.log.Info("sched: migrating watchdog: tick reconciled",
					"reconciled", reconciled)
			}
		}
	}
}
