// TLS cert rotation rotator for schedd (ADR-052 §5 / PR-E).
//
// Problem today: schedd's two mTLS legs (the inbound gatewayd-internal
// server listener + the outbound vmmd dial) load their cert material
// once at startup via LoadServerTLSWithVerifier / LoadVMMTLSWithVerifier.
// A `gregale pki rotate` writes new files on disk but does NOT take
// effect until the daemon restarts.
//
// The fix: each leg threads a tlsRotator through the SIGHUP-driven
// cert reload path. The rotator holds the live *tls.Config behind
// an atomic.Pointer so concurrent reads (handshake path, SIGHUP-driven
// swap path) don't race. Two pieces:
//
//  1. The daemon calls Load*TLSConfigWithReload at startup, passing
//     the rotator's Reload closure as the per-handshake callback.
//     Stdlib invokes that callback on every handshake, where it
//     returns the rotator's current *tls.Config.
//  2. On SIGHUP, WatchTLSReload (pkg/wire) calls a separate
//     disk-loader closure, then rotator.Set(newCfg). The next
//     handshake reads the freshly-swapped config via the per-handshake
//     callback stdlib already consults; no listener rebuild needed.
//
// Failure posture matches the egress-bundle reload contract (best
// effort, prior material stays live). A failed reload keeps the prior
// config live in the rotator and is Warn-logged; the daemon does NOT
// refuse to keep running on a cert error.
//
// Sister files: cmd/vmmd/tls_rotator.go and cmd/apid/tls_rotator.go.
// Identical shape — kept per daemon for ergonomics. Each daemon
// imports pkg/wire.WatchTLSReload (no rotation logic duplication).
package main

import (
	"crypto/tls"
	"sync/atomic"
)

// tlsRotator is the schedd-side Set[Get] adapter for
// pkg/wire.WatchTLSReload. It holds the live *tls.Config behind an
// atomic.Pointer so:
//
//   - WatchTLSReload can swap the live config on every successful
//     SIGHUP-driven reload without contending with the handshake
//     path.
//   - Handlers / dialers that need the current config (e.g. for
//     Listen, ServerCreds, or `deps.dialVMM` at the call instant)
//     call Get and read the stable pointer.
//
// Sync: atomic.Pointer is the load-bearing primitive. Set is called
// only on successful reload (WatchTLSReload never swaps on error).
// Get may be called from any goroutine.
type tlsRotator struct {
	ptr atomic.Pointer[tls.Config]
}

// newTLSRotator builds a rotator holding initial. A nil initial
// is tolerated and degrades to "no TLS configured" — Set becomes a
// silent no-op (the rotator never acquired material). This keeps
// the single-box / no-cluster paths from crashing on boot.
func newTLSRotator(initial *tls.Config) *tlsRotator {
	r := &tlsRotator{}
	if initial != nil {
		r.ptr.Store(initial)
	}
	return r
}

// Set replaces the rotator's live *tls.Config. Called by
// pkg/wire.WatchTLSReload after a successful reload. A nil cfg is
// silently dropped so a buggy loader that returns (nil, nil) on
// success doesn't silently null out an active rotator —
// WatchTLSReload already warns on this case (cmd/apid → keep prior).
func (r *tlsRotator) Set(cfg *tls.Config) {
	if cfg == nil || r == nil {
		return
	}
	r.ptr.Store(cfg)
}

// Get returns the rotator's live *tls.Config, or nil if no
// material has ever been Set (the single-box / empty-cluster
// posture). Callers that branch on a nil cfg should treat the
// nil return the same as Load*TLSConfig's (nil, nil) contract:
// the dial/listen site stays in the legacy shape.
func (r *tlsRotator) Get() *tls.Config {
	if r == nil {
		return nil
	}
	return r.ptr.Load()
}

// Reload returns a wire.ReloadFunc the Load*TLSConfigWithReload
// factory can hand to stdlib. On every handshake, stdlib consults
// the closure, which reads the live *tls.Config from the rotator.
// The closure is goroutine-safe: it holds only the rotator's
// pointer (an atomic load) and never writes through it.
//
// The closure is the unit of freshness: stdlib re-runs it on
// every handshake, so a SIGHUP-driven Set between two handshakes
// is observable to the second handshake. gRPC's tlsCreds keeps the
// outer *tls.Config for the listener's lifetime; the reload
// handshake path is the per-handshake indirection that surfaces
// the rotated material.
//
// initial is the daemon-supplied fallback (the startup value, in
// case WatchTLSReload never fires): the closure returns initial
// when the rotator is empty, so a single-box daemon that loses
// its rotator still hands stdlib something usable.
func (r *tlsRotator) Reload(initial *tls.Config) func() (*tls.Config, error) {
	return func() (*tls.Config, error) {
		if r == nil {
			return initial, nil
		}
		if cur := r.ptr.Load(); cur != nil {
			return cur, nil
		}
		return initial, nil
	}
}
