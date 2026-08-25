// Package main — readiness.go constructs the schedd-side
// /readyz probe (issue #571 PR-A2). Two signals:
//
//   - PG ping: schedd is the SOLE writer to the `instances`
//     table (CLAUDE.md Component ownership); a degraded pgxpool
//     means cold wakes cannot be admitted. The probe uses
//     NewPGPingSignal against the same pool the engine reads
//     and writes.
//   - gRPC listener bound: flips true once deps.listen() returns
//     a usable net.Listener. vmmd dials this listener on every
//     wake; /readyz cannot return 200 before the listener is up.
//
// The probe is exposed on the metrics mux via
// pkg/wire.ControlMuxLite. The customer-side /readyz
// (cmd/<daemon>/handlers_ready.go family) is the cookie-auth
// path with a rich JSON body — the operator-side /readyz on the
// metrics mux returns a short ASCII body for the LB scrape.
package main

import (
	"context"
	"sync"
	"time"

	"github.com/onebox-faas/faas/pkg/wire"
)

// pgPool is the subset of *pgxpool.Pool we need for the PG
// ping signal. Defined locally so the test path doesn't have
// to import pgxpool.
type pgPool interface {
	Ping(ctx context.Context) error
}

// BuildReadinessProbe constructs the schedd /readyz probe.
// pgPingEvery is the cadence the PG ping signal uses
// (typically 5 s); the helper goroutine pings on this cadence
// and flips the signal ready on success / not-ready with the
// error message on failure.
//
// The returned grpcBoundSignal's MarkBound() must be called
// once immediately before gsrv.Serve(lis) inside the schedd
// serve goroutine (PR #1091 review Finding 5). Calling it
// earlier — right after deps.listen() — leaves a ~465-line
// setup window (gsrv, scheddgrpc.NewWithStats, /metrics
// endpoint, scaleup trigger, …) where a panic or early
// return would leave /readyz reporting ready while no gRPC
// server is actually running. MarkBound is sync.Once-guarded
// so it's idempotent and race-free across the boot path.
// The returned stop func flips the PG signal to "pg ping
// stopped" on shutdown; the daemon boot path defers it.
//
// nil pool (unit-test path with no metrics_addr wired) returns
// a probe with no PG signal — only the gRPC bound signal. The
// /readyz body surfaces the failing reason, so an operator
// reading the panel sees "pg pool nil" rather than a silent 200.
func BuildReadinessProbe(ctx context.Context, pool pgPool, pgPingEvery time.Duration) (*wire.ReadyzProbe, *grpcBoundSignal) {
	p := &wire.ReadyzProbe{}
	if pool != nil {
		sig, stop := wire.NewPGPingSignal(ctx, pool, pgPingEvery)
		p.RegisterSignal(sig, stop)
	} else {
		s := p.Register()
		s.Set(false, "pg pool nil (test path)")
	}
	bound := &grpcBoundSignal{}
	p.RegisterSignal(bound.Signal(), nil)
	return p, bound
}

// grpcBoundSignal — see cmd/vmmd/readiness.go for the canonical
// comment. Same shape: Signal() returns the underlying
// *wire.ReadySignal; MarkBound() flips it ready. MarkBound is
// guarded by sync.Once so the flip can be called from the serve
// goroutine without races and a panic during the ~465 lines of
// schedd setup between deps.listen() and gsrv.Serve cannot
// leave /readyz wedged in a non-bound state on a retry
// (PR #1091 review Finding 5).
type grpcBoundSignal struct {
	sig  *wire.ReadySignal
	once sync.Once
}

func (g *grpcBoundSignal) Signal() *wire.ReadySignal {
	if g.sig == nil {
		g.sig = &wire.ReadySignal{}
		g.sig.Set(false, "grpc not yet bound")
	}
	return g.sig
}

func (g *grpcBoundSignal) MarkBound() {
	g.once.Do(func() {
		g.Signal().Set(true, "")
	})
}
