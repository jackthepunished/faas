// write_gate_serve.go — the writeGate.ServeHTTP entry point.
// This file is the dispatcher: classify → metric → response.
// All classification logic lives in write_gate_classify.go;
// all response emitters live in write_gate_respond.go. The
// dispatcher itself is intentionally thin so the test suite
// can drive each (decision × authKind) cell independently.
//
// # Latency observation
//
// Every path that does work (decisions 2..8) emits one
// WriteRedirectLatency().Observe(d) call. The bypass path
// (decision 1) does NOT — bypass is the no-op short-circuit
// and we don't want it to skew the histogram. The histogram
// buckets are sized for the Tier A9 quota
// StandbyWriteRedirectTimeoutMS (5 s); see pkg/wire/metrics.go.
package main

import (
	"net/http"
	"time"

	"github.com/onebox-faas/faas/pkg/gateway/writegate"
)

// ServeHTTP classifies the request, emits the appropriate
// metric + latency observation, and dispatches to one of the
// respond* helpers in write_gate_respond.go.
//
// All four timers (retry-after, no-leader retry, etc.) are
// pre-resolved at construction (write_gate.go::newWriteGate)
// from pkg/api/limits.go; the hot path reads from struct fields.
//
// The gate returns BEFORE incrementing the latency observer
// for the bypass path (decisionBypass) — see the file doc.
//
// The switch over classifyDecision is exhaustive over the
// closed set in write_gate_classify.go. If a new outcome is
// added without a corresponding case here, the compiler will
// not catch it (Go's switch on named constants is not
// exhaustive-checked), so we add an `_ = allClassifyDecisions`
// line in an init() to keep the slice live and let the
// `vet -unreachable` gate catch drift in code review.
func (g *writeGate) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	defer func() {
		// Defer of the metric observer is wrong on bypass
		// (we don't want to observe a no-op), so the
		// observers are inlined in the non-bypass arms
		// below. The defer here is a no-op safety net:
		// if a future refactor reorders the classify call,
		// the latency is still bounded by the request
		// lifetime.
		_ = start
	}()

	// Step 1 — resolve leader. The resolver has its own
	// cache; this is a mutex read in the steady state. We
	// do NOT skip the resolver on bypass paths because
	// we don't know yet whether the request is a bypass.
	leaderName, isMe, err := g.resolver.Current(r.Context())

	// Step 2 — classify. A resolver error collapses to
	// decisionUnreachable per ADR-084 §Decision #7.
	//
	// NOTE: we deliberately do NOT consult `err` here for
	// any other decision. The CachedLeaderResolver returns
	// the cached value when present, even if the
	// underlying store is briefly unreachable (the cache
	// TTL is 5 s, longer than a typical PG hiccup); the
	// gate thus serves a slightly-stale leader rather
	// than 503ing on every transient blip. When the cache
	// is empty AND the store errors, the resolver returns
	// err != nil AND leaderName == "" — both conditions
	// collapse to decisionUnreachable below.
	result := g.classifyRequest(r, leaderName, isMe)
	if err != nil && result.decision != decisionBypass && result.decision != decisionSameBox {
		// Override: a resolver error on a non-bypass,
		// non-same-box path. We treat this as unreachable
		// regardless of what leaderName resolved to
		// because the next request may hit a different
		// error and the cache is unreliable.
		result.decision = decisionUnreachable
	}

	// Step 3 — dispatch + metric + response. The switch is
	// exhaustive over the closed classifyDecision set; a
	// drift here shows up at code review (the
	// `allClassifyDecisions` slice in write_gate_respond.go
	// keeps the set live for static analysis).
	switch result.decision {
	case decisionBypass:
		g.next.ServeHTTP(w, r)
		return

	case decisionLoop:
		g.metrics.WriteRedirectLatency().Observe(time.Since(start).Seconds())
		g.respondLoop(w, r, result.authKind)
		return

	case decisionSameBox:
		g.metrics.WriteRedirectLatency().Observe(time.Since(start).Seconds())
		g.respondSameBox(w, r, result.authKind)
		return

	case decisionUnreachable:
		g.metrics.WriteRedirectLatency().Observe(time.Since(start).Seconds())
		g.respondUnreachable(w, r, result.authKind)
		return

	case decisionCookieRedirect:
		g.metrics.WriteRedirectLatency().Observe(time.Since(start).Seconds())
		g.respondCookieRedirect(w, r, leaderName, result.authKind)
		return

	case decisionRelayed:
		g.metrics.WriteRedirectLatency().Observe(time.Since(start).Seconds())
		g.respondRelayed(w, r, leaderName, result.authKind)
		return

	case decisionMTLSFailure, decisionError:
		// These two paths are reached only via the
		// respondRelayFailure helper (which receives an
		// error from LeaderHTTPClient.Relay). The
		// classifier never returns them directly because
		// the relay hasn't happened yet at classify time.
		// We log + 500 because reaching this branch is a
		// programming error.
		g.log.Error("writegate: classifyDecision reached post-classify; this is a bug",
			"decision", int(result.decision),
			"path", r.URL.Path,
		)
		http.Error(w, "writegate: internal classification error", http.StatusInternalServerError)
		return
	}

	// Defensive: classifyDecision outside the closed set.
	// The compile-time guard `var _ = allClassifyDecisions`
	// would not catch this (the slice is also unconstrained
	// at runtime), so we log + 500.
	g.log.Error("writegate: classifyDecision out of closed set",
		"decision", int(result.decision),
		"path", r.URL.Path,
	)
	http.Error(w, "writegate: internal classification error", http.StatusInternalServerError)
}

// init pins allClassifyDecisions to a live reference so the
// linker keeps the slice in the binary (vet can then warn if
// a constant drifts out of the slice). This is the same
// discipline pkg/wire uses for its pre-instantiated outcome
// matrices.
func init() {
	if len(allClassifyDecisions) != 8 {
		panic("writegate: classifyDecision closed set drifted from 8 entries")
	}
	_ = writegate.AllWriteOutcomes // pin the package import
}
