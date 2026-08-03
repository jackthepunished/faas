//go:build !ignore

// framework_ready_stamper.go (issue #470 / PR #470-FU-B) is the
// cmd/vmmd ↔ pkg/fcvm adapter that bridges the local
// FrameworkReadyStamper interface to the State.Store.SetInstanceFrameworkReadyAt
// method. The adapter exists because the Manager shouldn't depend
// on pkg/state (a wider import surface than the receipt path needs);
// the State surface is wider than FrameworkReadyStamper's one-method
// contract, so a direct assignment would loosen the interface.
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

	"github.com/onebox-faas/faas/pkg/fcvm"
	"github.com/onebox-faas/faas/pkg/state"
)

// stateStamper is the local interface the adapter needs. We
// can't widen fcvm.FrameworkReadyStamper (that would couple the
// Manager to pkg/state), so the adapter declares the minimum
// surface it consumes. state.Store satisfies this for both
// PgStore and the in-memory test store.
type stateStamper interface {
	SetInstanceFrameworkReadyAt(ctx context.Context, id string, readyAt time.Time) error
}

// stamperFromStore adapts the State.SetInstanceFrameworkReadyAt
// method to the local fcvm.FrameworkReadyStamper interface. The
// log is non-nil in production; nil falls back to slog.Default
// so the adapter is safe to call from tests.
func stamperFromStore(s state.Store, log *slog.Logger) fcvm.FrameworkReadyStamper {
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
