// In-process GC for the webhookdedupe dedupe state. The dedupe is
// a process-local sync.Map; the sweeper walks it every DefaultSweepInterval
// and removes entries whose expires_at has passed. Without this
// goroutine the map would only grow for the lifetime of the daemon —
// the sweep bounds the memory footprint to ~(deliveries in last TTL).

package webhookdedupe

import (
	"context"
	"sync/atomic"
	"time"
)

// DefaultSweepInterval is the cadence at which the sweeper walks
// the dedupe state. 60s matches the meterd dunning sweep cadence
// (pkg/meter/dunning.go); the dedupe map is much smaller than
// the dunning work, so the same cadence is fine.
const DefaultSweepInterval = 60 * time.Second

// Sweeper is the apid-side GC goroutine for the dedupe state.
// The zero value is invalid; use NewSweeper.
type Sweeper struct {
	interval time.Duration
	now      func() time.Time
}

// NewSweeper returns a sweeper that ticks at the given interval.
// interval <= 0 falls back to DefaultSweepInterval. The now
// field is left at time.Now; tests can override it via
// setSweeperNowForTest.
func NewSweeper(interval time.Duration) *Sweeper {
	if interval <= 0 {
		interval = DefaultSweepInterval
	}
	return &Sweeper{interval: interval, now: time.Now}
}

// RunOnce walks the dedupe state and removes entries whose
// expires_at is at or before the sweeper's now(). Returns the
// number of entries removed. The walk is O(N); the map is
// expected to be small (deliveries in the last 5 minutes per
// provider). A nil receiver is a no-op so misconfigured boot
// paths don't panic.
func (s *Sweeper) RunOnce() int64 {
	if s == nil {
		return 0
	}
	var removed int64
	now := s.now()
	cutoff := now.Add(-TTL)
	store.Range(func(k, v any) bool {
		exp, ok := v.(time.Time)
		if !ok || !exp.After(cutoff) {
			store.Delete(k)
			atomic.AddInt64(&removed, 1)
		}
		return true
	})
	return removed
}

// Run blocks on the ticker + ctx.Done, returning ctx.Err() on
// cancellation. Stops cleanly on context cancel; the daemon
// shutdown path relies on the ctx cancel to tear the goroutine
// down. A nil receiver is a no-op so misconfigured boot paths
// don't panic.
func (s *Sweeper) Run(ctx context.Context) error {
	if s == nil {
		return nil
	}
	t := time.NewTicker(s.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			s.RunOnce()
		}
	}
}
