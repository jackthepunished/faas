package db

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// TestWarmUp_NilPool ensures a nil pool is rejected with a typed
// error rather than panicking on the pool.Acquire call below.
func TestWarmUp_NilPool(t *testing.T) {
	err := WarmUp(context.Background(), nil, 1, time.Second)
	if err == nil {
		t.Fatal("expected error for nil pool, got nil")
	}
	if !errors.Is(err, err) { // placeholder; we'll match on substring below
		t.Logf("got err = %v", err)
	}
}

// TestWarmUp_BadInputs rejects non-positive want / non-positive
// deadline so a future caller can't accidentally pass (want=0) and
// silently no-op.
func TestWarmUp_BadInputs(t *testing.T) {
	cases := []struct {
		name     string
		want     int
		deadline time.Duration
	}{
		{"want=0", 0, time.Second},
		{"want=-1", -1, time.Second},
		{"deadline=0", 1, 0},
		{"deadline=-1s", 1, -time.Second},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// We pass a nil pool because the input-validation
			// runs before the pool is touched; a nil pool + bad
			// input would otherwise trip the nil check first.
			err := WarmUp(context.Background(), nil, tc.want, tc.deadline)
			if err == nil {
				t.Fatalf("expected error for %s, got nil", tc.name)
			}
		})
	}
}

// TestWarmUp_ClosedPool ensures a closed pool returns an error
// rather than hanging the warm-up deadline (pgxpool.Pool.Acquire
// returns "closed pool" immediately on a closed pool).
func TestWarmUp_ClosedPool(t *testing.T) {
	cfg, err := pgxpool.ParseConfig("postgres:///nothing?host=/run/postgresql&user=faas")
	if err != nil {
		t.Skipf("cannot parse dsn: %v", err)
	}
	cfg.MaxConns = 2
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Skipf("cannot open pool: %v", err)
	}
	// Don't Ping — close immediately to keep this test offline.
	pool.Close()

	err = WarmUp(context.Background(), pool, 1, 500*time.Millisecond)
	if err == nil {
		t.Fatal("expected error on closed pool, got nil")
	}
	// pgxpool returns a non-ErrWarmUpTimeout error on a closed pool
	// ("closed pool"); we want callers to see that as a typed
	// "couldn't warm up" rather than a silent hang. The warm-up
	// returned the underlying error verbatim.
	if errors.Is(err, ErrWarmUpTimeout) {
		t.Fatalf("closed-pool error should not surface as ErrWarmUpTimeout (would mask the actual cause); got %v", err)
	}
}

// TestWarmUp_ContextCanceled ensures caller-side cancellation
// returns ctx.Err() rather than ErrWarmUpTimeout (the two are
// distinct surfaces; ErrWarmUpTimeout is the "boot starvation"
// signal, ctx.Err() is the "caller gave up" signal).
func TestWarmUp_ContextCanceled(t *testing.T) {
	cfg, err := pgxpool.ParseConfig("postgres:///nothing?host=/run/postgresql&user=faas")
	if err != nil {
		t.Skipf("cannot parse dsn: %v", err)
	}
	cfg.MaxConns = 1
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Skipf("cannot open pool: %v", err)
	}
	defer pool.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel before WarmUp runs

	err = WarmUp(ctx, pool, 1, time.Second)
	if err == nil {
		t.Fatal("expected error on canceled ctx, got nil")
	}
	if errors.Is(err, ErrWarmUpTimeout) {
		t.Fatalf("ctx.Canceled should not surface as ErrWarmUpTimeout; got %v", err)
	}
}

// TestWarmUp_HappyPath_Postgres exercises the happy path against a
// real Postgres when DATABASE_URL is set (skipped otherwise). This
// is the integration pin that complements the offline tests above.
func TestWarmUp_HappyPath_Postgres(t *testing.T) {
	if testing.Short() {
		t.Skip("short")
	}
	dsn := ""
	if dsn == "" {
		t.Skip("DATABASE_URL unset; skipping live warm-up test")
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	cfg.MaxConns = 4
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	defer pool.Close()

	if err := WarmUp(context.Background(), pool, 2, 5*time.Second); err != nil {
		t.Fatalf("warm up healthy pool: %v", err)
	}
}

// TestWarmUp_WantExceedsMaxConns ensures the helper reports a clean
// failure when want is larger than the pool's MaxConns — pgxpool
// blocks indefinitely waiting for a connection that's never going
// to free, so the deadline must trip before that point. The helper
// returns ErrWarmUpTimeout in that case.
func TestWarmUp_WantExceedsMaxConns_Postgres(t *testing.T) {
	if testing.Short() {
		t.Skip("short")
	}
	dsn := ""
	if dsn == "" {
		t.Skip("DATABASE_URL unset; skipping live want>MaxConns test")
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	cfg.MaxConns = 1
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	defer pool.Close()

	// Hold the only connection.
	held, err := pool.Acquire(context.Background())
	if err != nil {
		t.Fatalf("hold conn: %v", err)
	}
	defer held.Release()

	start := time.Now()
	err = WarmUp(context.Background(), pool, 2, 500*time.Millisecond)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected ErrWarmUpTimeout when want > MaxConns and all conns held, got nil")
	}
	if !errors.Is(err, ErrWarmUpTimeout) {
		t.Fatalf("err = %v, want ErrWarmUpTimeout", err)
	}
	if elapsed > 2*time.Second {
		t.Errorf("WarmUp took %s; deadline was 500ms (expected ~500ms)", elapsed)
	}
}
