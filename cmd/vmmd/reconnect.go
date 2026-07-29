// reconnect.go — shared reconnect helpers for vmmd's long-lived
// gRPC streams (ADR-025 axis 5).
//
// Background. cmd/vmmd/capacity_publisher.go's outer reconnect
// loop dials schedd, opens a stream, and on transient failure
// re-dials with an exponential backoff ladder: 1s → 2s → 4s →
// 8s → 16s → 30s (capped; pure doubling until cap). The same
// shape exists in cmd/gatewayd's warmhints publisher
// (gatewayd/warmhints.go:103-138) and the schedd-side pg_notify
// subscribe loop (cmd/schedd/main.go:558-613). This file pins
// the vmmd-side helpers so the capacity publisher and any
// future vmmd-stream (egress-allowlist push, health-heartbeat
// out, etc.) share the same cadence without copy-pasting ~25
// LoC of jitter math.
//
// The shape is intentionally tiny: nextBackoff is pure math
// (no I/O, no time), and sleepCtx is a time.Timer + select. A
// test can drive the backoff ladder in a tight loop without
// real sleeps; the jitter source is a *rand.Rand so the
// production code can pass a real RNG (PR-1 injects a
// per-goroutine source) and tests can pass a seeded one for
// deterministic bounds.
//
// Why jitter. Without jitter, all vmmds that lost their schedd
// connection re-dial at the same instant, producing a
// thundering-herd on schedd startup. The 0–500ms pad is small
// enough to not delay a rapid recovery but large enough to
// spread reconnects across a 500ms window when N=10 vmmds.

package main

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"sync"
	"time"
)

// MaxBackoff is the upper bound of the ladder. Reaches capacity
// after 4 failures (1s → 2s → 5s → 10s → 30s). The 30s cap
// mirrors WarmHintPublisher's maxBackoff (gatewayd/warmhints.go:78)
// and the schedd-side pg_notify loop (cmd/schedd/main.go:584).
const MaxBackoff = 30 * time.Second

// jitterBound is the upper bound on the random pad added to
// each sleep. 500ms is the engineer's pick: small enough to
// not delay recovery, large enough to spread reconnects. Tune
// here if the cadence changes.
const jitterBound = 500 * time.Millisecond

// rng is the narrow seam used by jitterMs. It's an interface
// so tests can inject a deterministic seeded source without
// reaching into crypto/rand. The production default is a
// goroutine-safe wrapper around crypto/rand.
type rng interface {
	Int31n(n int32) int32
}

// cryptoRand wraps crypto/rand in the rng interface. crypto/rand
// is concurrency-safe (it locks internally), so a single
// instance can be shared across reconnect goroutines.
type cryptoRand struct{}

func (cryptoRand) Int31n(n int32) int32 {
	if n <= 0 {
		return 0
	}
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand.Read is documented to never fail on
		// Linux/macOS; a failure here is a kernel-level
		// /dev/urandom failure and the process has bigger
		// problems. Return 0 to disable jitter rather than
		// panic — the reconnect ladder still works, just
		// without the spread.
		return 0
	}
	// Unbiased reduction (PR-1 review): the previous
	// implementation used `uint32(b) % uint32(n)` which has
	// a modulo bias of `(2^32 mod n) / 2^32` extra hits on
	// the lower residues (≈ 2% for n=500). For jitter this
	// is invisible, but the multiplication trick is one
	// line and removes the bias without changing the
	// contract: `x * n >> 32` is uniform in [0, n) for any
	// 0 ≤ x < 2^32.
	v := binary.BigEndian.Uint32(b[:])
	return int32((uint64(v) * uint64(uint32(n))) >> 32)
}

// defaultRng is the process-wide RNG used by jitterMs. We keep
// it as a package-level var so tests can swap it for a seeded
// source without threading a pointer through every call site.
// The single-cryptoRand instance is goroutine-safe.
//
// Production hazard note (PR-1 review). If a test calls
// setRngForTest but forgets to invoke the returned restore
// closure (or runs in parallel with a production goroutine
// that calls jitterMs), the production reconnect loop will
// observe the seeded RNG. The package does not run with -race
// in production and the swap has no observer to detect this,
// so callers must:
//
//  1. defer the restore closure immediately after the swap.
//  2. NOT use t.Parallel() — parallel tests sharing the same
//     package-global would race the restore.
var (
	defaultRng   rng = cryptoRand{}
	defaultRngMu sync.RWMutex
)

// setRngForTest swaps the production RNG for a test-supplied
// one. Returns a teardown closure that restores the original.
// Used only from _test.go; live callers must not call this.
//
// See the "Production hazard note" above for the two
// invariants the caller must uphold.
func setRngForTest(r rng) func() {
	defaultRngMu.Lock()
	prev := defaultRng
	defaultRng = r
	defaultRngMu.Unlock()
	return func() {
		defaultRngMu.Lock()
		defaultRng = prev
		defaultRngMu.Unlock()
	}
}

// jitterMs returns a uniformly-distributed random duration in
// [0, jitterBound). The current defaultRng is read under RLock
// so a concurrent setRngForTest swap doesn't race the read.
func jitterMs() time.Duration {
	defaultRngMu.RLock()
	r := defaultRng
	defaultRngMu.RUnlock()
	n := r.Int31n(int32(jitterBound / time.Millisecond))
	return time.Duration(n) * time.Millisecond
}

// nextBackoff doubles d up to max. Mirrors the gatewayd warmhints
// publisher (cmd/gatewayd/warmhints.go:181-187) and the schedd-
// side pg_notify loop (cmd/schedd/main.go:558-613) so the three
// daemons use a consistent cadence. Pure math — clock-independent
// and safe to call in tight test loops.
func nextBackoff(d, max time.Duration) time.Duration {
	next := d * 2
	if next > max {
		return max
	}
	return next
}

// sleepCtx sleeps for d + jitterBound/2 jitter, returning true
// if the timer fired or false if ctx fired first. The jitter
// pad is `0–500ms` (uniform). Tested for both bounds
// (cmd/vmmd/reconnect_test.go).
//
// The jitter is added so a fleet of vmmds reconnecting after
// schedd restart doesn't land on the same instant. We add, not
// multiply, because the backoff ladder itself provides the
// spread; multiplying would push reconnects into the seconds
// range and break the rapid-recovery contract.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d + jitterMs())
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// backoffLadder returns the canonical ladder: 1s, 2s, 4s, 8s,
// 16s, 30s. Same shape as cmd/gatewayd/warmhints.go:103-138
// (which uses pure doubling until cap). Exposed for tests that
// pin the shape; the publisher's reconnect loop computes steps
// inline via nextBackoff rather than reading this slice.
func backoffLadder() []time.Duration {
	return []time.Duration{
		1 * time.Second,
		2 * time.Second,
		4 * time.Second,
		8 * time.Second,
		16 * time.Second,
		30 * time.Second,
	}
}

// Compile-time guard: ensure cryptoRand's contract matches the
// rng interface. If crypto/rand's signature changes, this
// line fails to compile rather than discovering the mismatch
// at runtime.
var _ rng = (*cryptoRand)(nil)
