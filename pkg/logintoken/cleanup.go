// Package logintoken runs the daily cleanup of the login_tokens
// table that backs dashboard password-reset (issue #165 PR #2,
// ADR-032). The table is shared with the legacy magic-link flow
// that PR #1 removed; the only production caller is the
// password-reset hand-off in cmd/apid/handlers_auth_login.go. We
// keep the table tight (delete anything past 24h) so the per-account
// attack surface stays bounded.
//
// This is a deliberate sibling of pkg/grace — same shape, same
// Run(ctx) / RunOnce() pattern, same test driving style. RunOnce is
// exported so tests walk the loop deterministically without spinning
// up a real ticker.
package logintoken

import (
	"context"
	"log/slog"
	"time"

	"github.com/onebox-faas/faas/pkg/state"
)

// Params is the runtime configuration for the cleanup loop. The
// only required field is Store; Interval and Log are optional (the
// constructor falls back to 24h + slog.Default() when zero / nil).
type Params struct {
	Store    state.Store
	Interval time.Duration
	Log      *slog.Logger
}

// Cleanup is the loop driver. Construct with New; drive with Run
// (blocks until ctx is cancelled) or RunOnce (one pass, returns the
// rows deleted).
type Cleanup struct {
	store    state.Store
	interval time.Duration
	log      *slog.Logger
}

// New returns a Cleanup wired to the given Store. Panics if Store is
// nil — the loop has no useful work without it. Interval ≤ 0 falls
// back to 24h; nil Log falls back to slog.Default().
func New(p Params) *Cleanup {
	if p.Store == nil {
		panic("logintoken: New requires a non-nil Store")
	}
	if p.Interval <= 0 {
		p.Interval = 24 * time.Hour
	}
	if p.Log == nil {
		p.Log = slog.Default()
	}
	return &Cleanup{store: p.Store, interval: p.Interval, log: p.Log}
}

// RunOnce deletes every login_tokens row whose expires_at is past
// the cutoff. Returns the row count. Errors propagate; the caller
// (either Run's ticker or the test) decides what to do with them.
func (c *Cleanup) RunOnce(ctx context.Context) (int64, error) {
	cutoff := time.Now()
	deleted, err := c.store.DeleteOldLoginTokens(ctx, cutoff)
	if err != nil {
		c.log.Error("logintoken.delete", "err", err)
		return 0, err
	}
	if deleted > 0 {
		c.log.Info("logintoken.deleted", "rows", deleted)
	}
	return deleted, nil
}

// Run drives the cleanup loop on a fixed Interval. The first pass
// runs immediately (so a daemon restart catches up); subsequent
// passes tick on the interval. Returns nil on graceful ctx cancel.
func (c *Cleanup) Run(ctx context.Context) error {
	if _, err := c.RunOnce(ctx); err != nil && ctx.Err() == nil {
		// Surfacing the first-pass error here would crash the
		// daemon on bad DB connectivity. Log (RunOnce already
		// logged) and continue; the next tick will retry.
		c.log.Warn("logintoken.first_pass_failed", "err", err)
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
