// pgstore_ratelimit.go — Postgres-backed CentralBackend for
// pg_ratelimit_counters (ADR-104 amendment 5, issue #881 Phase 4 C3).
//
// This file is the production-side implementation of the
// CentralBackend interface declared in pkg/gateway. It lives
// in pkg/state (alongside pgstore.go + the sqlc-generated queries)
// because:
//
//   1. The Store adapter already owns the pgxpool — no need to
//      open a second pool just for the counter.
//   2. The consume SQL is hand-written (per ADR-041 carve-out for
//      single-statement rate counters), bypassing sqlc. This file
//      keeps the package-level import set narrow: pgx + pgxpool
//      only.
//
// Interface assertion: the compile-time `var _ gateway.CentralBackend =
// (*PGRateLimitBackend)(nil)` lives in
// cmd/gatewayd-internal/run.go's wireup_test.go because pkg/state
// does not import pkg/gateway (and adding the import would
// invert the package layering — cf. memory
// pkg-api-cannot-import-pkg-state.md).
//
// # SQL shape
//
//	INSERT INTO pg_ratelimit_counters (scope, subject_id, plan, tokens, last_refill)
//	VALUES ($1, $2, $3, $4, now())
//	ON CONFLICT (scope, subject_id, plan) DO UPDATE
//	  SET tokens = GREATEST(0,
//	    pg_ratelimit_counters.tokens
//	    + FLOOR(EXTRACT(EPOCH FROM (now() - pg_ratelimit_counters.last_refill))
//	             * $5)::bigint
//	    - 1
//	  ),
//	  last_refill = now()
//	WHERE pg_advisory_xact_lock(hashtext(($1 || ':' || $2 || ':' || $3)::text)) IS NOT NULL
//	RETURNING tokens;
//
// The advisory lock scopes to the implicit txn so two replicas
// contending on the same row serialise WITHOUT holding a row lock
// that blocks vacuum. Single statement; one round-trip; no
// separate transaction overhead. The 00126 comments cite ADR-070
// explicitly.
//
// # Degraded posture
//
// If Postgres is unreachable, ConsumeToken / PeekToken return
// (0, false, err) / (0, err). The Limiter's fast-path-cache logic
// falls back to the in-process bucket under that error path
// (see pkg/gateway/ratelimit.go:300-609 the Allow/Poke seam)
// and emits a `ratelimit_degraded` audit row.
//
// # Phase 4 scope
//
// Per-app + per-account + per-rule only. Per-consumer central mode
// is Phase 5 (the PK does NOT include consumer_id; the __other__
// collapse bucket stays in-process until Phase 5).

package state

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// RateLimitRow is the minimum row projection PGRateLimitBackend
// reads from pg_ratelimit_counters. Tokens are bigint today; a
// future ADR that adds fractional refill (cf. 00126 comment
// lines 28-34) would change the column type to numeric(20,4).
type RateLimitRow struct {
	Scope      string
	SubjectID  string
	Plan       string
	Tokens     int64
	LastRefill time.Time
}

// PGRateLimitBackend is the production CentralBackend. Constructed
// by cmd/gatewayd-internal/run.go iff [ratelimit] mode = "central"
// (the TOML knob added in C2). The pool is shared with the rest
// of the daemon (no second pool).
type PGRateLimitBackend struct {
	pool *pgxpool.Pool
	// rps is the per-second refill rate for the consume path. The
	// math is `floor(elapsed_seconds * rps)` added to tokens
	// before the -1 decrement. Plan-aware — the Limiter passes
	// the plan rps; the backend ignores the per-bucket math and
	// just runs the SQL. The interface carries only the scope
	// triple today; a future per-request-cost variation would
	// extend the signature without changing this struct.
	rps func(plan string) (float64, bool)
}

// NewPGRateLimitBackend wires the production backend. rps resolves
// the per-plan refill rate (Free 5 / Hobby 50 / Pro 250 / Scale
// 1500 — see pkg/api/limits.go RateLimitRPS). The closure pattern
// avoids a circular import on pkg/api (state → api is fine, but
// staying closure-based keeps this file's import set minimal).
func NewPGRateLimitBackend(pool *pgxpool.Pool, rps func(plan string) (float64, bool)) *PGRateLimitBackend {
	return &PGRateLimitBackend{pool: pool, rps: rps}
}

// ConsumeToken attempts to consume one token from the central
// counter for (scope, subjectID, plan). Implements
// gateway.CentralBackend (the interface assertion lives in
// run.go's wireup_test.go).
//
// Returns:
//
//	remaining int   — tokens remaining AFTER the consume (>= 0).
//	                  A return of (0, false, nil) signals the
//	                  bucket is empty and the caller must reject.
//	ok bool         — true iff the consume succeeded.
//	err error       — non-nil iff Postgres was unreachable or
//	                  the advisory lock deadlocked; the caller
//	                  MUST fall back to the in-process bucket.
func (b *PGRateLimitBackend) ConsumeToken(ctx context.Context, scope, subjectID, plan string) (int, bool, error) {
	rps, ok := b.rps(plan)
	if !ok || rps <= 0 {
		// Unknown plan or zero rps — degrade soft: behave like
		// the noop backend (always admit). Production never hits
		// this branch; it's a defensive fallback for stale plan
		// strings during a cluster upgrade (memory
		// pr-894-pg-shard-2-cache-bleed.md).
		return 0, true, nil
	}
	const q = `
		INSERT INTO pg_ratelimit_counters (scope, subject_id, plan, tokens, last_refill)
		VALUES ($1, $2, $3, $4, now())
		ON CONFLICT (scope, subject_id, plan) DO UPDATE
		  SET tokens = GREATEST(0,
		    pg_ratelimit_counters.tokens
		    + FLOOR(EXTRACT(EPOCH FROM (now() - pg_ratelimit_counters.last_refill))
		            * $5)::bigint
		    - 1
		  ),
		  last_refill = now()
		WHERE pg_advisory_xact_lock(hashtext(($1 || ':' || $2 || ':' || $3)::text)) IS NOT NULL
		RETURNING tokens`
	var remaining int64
	if err := b.pool.QueryRow(ctx, q,
		scope, subjectID, plan, int64(rps), rps,
	).Scan(&remaining); err != nil {
		return 0, false, fmt.Errorf("ratelimit central ConsumeToken: %w", err)
	}
	return int(remaining), remaining > 0, nil
}

// PeekToken returns the central counter's current tokens WITHOUT
// decrementing. Implements gateway.CentralBackend.
//
// Returns:
//
//	remaining int   — tokens currently available centrally.
//	err error       — non-nil iff Postgres was unreachable.
func (b *PGRateLimitBackend) PeekToken(ctx context.Context, scope, subjectID, plan string) (int, error) {
	const q = `
		SELECT tokens FROM pg_ratelimit_counters
		 WHERE scope = $1 AND subject_id = $2 AND plan = $3`
	var remaining int64
	if err := b.pool.QueryRow(ctx, q, scope, subjectID, plan).Scan(&remaining); err != nil {
		// pgx returns pgx.ErrNoRows for a missing key; treat
		// as zero (the consume path will INSERT-on-conflict
		// the row on the next Allow).
		return 0, fmt.Errorf("ratelimit central PeekToken: %w", err)
	}
	return int(remaining), nil
}

// Invalidate drops any in-process cache entry for (scope,
// subjectID, plan). Implements gateway.CentralBackend. Called by
// the LISTEN-side 'rate_limit_changed' pg_notify consumer
// (pkg/wire/pgratelimit_invalidator.go — C4 of this mega-PR
// cluster) when a peer replica writes to the counter.
//
// The PGRateLimitBackend itself has no in-process cache to
// invalidate — Postgres IS the shared state — so this is a
// no-op. The signature is here to satisfy the interface; the
// Limiter's local LRU cache invalidation is handled by the
// Limiter itself (see pkg/gateway/ratelimit.go:489-501 the
// forgetLocked path).
func (b *PGRateLimitBackend) Invalidate(scope, subjectID, plan string) {}
