// PG-backed NodeVerifier for production multi-box deployments (ADR-056).
//
// The verifier holds a snapshot of compute_nodes.name -> compute_nodes.id,
// refreshed on every 'compute_node_changed' pg_notify tick. The
// snapshot is read on every handshake (RLock-guarded, no per-handshake
// DB round-trip).
//
// Drain-loop shape mirrors pkg/sched.NodeKeyRegistry.Run — the same
// subscribeWithReconnect pattern + Run(ctx, ch) shape that every
// daemon already uses. nil-receiver is tolerated (no-op drain; the
// daemon simply doesn't wire the verifier when the multi-box gate is
// closed).
//
// Snapshot refresh contract (defensive): a transient loader failure
// keeps the last-known-good snapshot. A de-sync to "allow nothing"
// would brick the cluster's mTLS legs — a single bad Postgres read
// would force every daemon's mTLS handshake to fail. The fresh
// snapshot only swaps in on success.

package wire

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	"github.com/onebox-faas/faas/pkg/db"
)

// NodeLoader is the production-side seam that returns the registered
// (cn, compute_node.id) tuples. NewPGNodeVerifier calls this on every
// refresh; tests can swap an in-memory loader. The two-field shape is
// minimal: production only needs name (== CN lookup key); ID is
// retained for diagnostic logging in LookupCN errors.
type NodeLoader interface {
	LoadNodes(ctx context.Context) ([]NodeRow, error)
}

// NodeRow is the minimal projection of compute_nodes needed by the
// verifier: the leaf-CN (== per-daemon identity from the leaf cert
// and == compute_nodes.name) and the compute_nodes.id (UUID) for
// diagnostics.
type NodeRow struct {
	CN string // matches Subject.CommonName on the registered leaf (== compute_nodes.name)
	ID string // compute_nodes.id (uuid); for diagnostics only
}

// PGNodeVerifier is the production NodeVerifier. Holds a snapshot
// (cn -> id) map, refreshed on every 'compute_node_changed' notify
// (covering both compute_nodes AND compute_node_keys per migration
// 00075's broader trigger). The snapshot is read on every handshake;
// the read path takes RLock, so concurrent dials don't serialize.
type PGNodeVerifier struct {
	mu     sync.RWMutex
	snap   map[string]string // cn (== name) -> id
	loader NodeLoader
	log    *slog.Logger
}

// NewPGNodeVerifier constructs an empty verifier bound to loader+log.
// The wiring goroutine calls Refresh once at startup, then
// subscribeWithReconnect(ctx, ..., PGNodeVerifier.Run) drives the
// notify-driven refresh loop.
func NewPGNodeVerifier(loader NodeLoader, log *slog.Logger) *PGNodeVerifier {
	return &PGNodeVerifier{
		snap:   make(map[string]string),
		loader: loader,
		log:    log,
	}
}

// LookupCN implements NodeVerifier. RLock-guarded read on the
// snapshot; nil-receiver returns nil (AllowAll) so legacy callers
// that pass a nil verifier don't crash.
func (v *PGNodeVerifier) LookupCN(cn string) error {
	if v == nil {
		return nil
	}
	v.mu.RLock()
	id, ok := v.snap[cn]
	v.mu.RUnlock()
	if !ok {
		return nodeVerifierWithCN(ErrNodeVerifierCNMismatch, cn)
	}
	_ = id // id is retained for diagnostics; future API can surface it via errors.As
	return nil
}

// Refresh swaps the snapshot for a fresh load. Errors keep the
// last-known-good snapshot (defensive: a transient loader failure
// must not de-sync the verifier to "allow nothing"). Returns the
// new size for diagnostics.
//
// A nil receiver returns an error so a forgotten wire at startup
// fails loudly (the daemon's NewPGNodeVerifier call must always
// succeed; if Refresh is never called, the snapshot stays empty
// and every handshake fails with ErrNodeVerifierCNMismatch).
func (v *PGNodeVerifier) Refresh(ctx context.Context) (int, error) {
	if v == nil {
		return 0, fmt.Errorf("wire: nil PGNodeVerifier")
	}
	if v.loader == nil {
		return 0, fmt.Errorf("wire: PGNodeVerifier has nil loader")
	}
	rows, err := v.loader.LoadNodes(ctx)
	if err != nil {
		if v.log != nil {
			v.log.Warn("wire: node verifier loader failed; keeping last-known-good",
				"err", err, "size", v.Size())
		}
		return v.Size(), fmt.Errorf("wire: load nodes: %w", err)
	}
	fresh := make(map[string]string, len(rows))
	for _, r := range rows {
		if r.CN == "" {
			// Skip malformed rows (defensive: an empty CN would
			// collide with every leaf's empty-CN error path).
			continue
		}
		fresh[r.CN] = r.ID
	}
	v.mu.Lock()
	v.snap = fresh
	v.mu.Unlock()
	if v.log != nil {
		v.log.Info("wire: node verifier snapshot refreshed",
			"size", len(fresh))
	}
	return len(fresh), nil
}

// Size returns the count of registered CNs (diagnostics + tests).
func (v *PGNodeVerifier) Size() int {
	if v == nil {
		return 0
	}
	v.mu.RLock()
	defer v.mu.RUnlock()
	return len(v.snap)
}

// Run drains an already-opened 'compute_node_changed' channel (or any
// other notification source) and refreshes the snapshot on every
// delivery. Every notify triggers Refresh; the loop survives
// transient loader failures because Refresh keeps the last-known-good
// snapshot. nil receiver is tolerated (no-op drain; identical shape
// to pkg/sched.NodeKeyRegistry.Run).
func (v *PGNodeVerifier) Run(ctx context.Context, ch <-chan db.Notification) error {
	if v == nil {
		<-ctx.Done()
		return ctx.Err()
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case _, ok := <-ch:
			if !ok {
				return nil
			}
			if _, err := v.Refresh(ctx); err != nil {
				// Refresh already logs at Warn; the loop survives
				// last-known-good.
				_ = err
			}
		}
	}
}

// Ensure PGNodeVerifier satisfies NodeVerifier at compile time.
var _ NodeVerifier = (*PGNodeVerifier)(nil)
