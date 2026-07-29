// Package netstats wraps a per-instance kernel byte counter with a
// regression-safe cumulative cache. The cache turns the raw
// cumulative interface-byte reading on root-side vethHost.rx_bytes
// into a per-tick delta and a cumulative reading that vmmd's
// Stats handler can serve on the wire without doing sysfs I/O on
// the hot path.
//
// # Source — ADR-046 §1
//
// Customer egress traverses `tap0 → vethPeer → vethHost`. On
// root-side `vethHost` this is **RX**. Reading vethHost.tx_bytes
// would count gateway → guest (ingress), not customer egress. The
// kernel-counter file is
// `/sys/class/net/<vethHost>/statistics/rx_bytes` and it is the
// same counter the per-plan `tc tbf` qdisc reads, so the cap and
// the meter are consistent.
//
// # Design
//
// The kernel exposes a monotonic counter (rx_bytes) that resets to
// 0 only at veth recreation (vmmd teardown of the instance).
// Converting that to "bytes this instance transmitted in this
// minute" requires the previous reading. This package holds that
// previous reading per instance and is the single source of truth
// for vmmd's netstats-on-the-wire output.
//
// # Regression contract (ADR-039 §3.1 mirror)
//
// A veth recreation (instance teardown) resets rx_bytes to 0.
// The cache detects this by comparing the new reading to the
// last: a non-monotonic step drops the previous baseline and
// starts a new one, returning ok=false from Observe until two
// post-regression samples have been seen. The schedd-side seam
// stamps Unknown on the first post-regression row and re-baselines
// on the next. Mirrors pkg/fcvm/cpustats.Cache.Observe exactly.
//
// # Concurrency
//
// The cache is safe for one writer (the vmmd network sample loop)
// and many readers (concurrent Stats gRPC handlers). The mu
// protects the map; reads under the mutex are O(1) and short. A
// future change to allow multi-writer pollers would shard by
// instance id; the current vmmd has exactly one sample loop so a
// single mutex is the cheapest correct thing.
package netstats

import (
	"sync"
	"time"
)

// Observation is the cache's input: an instance id, the cumulative
// byte counter from root-side vethHost.rx_bytes (interface bytes,
// not IP bytes — includes Ethernet framing), and the wall-clock
// moment the reading was taken.
type Observation struct {
	InstanceID string
	RXBytes    uint64
	At         time.Time
}

// Reading is the cache's output: the per-tick byte delta since the
// previous observation, and the cumulative byte counter preserved
// across regression. Delta is reset to 0 on baseline / regression
// — the cumulative counter is the load-bearing seam (the future
// egress-billing PR reads CumulativeBytes and computes the
// per-billing-window diff itself, free of the cache's reset
// semantics).
type Reading struct {
	// DeltaBytes is the byte difference between this observation
	// and the previous one. 0 on baseline, regression, or clock
	// skew. Zero is the correct value for "no egress in this
	// window" — distinct from "no data" which is signalled by
	// Valid=false.
	DeltaBytes uint64
	// CumulativeBytes is the cumulative rx_bytes reading this
	// cache has tracked since the last regression. It survives
	// across observations and is the seam the future billing PR
	// reads. Resets to 0 on regression (veth recreation).
	CumulativeBytes uint64
	// Valid is false when the cache has only one observation
	// (baseline not yet established) or the previous observation
	// was dropped on regression. Callers MUST treat Valid=false
	// as "no data"; never default to 0 silently — the
	// schedd-side adapter stamps Unknown and skips.
	Valid bool
}

type lastSample struct {
	rx              uint64
	at              time.Time
	accumCumulative uint64
	lastDelta       uint64
}

// Cache is a per-instance byte-counter cache. Construct with New
// (testable, injectable clock) or NewWithDefaults (production
// vmmd wiring).
type Cache struct {
	mu   sync.Mutex
	last map[string]lastSample
	now  func() time.Time
}

// New returns a Cache. now is consulted on every Observe; pass
// nil to use time.Now.
func New(now func() time.Time) *Cache {
	if now == nil {
		now = time.Now
	}
	return &Cache{last: make(map[string]lastSample), now: now}
}

// NewWithDefaults returns a Cache that uses time.Now. Intended
// for cmd/vmmd wiring.
func NewWithDefaults() *Cache { return New(nil) }

// Observe records one /sys/class/net/<vethHost>/statistics/rx_bytes
// reading for the given instance and returns the resulting
// Reading. ok is false on the first observation for a given
// instance (baseline) and on an rx_bytes regression; in either
// case the previous baseline is reset so the next Observe returns
// a real reading.
//
// now is the wall-clock moment of the reading. Callers that batch
// readings from a single sample loop should pass a single now
// across the batch so the per-instance deltas are computed
// against the same instant — otherwise the per-tick deltas drift
// with the loop's iteration order.
//
// Reading.DeltaBytes is always 0 when ok=false. The cumulative
// counter preserved across regressions is the billing seam; the
// per-tick delta is what the meterd sampler appends to
// usage_minutes.net_tx_bytes additively each minute.
func (c *Cache) Observe(o Observation) (Reading, bool) {
	if o.InstanceID == "" {
		return Reading{Valid: false}, false
	}
	now := o.At
	if now.IsZero() {
		now = c.now()
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	prev, hadPrev := c.last[o.InstanceID]
	// First observation: just record the baseline; no delta yet.
	if !hadPrev {
		c.last[o.InstanceID] = lastSample{rx: o.RXBytes, at: now}
		return Reading{Valid: false}, false
	}
	// Regression: veth recreation (instance teardown + new
	// vethHost). Drop baseline, start fresh. Mirrors
	// pkg/fcvm/cpustats.Cache.Observe exactly — the kernel
	// counter starts over, and the previous interval's egress
	// is not patched across the break.
	if o.RXBytes < prev.rx {
		c.last[o.InstanceID] = lastSample{rx: o.RXBytes, at: now}
		return Reading{Valid: false}, false
	}
	// No wall-clock progression: degenerate. Keep the previous
	// accumulator and return Valid=false so callers don't divide
	// by zero or claim a delta of zero.
	if !now.After(prev.at) {
		return Reading{CumulativeBytes: prev.accumCumulative, Valid: false}, false
	}
	delta := o.RXBytes - prev.rx
	accum := prev.accumCumulative + delta
	c.last[o.InstanceID] = lastSample{
		rx:              o.RXBytes,
		at:              now,
		accumCumulative: accum,
		lastDelta:       delta,
	}
	return Reading{
		DeltaBytes:      delta,
		CumulativeBytes: accum,
		Valid:           true,
	}, true
}

// Forget drops the cached baseline for an instance. Call this
// when vmmd tears down an instance so the map does not grow
// unbounded across the vmmd process lifetime.
func (c *Cache) Forget(instanceID string) {
	c.mu.Lock()
	delete(c.last, instanceID)
	c.mu.Unlock()
}

// Reset wipes all baselines. Used by tests and on vmmd restart
// (the in-memory state is gone anyway, but Reset is the documented
// seam for any future "drop everything" admin operation).
func (c *Cache) Reset() {
	c.mu.Lock()
	c.last = make(map[string]lastSample)
	c.mu.Unlock()
}

// Lookup returns the most recent Reading for an instance without
// advancing the baseline. ok=false means the cache has no baseline
// for the instance (first sample, regression, or forgotten). The
// returned Reading's Valid field mirrors ok semantically; callers
// should check both.
//
// The Reading.DeltaBytes is the delta computed at the most recent
// Observe (i.e. the byte delta for the most recent ~250 ms window
// of sysfs activity). It is not recomputed at Lookup time — that
// would require a fresh sysfs read and would put I/O back on the
// hot path the cache was designed to avoid. schedd's 250 ms poller
// will pull a fresh value at most 250 ms after the next sample
// loop tick.
func (c *Cache) Lookup(instanceID string) (Reading, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	prev, ok := c.last[instanceID]
	if !ok {
		return Reading{Valid: false}, false
	}
	return Reading{
		DeltaBytes:      prev.lastDelta,
		CumulativeBytes: prev.accumCumulative,
		Valid:           true,
	}, true
}

// Size returns the number of instances currently tracked. For
// diagnostics and tests; not used on the hot path.
func (c *Cache) Size() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.last)
}
