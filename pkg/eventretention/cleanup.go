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
//
// ADR-091 D20.3 / PR-B residual: RunOnce observes three wire metrics
// (deleted counter, retention-lag gauge, kind-prefix volume counter)
// so an operator can see "is the prune loop running AND is it making
// progress" without grepping logs. The Ops interface is narrow so
// tests can pass a stub that records increments without spinning up
// a real Prometheus registry.
package eventretention

import (
	"context"
	"log/slog"
	"time"

	"github.com/prometheus/client_golang/prometheus"
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

// Ops is the narrow counter+gauge surface the cleanup loop
// observes at the end of every RunOnce pass. The concrete
// *wire.OpsMetrics satisfies it (apid wires one up via
// WithOpsMetrics). Defined as an interface so tests can pass a
// stub that records increments without spinning up a real
// Prometheus registry.
//
// Mirrors pkg/audit.Ops: each method returns the underlying
// Prometheus object so the call site decides whether and how to
// mutate it. Lets tests substitute a no-op Observer / Counter
// without spinning up a real registry.
//
// ADR-091 D20.3 / PR-B residual:
//
//   - AuditEventsDeleted returns the per-run counter the loop
//     increments with .Add(float64(n)). RunOnce only calls Add
//     when deleted > 0 to avoid the counter ticking up on idle
//     days.
//
//   - AuditEventsRetentionLag returns the per-run gauge the loop
//     sets to (now − cutoff). Always fired at the end of a
//     successful pass so a pinned-zero value is the canary for
//     "the loop is running but never deleting anything".
//
// The per-kind-prefix emit counter (apid_audit_events_volume_total
// {kind_prefix}) is NOT in this interface — that counter is
// incremented by the emit hot path (pkg/audit.Auditor), not by
// the retention loop. The volume counter lives on a separate
// emit-side interface so the two concerns stay decoupled.
type Ops interface {
	AuditEventsDeleted() prometheus.Counter
	AuditEventsRetentionLag() prometheus.Gauge
}

// kindPrefixFromKind is intentionally NOT defined here — the kind
// prefix mapping lives on the emit side (pkg/audit.Auditor) so
// RunOnce doesn't have to know about audit-event kind namespaces.
// The pre-instantiated label set in pkg/wire/metrics.go is the
// single source of truth for what prefix strings exist.

// Params is the runtime configuration for the cleanup loop. The
// only required field is Store; Interval / Log / Now / Ops are
// optional. CutoffDays ≤ 0 falls back to DefaultCutoffDays (90).
type Params struct {
	Store      DeleteOldEventsStore
	Interval   time.Duration
	CutoffDays int
	Log        *slog.Logger
	// Now is the clock function. nil defaults to time.Now. Tests
	// pass a fixed time so the cutoff is deterministic.
	Now func() time.Time
	// Ops is the metrics surface (ADR-091 D20.3). nil disables
	// the metric path so unit tests don't need a registry. The
	// production wiring in cmd/apid passes WithOpsMetrics.
	Ops Ops
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
	ops        Ops
}

// SetOps replaces the Ops interface after construction. Mirrors
// pkg/audit.Auditor.SetOps — used by cmd/apid where the cleanup
// loop starts before the Prometheus registry is wired (the
// goroutine outlives the construction order). Pass nil to
// disable the metric path. Safe to call from any goroutine before
// the first RunOnce fires.
func (c *Cleanup) SetOps(ops Ops) {
	c.ops = ops
}

// New returns a Cleanup wired to the given Store. Panics if Store
// is nil — the loop has no useful work without it. Interval ≤ 0
// falls back to 24h; CutoffDays ≤ 0 falls back to DefaultCutoffDays.
// nil Log falls back to slog.Default(); nil Now falls back to
// time.Now. nil Ops disables the metric path (unit tests).
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
		ops:        p.Ops,
	}
}

// RunOnce deletes every event row whose `at` is older than the
// configured cutoff. Returns the row count. Errors propagate;
// the caller (either Run's ticker or the test) decides what to
// do with them. On success, observes the per-pass metrics:
//
//   - AuditEventsDeleted: only fired when deleted > 0 (avoid the
//     counter ticking up on idle days).
//   - AuditEventsRetentionLag: always fired (so a pinned-zero
//     value is the canary for "loop running but never deleting").
func (c *Cleanup) RunOnce(ctx context.Context) (int64, error) {
	now := c.now()
	cutoff := now.AddDate(0, 0, -c.cutoffDays)
	deleted, err := c.store.DeleteOldEvents(ctx, cutoff)
	if err != nil {
		c.log.Error("eventretention.delete", "err", err, "cutoff", cutoff)
		return 0, err
	}
	if deleted > 0 {
		c.log.Info("eventretention.deleted", "rows", deleted, "cutoff", cutoff)
		if c.ops != nil {
			c.ops.AuditEventsDeleted().Add(float64(deleted))
		}
	}
	if c.ops != nil {
		c.ops.AuditEventsRetentionLag().Set(now.Sub(cutoff).Seconds())
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
