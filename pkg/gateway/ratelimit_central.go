// Package gateway — central-mode rate-limit backend seam (ADR-104
// amendment 5, issue #881 Phase 4).
//
// The pre-Phase-4 Limiter (pkg/gateway/ratelimit.go) is in-process:
// every gatewayd-internal replica owns a private bucket map and the
// 00126 pg_ratelimit_counters table exists but has zero Go readers.
// On a multi-replica Tier-A7 cluster, sticky-by-warm-node routing
// (ADR-070) does NOT pin a single replica, so per-process buckets
// see a fraction of customer traffic and the limit leaks.
//
// CentralBackend is the seam that closes the leak. The fast-path
// pattern (cf. PGNodeVerifier at pkg/wire/pgverifier.go):
//
//   1. The in-process Limiter continues to serve hot-path Peek
//      (the response-header writer X-RateLimit-* is read from the
//      local map — sub-1ms, zero PG round-trips).
//   2. On Allow returning false (local would reject), the limiter
//      consults central.PeekToken. If central still has tokens, the
//      request is admitted anyway. This bounds PG round-trips to the
//      local-would-reject boundary case only — typically < 1% of
//      admits under normal load.
//   3. On every admit, central.ConsumeToken decrements the central
//      counter (single SQL statement, pg_advisory_xact_lock on the
//      (scope, subject_id, plan) tuple).
//   4. Peers invalidate their in-process cache on every
//      'rate_limit_changed' pg_notify tick (see
//      pkg/wire/pgratelimit_invalidator.go).
//
// Phase 4 covers per-app + per-account + per-rule scopes (the 00281
// migration widened the 00126 CHECK to include scope='rule'). Per-
// consumer central mode is Phase 5: the PK (scope, subject_id, plan)
// does NOT include consumer_id; the __other__ collapse bucket stays
// in-process until Phase 5.

package gateway

import "context"

// CentralBackend is the production-side seam that allows a Limiter
// to coordinate across N gatewayd-internal replicas via a shared
// Postgres counter row (pg_ratelimit_counters, migration 00126,
// widened by migration 00281 to include scope='rule').
//
// Implementations:
//
//   - noopCentralBackend{} (default; matches today's behaviour
//     byte-for-byte; the Limiter never consults Postgres).
//   - state.PGRateLimitBackend (C3 of the Phase 4 mega-PR cluster;
//     wired iff [ratelimit] mode = "central" in the daemon TOML).
//
// ConsumeToken / PeekToken signatures use the same (scope, subject_id,
// plan) key triple as the central SQL row; cost is always 1 token per
// Allow call (the limiter math is fixed). Future per-request-cost
// variations (e.g. byte-weighted limits) would extend the signature
// without breaking the closed set of existing call sites.
type CentralBackend interface {
	// ConsumeToken attempts to consume one token from the
	// central counter for (scope, subject_id, plan). Returns:
	//
	//   remaining int  — tokens remaining AFTER the consume
	//                    (>= 0; the consume always succeeds or
	//                    never happened — there is no negative
	//                    balance on the wire)
	//   ok bool        — true iff the consume succeeded (i.e.,
	//                    remaining tokens >= 0 after refill + -1)
	//   err error      — non-nil iff Postgres was unreachable or
	//                    the advisory lock deadlocked; the caller
	//                    MUST fall back to the in-process bucket
	//                    in degraded mode (ADR-070 bench follow-up)
	//
	// Single SQL statement (no separate transaction); serialises
	// contending replicas via pg_advisory_xact_lock on the
	// hashtext of (scope, subject_id, plan).
	ConsumeToken(ctx context.Context, scope, subjectID, plan string) (remaining int, ok bool, err error)

	// PeekToken returns the central counter's current tokens
	// for (scope, subject_id, plan) WITHOUT decrementing. Used
	// by the fast-path-cache's boundary-case consult — the
	// limiter calls PeekToken only when its in-process bucket
	// would reject, to check whether the central counter has
	// refilled enough to admit anyway.
	//
	// Returns:
	//   remaining int  — tokens currently available centrally
	//                    (>= 0; the row's tokens column)
	//   err error      — non-nil iff Postgres was unreachable;
	//                    the caller MUST fall back to the
	//                    in-process bucket.
	PeekToken(ctx context.Context, scope, subjectID, plan string) (remaining int, err error)

	// Invalidate drops any in-process cache entry for
	// (scope, subject_id, plan). Called by the LISTEN-side
	// 'rate_limit_changed' pg_notify consumer (pkg/wire) when a
	// peer replica writes to the counter; the next Allow call
	// repopulates the entry via PeekToken.
	Invalidate(scope, subjectID, plan string)
}

// noopCentralBackend is the default CentralBackend — it never
// reaches Postgres, so the Limiter's behaviour is identical to the
// pre-Phase-4 in-process map (ADR-104 amendment 4 wording: "central
// mode was rejected; in-process bucket is sufficient for single-box
// deployments"). The default is what the existing NewLimiter /
// NewLimiterWithLRU constructors wire; C3 of the mega-PR cluster
// adds NewLimiterWithCentral that swaps in the production
// implementation iff Mode = "central".
type noopCentralBackend struct{}

// Compile-time interface check.
var _ CentralBackend = noopCentralBackend{}

// ConsumeToken on a noop backend returns (0, true, nil) — the
// limiter treats "noop" as "the central counter is infinite" which
// is the conservative answer under degraded posture: the in-process
// bucket is the only source of truth, and a noop answer never
// flips a local-allow to a local-reject.
func (noopCentralBackend) ConsumeToken(context.Context, string, string, string) (int, bool, error) {
	return 0, true, nil
}

// PeekToken on a noop backend returns (0, nil) — the limiter's
// fast-path consults central only when local would reject, and
// "no answer from central" is treated as "central says admit" by
// convention (cf. ConsumeToken above).
func (noopCentralBackend) PeekToken(context.Context, string, string, string) (int, error) {
	return 0, nil
}

// Invalidate on a noop backend is a no-op — there is no in-process
// cache for the central counter to invalidate.
func (noopCentralBackend) Invalidate(string, string, string) {}
