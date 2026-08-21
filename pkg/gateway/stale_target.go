package gateway

import (
	"context"
	"sync/atomic"
)

// staleTargetSignal is request-local state shared by the handler and the
// forwarding bridge. A bridge failure can mean that the routing cache still
// points at an instance whose vmmd/netns has disappeared; keeping that target
// cached turns one infrastructure failure into a persistent 502 loop. The
// signal deliberately lives in context rather than in an HTTP header so the
// internal decision can never leak to a customer response.
type staleTargetSignal struct {
	stale atomic.Bool
}

type staleTargetSignalKey struct{}

func withStaleTargetSignal(ctx context.Context, signal *staleTargetSignal) context.Context {
	return context.WithValue(ctx, staleTargetSignalKey{}, signal)
}

func markStaleTarget(ctx context.Context) {
	if signal, ok := ctx.Value(staleTargetSignalKey{}).(*staleTargetSignal); ok && signal != nil {
		signal.stale.Store(true)
	}
}

func staleTargetDetected(ctx context.Context) bool {
	signal, ok := ctx.Value(staleTargetSignalKey{}).(*staleTargetSignal)
	return ok && signal != nil && signal.stale.Load()
}
