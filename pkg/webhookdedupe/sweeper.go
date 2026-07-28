// apid-side sweep goroutine for webhook_deliveries (issue #294).
//
// Lives in pkg/webhookdedupe (not cmd/apid) so the test suite can
// exercise the lifecycle — RunOnce is exported for the deterministic
// test path, Run spins a ticker that calls RunOnce every Interval.
//
// Mirrors the shape of pkg/grace (G6 grace timer) and
// pkg/logintoken (login-token cleanup) so apid's main.go wiring is
// a one-line `go sweeper.Run(ctx)` next to the existing goroutines.
//
// gatewayd does NOT run a sweep — apid is the only writer for the
// Stripe + Paddle paths AND for the GitHub path (gatewayd inserts
// the row but apid sweeps it; a single sweep goroutine is simpler
// than per-provider sweeps and the partial index on
// webhook_deliveries_expires_idx keeps the DELETE cheap).
package webhookdedupe

import (
	"context"
	"log/slog"
	"time"

	"github.com/onebox-faas/faas/pkg/state"
)

// DefaultSweepInterval is the cadence Run uses when the caller does
// not specify one. 60s matches the meterd dunning sweep cadence
// (pkg/meter/dunning.go:223) — the webhook_deliveries table is much
// smaller than the dunning work, so the same cadence is fine.
const DefaultSweepInterval = 60 * time.Second

// Sweeper periodically calls state.Store.SweepExpiredWebhookDeliveries
// to keep the webhook_deliveries table bounded. Constructed once
// per apid process and driven from main.go via `go swp.Run(ctx)`.
type Sweeper struct {
	store    state.Store
	log      *slog.Logger
	interval time.Duration
	now      func() time.Time // clock seam for tests
}

// NewSweeper builds the goroutine. log + store must be non-nil.
// Pass interval = 0 to use DefaultSweepInterval.
func NewSweeper(store state.Store, log *slog.Logger, interval time.Duration) *Sweeper {
	if interval <= 0 {
		interval = DefaultSweepInterval
	}
	return &Sweeper{
		store:    store,
		log:      log,
		interval: interval,
		now:      time.Now,
	}
}

// RunOnce performs one sweep. Exported so tests can drive the
// sweep deterministically without spinning up a real ticker.
// Returns the rows deleted (informational; the caller does not
// gate on it).
func (s *Sweeper) RunOnce(ctx context.Context) (int64, error) {
	if s == nil || s.store == nil {
		return 0, nil
	}
	n, err := s.store.SweepExpiredWebhookDeliveries(ctx, s.now())
	if err != nil {
		if s.log != nil {
			s.log.Warn("webhookdedupe sweep error", "err", err)
		}
		return 0, err
	}
	if n > 0 && s.log != nil {
		s.log.Info("webhookdedupe sweep", "rows", n)
	}
	return n, nil
}

// Run blocks until ctx is cancelled, calling RunOnce every
// Interval. Errors are logged at WARN; the loop continues. The
// function returns ctx.Err() on cancellation.
//
// Mirrors the lifecycle shape of grace.New(...).Run(ctx) and
// logintoken.New(...).Run(ctx) so the apid main.go wiring is
// consistent with its sibling goroutines.
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
			if _, err := s.RunOnce(ctx); err != nil {
				// RunOnce already logs; loop continues.
			}
		}
	}
}