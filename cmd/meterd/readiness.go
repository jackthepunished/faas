// Package main — readiness.go constructs the meterd-side
// /readyz probe (issue #571 PR-A2). meterd already exposes
// /healthz (cmd/meterd/main.go:~1000) which renders a rich JSON
// verdict via loop.Health(time.Now()). This file adds a
// short-ASCII /readyz on the same metrics mux that surfaces
// the same loop.Health verdict via a 1s adapter goroutine.
//
// Why an adapter goroutine (instead of calling loop.Health on
// the /readyz hot path): the /healthz handler already calls
// loop.Health on every scrape, so the verdict is always
// fresh — no point recomputing it. The adapter ticks at 1 s
// to keep the ReadySignal up-to-date with the LAST tick fire
// times so a /readyz scrape that lands between two /healthz
// scrapes still sees the current verdict.
//
// When loop.Health reports Healthy==false, the ReadySignal
// reason surfaces the stale tick names comma-separated so an
// operator reading /readyz knows which sweep is hung. The
// /healthz handler still emits the per-tick JSON for
// dashboards.
package main

import (
	"strings"
	"time"

	"github.com/onebox-faas/faas/pkg/meter"
	"github.com/onebox-faas/faas/pkg/wire"
)

// BuildReadinessProbe constructs the meterd /readyz probe
// driven by loop.Health. The probe starts ready=true (a fresh
// daemon with no ticks fired yet still reports Healthy=false
// per meter.Health; the operator sees "stale: ..." immediately
// on the first /readyz scrape).
//
// Returns the probe + a stop func the daemon boot path defers.
// The stop func flips the signal to "meterd stopping" on
// shutdown so the SIGTERM drain window surfaces in
// daemon_ready{daemon="meterd"} as 0.
func BuildReadinessProbe(loop *meter.Loop) (*wire.ReadyzProbe, func()) {
	sig := &wire.ReadySignal{}
	// Pre-arm so the first /readyz scrape doesn't see a stale
	// 503 — the first adapter tick will flip it to false if
	// any tick is stale. Mirrors pkg/wire/readiness.go
	// NewStalenessSignal's pre-arm.
	sig.Set(true, "")
	p := &wire.ReadyzProbe{}
	p.RegisterSignal(sig)

	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		t := time.NewTicker(time.Second)
		defer t.Stop()
		// First evaluation fires immediately (the first tick is
		// at +1 s).
		evaluate := func() {
			if loop == nil {
				sig.Set(false, "meterd loop nil")
				return
			}
			status := loop.Health(time.Now())
			if status.Healthy {
				sig.Set(true, "")
				return
			}
			if len(status.Stale) == 0 {
				sig.Set(false, "unhealthy")
				return
			}
			sig.Set(false, "stale: "+strings.Join(status.Stale, ","))
		}
		evaluate()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				evaluate()
			}
		}
	}()
	stopFn := func() {
		close(stop)
		<-done
		sig.Set(false, "meterd stopping")
	}
	return p, stopFn
}