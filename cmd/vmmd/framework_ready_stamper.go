//go:build !ignore

// framework_ready_stamper.go (issue #470 / PR #470-FU-B, extended
// for tail events in issue #667 / ADR-078) is the cmd/vmmd ↔
// pkg/fcvm adapter that bridges the local FrameworkReadyStamper +
// TailTerminalStamper interfaces to the State.Store methods.
//
// Two interfaces, one adapter: a single storeStamper satisfies
// both the FrameworkReadyStamper and TailTerminalStamper
// contracts so the Manager's two receipt paths (the framework_ready
// DGRAM + the tail_event DGRAM) share the same SQL-persistence
// seam. The adapter exists because the Manager shouldn't depend
// on pkg/state (a wider import surface than the receipt path needs);
// the State surface is wider than the two one-method interfaces
// combined, so a direct assignment would loosen the interface.
//
// We accept the abstract state.Store (not the concrete *state.PgStore)
// so the vmmd main loop can wire the stamper whether store is the
// pool-backed pgstore or the in-memory test double — the rebase
// onto the State-interface main loop forced this on us, and the
// interface contract is the right knob for the test path too.
//
// Errors are logged Warn (the in-memory stamp on the live
// Instance is the load-bearing signal for the histogram; the
// SQL column is the durable record the engine's
// captureWarmSnapshot reads back). The receipt still succeeds
// on a transient PG hiccup — same correctness-preserving
// behaviour as the cold-boot path's per-PG-call error handling.

package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/onebox-faas/faas/pkg/state"
)

// stateStamper is the local interface the adapter needs. We
// can't widen fcvm.FrameworkReadyStamper / TailTerminalStamper
// (that would couple the Manager to pkg/state), so the adapter
// declares the minimum surface it consumes. state.Store
// satisfies this for both PgStore and the in-memory test store.
type stateStamper interface {
	SetInstanceFrameworkReadyAt(ctx context.Context, id string, readyAt time.Time) error
	DecrementInstanceTailCount(ctx context.Context, id string, n int32) error
}

// stamperFromStore adapts the State methods to the local
// fcvm.FrameworkReadyStamper + fcvm.TailTerminalStamper
// interfaces. The returned value satisfies BOTH interfaces
// (single storeStamper receiver implements both SetFrameworkReadyAt
// and DecrementInstanceTailCount). The log is non-nil in
// production; nil falls back to slog.Default so the adapter
// is safe to call from tests.
//
// Returns a *storeStamper (not an interface) so the caller can
// pass the same instance to both WithFrameworkReadyStamper and
// WithTailTerminalStamper without two adapter allocations.
func stamperFromStore(s state.Store, log *slog.Logger) *storeStamper {
	if log == nil {
		log = slog.Default()
	}
	return &storeStamper{store: s, log: log}
}

// storeStamper is the concrete adapter. Unused on non-linux
// cmd paths (the framework_ready receiver is itself linux-only,
// so the stamper is only ever wired up on a host where the
// pool is reachable).
type storeStamper struct {
	store stateStamper
	log   *slog.Logger
}

// SetFrameworkReadyAt forwards to the store. The error is logged
// but not returned to the Manager — the in-memory stamp is the
// live authoritative signal; the SQL column is the durable
// record the engine (PR #470-FU-A) reads back. A transient
// error here must not lose the receipt.
func (p *storeStamper) SetFrameworkReadyAt(ctx context.Context, instance string, readyAt time.Time) error {
	if err := p.store.SetInstanceFrameworkReadyAt(ctx, instance, readyAt); err != nil {
		p.log.Warn("vmmd: framework_ready stamper persist",
			"instance", instance, "err", err)
		return err
	}
	return nil
}

// DecrementInstanceTailCount (issue #667 / ADR-078) forwards
// to the store. Mirrors SetFrameworkReadyAt's error policy:
// log Warn, do not return — the in-memory stamp on the live
// Instance is the runner's source of truth (the WaitGroup
// view); the SQL column is the durable mirror the schedd
// reaper reads (PR 4). A transient PG hiccup must not lose
// the receipt; the snapshotAndPark 5s watchdog force-parks
// regardless.
func (p *storeStamper) DecrementInstanceTailCount(ctx context.Context, instance string) error {
	// The TailTerminalStamper interface is the per-receipt (single-step)
	// layer; the watchdog's bulk decrement (n = unfinished count) is
	// issued directly from pkg/sched/engine.snapshotAndPark against
	// state.Store, not through this adapter. So this entry point
	// always passes 1.
	if err := p.store.DecrementInstanceTailCount(ctx, instance, 1); err != nil {
		p.log.Warn("vmmd: tail_terminal stamper persist",
			"instance", instance, "err", err)
		return err
	}
	return nil
}
