// write_gate.go — Tier A9 / ADR-084 standby write-redirect
// handler.
//
// This is the load-bearing piece of PR-B: every mutating apid
// request that arrives at a STANDBY node (i.e. a node where
// LeaderResolver.Current returns (name != localName, isMe=false))
// is either relayed to the leader over mTLS (bearer / anonymous)
// or 307-redirected to the leader's public URL (cookie). When
// the resolver returns no leader, the gate emits 503 with a
// 60-second Retry-After (the operator-visible "failover in
// progress" signal).
//
// # Why the gate sits in cmd/gatewayd-internal
//
// gatewayd-public is the TLS edge — it terminates customer
// traffic and forwards to gatewayd-internal over a unix
// socket (Tier A7 / ADR-070). gatewayd-internal owns the
// "is this box the leader?" decision because that's a
// control-plane concern (the resolver talks to PG, and PG
// access is a gatewayd-internal surface, not a per-request
// hot path). gatewayd-public is purely a TLS terminator; it
// never consults PG. So the gate sits in apidProxy after
// the logs-handler carve-out and before the apid proxyToApid
// dispatch (proxy.go:113-123).
//
// # 8-case decision tree
//
// The gate has exactly 8 paths; each emits exactly one
// `gatewayd_internal_write_redirect_total{outcome=...}`
// increment and (for paths that actually do work) one latency
// observation. The metric vocabulary is closed — see
// pkg/wire/metrics.go::writeRedirectOutcomes and the compile-
// time pinned test TestOpsMetrics_WriteRedirectPreinstantiated.
//
//  1. bypass    — read method / carve-out path / non-apid path.
//     Pass through to next. NO metric increment.
//  2. loop      — inbound X-Faas-Forwarded-Leader set. 400.
//  3. same_box  — resolver says I am the leader. Pass through.
//  4. unreachable — resolver says no leader. 503 + Retry-After: 60.
//  5. cookie_redirect — auth=cookie on standby. 307 to leader URL.
//  6. relayed   — auth=bearer/anonymous on standby. mTLS hop.
//  7. mTLS_failure — handshake / cert chain error. 503 + Retry-After: 5.
//  8. error     — catch-all (defensive). 503 + Retry-After: 5.
//
// Paths 1, 3 are the steady state on a healthy two-box
// fleet; path 6 dominates the standby's metric on a busy
// fleet; path 5 is the dashboard's split for cookie vs
// bearer; paths 2, 4, 7, 8 are the §12 alert panel.
//
// # Why a custom 503 body
//
// Per spec §10 / pkg/api/errors.go the API uses RFC 7807
// problem-detail JSON. The gate is in front of apid, so it
// emits the body directly (apid's handler-chain won't run on
// a relayed or rejected request). The shape matches apid's
// problem-detail so client SDKs can decode uniformly.
//
// # Why isApidPath is injected
//
// The gate sits in the wake/proxy pipeline that has its own
// `isApidPath` predicate (cmd/gatewayd-internal/proxy.go:220).
// Per PR-B / ADR-084 §Decision #2 the predicate was promoted
// to pkg/apid/router.go; we receive it as an injected function
// rather than reaching for it directly so the gate stays
// testable with a stub matcher.
package main

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/onebox-faas/faas/pkg/api"
	"github.com/onebox-faas/faas/pkg/gateway/writegate"
	"github.com/onebox-faas/faas/pkg/wire"
)

// writeGateMetrics is the surface the gate uses from
// *wire.OpsMetrics — extracted to an interface so the test
// suite can pass a stub without standing up the full
// Prometheus registry every daemons constructs.
//
// The interface is intentionally narrow: the gate increments
// the write-redirect counter, observes the latency histogram,
// and cross-links mTLS failures into the Tier A8 active-
// passive failover counter (per ADR-084 §Decision #7 — keep
// the two dashboards in sync so an operator chasing one
// metric lands on the other).
type writeGateMetrics interface {
	WriteRedirectTotal(outcome, authKind string) prometheus.Counter
	WriteRedirectLatency() prometheus.Observer
	ActivePassiveFailovers(outcome string) prometheus.Counter
}

// compile-time guard: *wire.OpsMetrics satisfies the
// interface. If pkg/wire ever drifts, this line breaks
// the build of cmd/gatewayd-internal.
var _ writeGateMetrics = (*wire.OpsMetrics)(nil)

// writeGate is the Tier A9 standby write-redirect handler.
// It is constructed once in run.go and wrapped around the
// apidProxy's `next` parameter (the wake/proxy pipeline).
type writeGate struct {
	next          http.Handler
	resolver      writegate.LeaderResolver
	client        writegate.LeaderHTTPClient
	isApidPath    func(string) bool
	localNodeName string
	metrics       writeGateMetrics
	log           *slog.Logger

	// Timers pre-resolved from pkg/api/limits so every emit
	// reads from the canonical constants (not a hard-coded
	// 5s / 60s). The struct caches them at construction —
	// these values are immutable for the daemon's lifetime.
	retryAfter         time.Duration
	noLeaderRetryAfter time.Duration
}

// newWriteGate builds a writeGate. `next` is the apidProxy
// (or the wake/proxy pipeline in tests). `isApidPath` is the
// shared predicate from pkg/apid/router.go — see the package
// doc above for the rationale on injection.
//
// All four timer/quota sources are read from pkg/api/limits
// at construction; the gate does NOT consult limits on the
// hot path. A unit test that exercises a different quota
// value swaps a fresh writeGate struct manually.
func newWriteGate(
	next http.Handler,
	resolver writegate.LeaderResolver,
	client writegate.LeaderHTTPClient,
	isApidPath func(string) bool,
	localNodeName string,
	metrics writeGateMetrics,
	log *slog.Logger,
) http.Handler {
	return &writeGate{
		next:               next,
		resolver:           resolver,
		client:             client,
		isApidPath:         isApidPath,
		localNodeName:      localNodeName,
		metrics:            metrics,
		log:                log,
		retryAfter:         time.Duration(api.StandbyWriteRetryAfterSeconds) * time.Second,
		noLeaderRetryAfter: time.Duration(api.StandbyWriteNoLeaderRetryAfterSeconds) * time.Second,
	}
}
