// TLS cert rotation rotator for apid (ADR-052 §5 / PR-E).
//
// Identical shape to cmd/schedd/tls_rotator.go and
// cmd/vmmd/tls_rotator.go — kept per daemon for ergonomics. Holds
// the live *tls.Config behind an atomic.Pointer so concurrent reads
// (handshake path, SIGHUP-driven swap path) don't race. Threaded
// through cmd/apid/main.go via three Load*TLSWithPrefixAndVerifierAndReload
// helper calls — one per active TLS cluster (advisory server,
// githubd-bridge server, githubd dial). SIGHUP-driven reloads
// re-read material via pkg/wire.WatchTLSReload.
//
// Failure posture matches the egress-bundle reload contract: a
// failed reload keeps the prior config live in the rotator and is
// Warn-logged; the daemon does NOT refuse to keep running on a
// cert error.
package main

import (
	"crypto/tls"
	"sync/atomic"
)

type tlsRotator struct {
	ptr atomic.Pointer[tls.Config]
}

func newTLSRotator(initial *tls.Config) *tlsRotator {
	r := &tlsRotator{}
	if initial != nil {
		r.ptr.Store(initial)
	}
	return r
}

func (r *tlsRotator) Set(cfg *tls.Config) {
	if cfg == nil || r == nil {
		return
	}
	r.ptr.Store(cfg)
}

func (r *tlsRotator) Get() *tls.Config {
	if r == nil {
		return nil
	}
	return r.ptr.Load()
}

// Reload returns a wire.ReloadFunc stdlib calls on every handshake.
// See cmd/schedd/tls_rotator.go's Reload doc-comment for the full
// contract and the stdlib callback interaction pattern.
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
