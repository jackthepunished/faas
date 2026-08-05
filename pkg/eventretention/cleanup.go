// Package eventretention runs the daily cleanup of the events table
// (IAM-4 audit log, ADR-035). The audit pipeline (issue #286's
// failed-login emission, the auth/key/secret/account/stateless
// namespaces, and the future wake-timeline / sidecar surfaces)
// keeps appending rows; without a retention trim the table grows
// ~3-4 GB/year per active-tier customer. The cleanup loop
// deletes rows older than 90 days, the SOC 2 CC6.2
// evidence-retention floor.
//
// This is a deliberate sibling of pkg/logintoken and pkg/grace —
// same shape, same Run(ctx) / RunOnce() pattern, same test driving
// style. RunOnce is exported so tests walk the loop deterministically
// without spinning up a real ticker.
//
// ADR-075 lands this package; the previous "Out of scope" bullet
// in ADR-035 §"Out of scope" is removed in the same PR.
package eventretention

import (
	"context"
	"log/slog"
	"time"
)

// DefaultCutoffDays is the retention floor (SOC 2 CC6.2).
const DefaultCutoffDays = 90

// DeleteOldEventsStore is the narrow surface this loop needs from
// state.Store. Keeping the interface narrow means tests can
// implement only DeleteOldEvents without stubbing the dozens of
// other Store methods. Production code passes a *state.PgStore
// (or *state.MemStore); the assignment is implicit because
// PgStore / MemStore implement DeleteOldEvents.
type DeleteOldEventsStore interface {
	DeleteOldEvents(ctx context.Context, before time.Time) (int64, error)
}

// Params is the runtime configuration for the cleanup loop. The
// only required field is Store; Interval / Log / Now are optional.
// CutoffDays ≤ 0 falls back to DefaultCutoffDays (90).
type Params struct {
	Store      DeleteOldEventsStore
	Interval   time.Duration
	CutoffDays int
	Log        *slog.Logger
	// Now is the clock function. nil defaults to time.Now. Tests
	// pass a fixed time so the cutoff is deterministic.
	Now func() time.Time
}

// Cleanup is the loop driver. Construct with New; drive with Run
// (blocks until ctx is cancelled) or RunOnce (one pass, returns the
// rows deleted).
type Cleanup struct {
	store      DeleteOldEventsStore
	interval   time.Duration
	cutoffDays int
	log        *slog.Logger
	now        func() time.Time
}

// New returns a Cleanup wired to the given Store. Panics if Store
// is nil — the loop has no useful work without it. Interval ≤ 0
// falls back to 24h; CutoffDays ≤ 0 falls back to DefaultCutoffDays.
// nil Log falls back to slog.Default(); nil Now falls back to
// time.Now.
func New(p Params) *Cleanup {
	if p.Store == nil {
		panic("eventretention: New requires a non-nil Store")
	}
	if p.Interval <= 0 {
		p.Interval = 24 * time.Hour
	}
	if p.CutoffDays <= 0 {
		p.CutoffDays = DefaultCutoffDays
	}
	if p.Log == nil {
		p.Log = slog.Default()
	}
	if p.Now == nil {
		p.Now = time.Now
	}
	return &Cleanup{
		store:      p.Store,
		interval:   p.Interval,
		cutoffDays: p.CutoffDays,
		log:        p.Log,
		now:        p.Now,
	}
}

// RunOnce deletes every event row whose `at` is older than the
// configured cutoff. Returns the row count. Errors propagate;
// the caller (either Run's ticker or the test) decides what to
// do with them.
func (c *Cleanup) RunOnce(ctx context.Context) (int64, error) {
	cutoff := c.now().AddDate(0, 0, -c.cutoffDays)
	deleted, err := c.store.DeleteOldEvents(ctx, cutoff)
	if err != nil {
		c.log.Error("eventretention.delete", "err", err, "cutoff", cutoff)
		return 0, err
	}
	if deleted > 0 {
		c.log.Info("eventretention.deleted", "rows", deleted, "cutoff", cutoff)
	}
	return deleted, nil
}

// Run drives the cleanup loop on a fixed Interval. The first pass
// runs immediately (so a daemon restart catches up); subsequent
// passes tick on the interval. Returns nil on graceful ctx cancel.
//
// Mirrors pkg/logintoken.Run exactly (same defence-in-depth on the
// first-pass error: log + continue rather than crash the daemon).
func (c *Cleanup) Run(ctx context.Context) error {
	if _, err := c.RunOnce(ctx); err != nil && ctx.Err() == nil {
		c.log.Warn("eventretention.first_pass_failed", "err", err)
	}
	ticker := time.NewTicker(c.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			_, _ = c.RunOnce(ctx)
		}
	}
}
