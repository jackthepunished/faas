// ADR-053: parent-mount orphan sweep loop. Ticks every
// ParentSweepInterval (default 30s) and force-umounts any parent
// loopback mount that's been live longer than
// vmmdmount.ParentMountMaxAge (30m). A normal staging path takes
// seconds; a mount that survives the max-age window almost
// certainly has an imaged child that crashed mid-flight without
// calling UmountParentExt4.
//
// Pattern: mirrors the schedd watchdog tick — ctx-bound
// select{}, ticker.Stop on exit, no goroutine leak. Exits
// cleanly on ctx.Done (the parent context cancellation is the
// signal that vmmd is shutting down; the deferred
// Registry.SweepAll in main.go is the authoritative final pass).
//
// The sweep is silent on the happy path (zero orphans); a
// non-zero count is logged at Info so an operator chasing
// "why did staging leave a mount behind" can grep vmmd logs.
// A sweep that fails to umount (e.g. EBUSY because a stray
// imaged is still reading) is logged at Warn; the registry
// keeps the entry and the next tick retries — same semantics
// as Registry.SweepOrphans.
package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/onebox-faas/faas/pkg/vmmdmount"
)

// runParentMountSweep is the orphan-sweep goroutine main.go
// launches with a context-bound cancel. Returns when ctx is
// Done (vmmd shutdown). interval<=0 falls back to 30s — the
// production default — so a misconfigured toml doesn't burn
// the loop spinning.
//
// Extracted as a top-level function (not a closure in main)
// so it can be tested directly with a sub-second interval and
// a counted logger.
func runParentMountSweep(ctx context.Context, reg *vmmdmount.Registry, interval time.Duration, log *slog.Logger) {
	if interval <= 0 {
		interval = 30 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			n := reg.SweepOrphans(log)
			if n > 0 {
				log.Info("vmmd: parent-mount orphan sweep", "n", n)
			}
		}
	}
}
