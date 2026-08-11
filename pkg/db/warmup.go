package db

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrWarmUpTimeout is returned by WarmUp when the requested number of
// connections couldn't be acquired before the deadline elapsed. The
// caller is expected to treat this as a fatal boot condition (the
// pool is too starved for the daemon's expected concurrent load) —
// failing fast at boot is preferable to mysterious post-bind
// "closed pool" failures later.
//
// The error is distinct from context.DeadlineExceeded so daemons can
// log it with a stable, typed shape (pool.warmup_failed) for metrics /
// alerting rather than as a generic deadline.
var ErrWarmUpTimeout = errors.New("db: warm up timeout")

// WarmUp acquires (and immediately releases) want connections from
// pool within deadline, sequentially. This is the boot-time
// pre-flight that proves the pool can serve N parallel connections
// before the daemon starts launching its background goroutines —
// solving the post-bind "closed pool" failure mode that hit
// cmd/apid's TestRekeyRunnerPg e2e across PR #823 (where 8+
// bgBefore goroutines simultaneously called pool.Acquire on a
// MaxConns=8 pool; the first-Subscribe errors in any one goroutine
// surfaced as "closed pool" inside the others).
//
//	acquired = 0
//	for acquired < want:
//	    conn, err := pool.Acquire(ctx)
//	    if err is ErrWarmUpTimeout-shaped: return ErrWarmUpTimeout
//	    acquired++
//	    defer conn.Release() — released on WarmUp return
//
// WarmUp is fail-CLOSED by default: if it can't acquire want
// connections within the deadline, the daemon should refuse to
// start. This matches apid's "control-plane daemon refuses to start
// under misconfigured boot" stance (pkg/role.Require, the audit-HMAC
// loaders, the recovery-HMAC loader's no-zero-key fallback). The
// want parameter is daemon-agnostic; callers pass the expected
// concurrent-acquire fan-out at boot (apid uses 4 — see cmd/apid/main.go).
//
// The deadline is bounded by ctx; on ctx cancellation WarmUp returns
// ctx.Err(). Pool-pointer nil and deadline<=0 are pre-checked
// errors (the deadline<=0 check is the sentinel that prevents an
// accidental zero-duration deadline from being interpreted as
// "as fast as possible").
//
// Use:
//   - cmd/apid (and any future daemon with > MaxConns/2 bgBefore
//     goroutines) calls WarmUp between db.Open and bgBefore launch.
//   - The test helper pkg/db/warmup_test.go exercises the nil-pool,
//     closed-pool, slow-pool, healthy-pool, and want>MaxConns cases
//     so a future refactor that breaks the contract trips a unit
//     test.
//
// ADR-094: pgxpool boot warm-up barrier. Pinning the contract here
// (and at the architecture-test layer in
// pkg/db/warmup_architecture_test.go) prevents a future contributor
// from silently reintroducing the race by removing the WarmUp call.
func WarmUp(ctx context.Context, pool *pgxpool.Pool, want int, deadline time.Duration) error {
	if pool == nil {
		return fmt.Errorf("db: WarmUp: nil pool")
	}
	if want <= 0 {
		return fmt.Errorf("db: WarmUp: want %d must be positive", want)
	}
	if deadline <= 0 {
		return fmt.Errorf("db: WarmUp: deadline %s must be positive", deadline)
	}

	// Hold the acquired connections in a slice so they're released
	// together on WarmUp return. Each conn is an *pgxpool.Conn; the
	// returned error from pool.Acquire(ctx) is the pgxpool-flavoured
	// "closed pool" / "context canceled" / timeout shape — we
	// distinguish "got none, ran out of time" from "got some, but
	// not enough" by the count at exit.
	conns := make([]*pgxpool.Conn, 0, want)
	releaseAll := func() {
		for _, c := range conns {
			c.Release()
		}
	}

	// Bound the parent ctx by deadline so a caller that passes a
	// long-lived ctx still trips the warm-up boundary on time. The
	// caller's ctx is preserved so a parent cancel still returns
	// ctx.Err() (the deadline is the "couldn't acquire N" path;
	// ctx.Err() is the "caller gave up" path).
	warmCtx, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()

	for len(conns) < want {
		conn, err := pool.Acquire(warmCtx)
		if err != nil {
			releaseAll()
			// Distinguish "ran out of time" from "caller cancelled".
			// pgxpool returns context.DeadlineExceeded when warmCtx
			// expires mid-Acquire; ctx.Err() on the bounded warmCtx
			// is DeadlineExceeded, which is the canonical signal.
			if errors.Is(warmCtx.Err(), context.DeadlineExceeded) {
				// errors.Join keeps both ErrWarmUpTimeout AND the
				// underlying pgxpool acquire error in the chain
				// so callers can errors.Is against either sentinel
				// (the pool-starvation vs context-canceled surfaces
				// are distinct). The leading message carries the
				// "what to do next" hint that the operator needs.
				return errors.Join(
					fmt.Errorf("%w: acquired %d/%d connections in %s (pool starvation — increase MaxConns or reduce concurrent goroutines)",
						ErrWarmUpTimeout, len(conns), want, deadline),
					err,
				)
			}
			if errors.Is(warmCtx.Err(), context.Canceled) {
				return fmt.Errorf("db: WarmUp: canceled: %w", warmCtx.Err())
			}
			// Any other error (most commonly "closed pool" when
			// the caller passed an already-closed pool) is
			// surfaced verbatim — it's not a starvation case.
			return fmt.Errorf("db: WarmUp: acquire %d/%d: %w", len(conns)+1, want, err)
		}
		conns = append(conns, conn)
	}
	releaseAll()
	return nil
}
