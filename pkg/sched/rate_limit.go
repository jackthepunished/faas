// Package sched — wake-rate-limit primitive.
//
// This file holds the per-app + per-account token-bucket primitive that
// throttles the rate at which schedd will admit *new* wake operations
// against a given app or account. It is the load-bearing admission gate
// called out by:
//
//   - ADR-099 Risk #1 (jobs run-to-completion workloads) — a Scale
//     customer running `--tasks 1000 --parallelism 100` against a parked
//     app could otherwise trigger 100 cold-boots in a single 1 s tick
//     and OOM the control plane.
//
//   - ADR-080 Risk #1 (per-app async task queue) — the synthetic-wake
//     dispatch path shares the same admission surface.
//
// Distinct from pkg/gateway.Limiter in three ways:
//
//  1. Layer: pkg/gateway.Limiter throttles *inbound HTTP requests* at the
//     gateway edge (per-app RPS, per-account RPM). This limiter throttles
//     *schedd admission decisions* — the act of deciding to wake a new
//     instance — which is a downstream consequence of HTTP traffic but
//     also fires from crons, jobs dispatch, and other non-HTTP sources.
//
//  2. Units: gateway uses seconds (RPS / RPM→RPS). Wake admissions are
//     burstier and slower (a Scale customer's cron tick can legitimately
//     want to wake 10 instances in the same second). The wake-side
//     buckets refill at "per minute" granularity so a momentary burst
//     doesn't drain a long-running quota.
//
//  3. Cardinality: gateway buckets are per-app + per-account (closed
//     set, two maps). Wake-side buckets are the same shape — the schema
//     mirrors the gateway limiter so a future Postgres-backed central
//     mode (per the wake_rate_limit_counters pattern that ADR-080 §Risk
//     #1 anticipates for the gateway case) can be lifted here with
//     minimal drift.
//
// schedd is single-leader, so a single in-process mutex is sufficient.
// Cross-node drift is bounded by the leader's serialised writes.
//
// The Engine consults this limiter in admitAndDispatch after resolveApp
// has populated app/account/limits but BEFORE admitGate, so a throttled
// wake neither burns the cooldown clock nor writes an unattached
// instance row. The branch lifts to WakeResult{AtCapacity: true,
// Reason: "rate_limit"} — the same shape WakeResult already carries for
// the per-app concurrency cap.
package sched

import (
	"sync"
	"time"

	"github.com/onebox-faas/faas/pkg/api"
)

// WakeRateLimiter throttles schedd's wake-admission rate per app and
// per account. The shape mirrors pkg/gateway.Limiter (token bucket,
// injectable clock, plan-keyed burst ceiling) but operates at the
// admission layer and uses per-minute refill math.
//
// A nil *WakeRateLimiter is a no-op — Allow returns true. This mirrors
// the Engine's other late-bound dependencies (verifier, audit,
// events.Platform) so unit tests that don't exercise the limiter can
// skip the wire-up.
type WakeRateLimiter struct {
	mu          sync.Mutex
	appBuckets  map[string]*wakeBucket // appID -> bucket
	acctBuckets map[string]*wakeBucket // accountID -> bucket
	now         func() time.Time
	noop        bool // test seam: WithNoop returns a copy that always allows
}

type wakeBucket struct {
	tokens float64
	rpm    float64 // refill rate (tokens per minute)
	burst  float64 // bucket ceiling
	last   time.Time
}

// NewWakeRateLimiter returns a limiter using the wall clock.
func NewWakeRateLimiter() *WakeRateLimiter {
	return &WakeRateLimiter{
		appBuckets:  map[string]*wakeBucket{},
		acctBuckets: map[string]*wakeBucket{},
		now:         time.Now,
	}
}

// NewWakeRateLimiterWithClock returns a limiter with an injectable
// clock. Test seam — production code uses NewWakeRateLimiter.
func NewWakeRateLimiterWithClock(now func() time.Time) *WakeRateLimiter {
	return &WakeRateLimiter{
		appBuckets:  map[string]*wakeBucket{},
		acctBuckets: map[string]*wakeBucket{},
		now:         now,
	}
}

// AllowWakeApp reports whether a wake-admission for the given appID on
// the given plan may proceed, consuming one token from the per-app
// bucket if so. Per-plan burst ceiling comes from
// api.Limits.WakeBurstPerApp (Free 1 / Hobby 5 / Pro 20 / Scale 100).
//
// Returns false on:
//   - unknown plan (fail closed; same shape as pkg/gateway.Limiter.Allow)
//   - plan value of 0 (Free plan throttles to zero — defensive; the
//     apid-side gate is the primary Free-plan block, this is the
//     schedd-side backstop)
//
// Refill rate is 1 token per (60s / burst) — at Scale=100/min the bucket
// refills every 600ms. The math intentionally uses minute-scale refill
// so a brief burst (a customer retry storm after a transient outage)
// does not drain the per-second budget the gateway limiter enforces.
func (l *WakeRateLimiter) AllowWakeApp(appID string, plan api.Plan) bool {
	if l == nil || l.noop {
		return true
	}
	limits, ok := api.LimitsFor(plan)
	if !ok {
		return false
	}
	burst := limits.WakeBurstPerApp
	if burst <= 0 {
		return false
	}
	return l.consume(l.appBuckets, appID, float64(burst), float64(burst))
}

// AllowWakeAccount reports whether a wake-admission for any app under
// the given accountID may proceed, consuming one token from the
// per-account bucket. Distinct from AllowWakeApp so a customer
// fanning out bursts across N apps in the same account cannot evade
// the per-app throttle by keeping per-app rates low (the same evasion
// shape pkg/gateway.Limiter.AllowAccount is designed to prevent).
//
// Per-plan burst ceiling: api.Limits.WakeBurstPerAccount
// (Free 1 / Hobby 10 / Pro 30 / Scale 150).
func (l *WakeRateLimiter) AllowWakeAccount(accountID string, plan api.Plan) bool {
	if l == nil || l.noop {
		return true
	}
	limits, ok := api.LimitsFor(plan)
	if !ok {
		return false
	}
	burst := limits.WakeBurstPerAccount
	if burst <= 0 {
		return false
	}
	return l.consume(l.acctBuckets, accountID, float64(burst), float64(burst))
}

// consume is the shared token-bucket math. Tokens refill at
// `burst` per minute (= `burst / 60` per second); the bucket ceiling
// is `burst`. A plan change updates the bucket's parameters without
// losing in-flight tokens — see TestWakeRateLimiter_AllowWakeApp_PlanChange.
func (l *WakeRateLimiter) consume(buckets map[string]*wakeBucket, id string, rpm, burst float64) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	b := buckets[id]
	if b == nil {
		b = &wakeBucket{tokens: burst, rpm: rpm, burst: burst, last: now}
		buckets[id] = b
	} else {
		// Refresh bucket parameters on every call so a mid-flight plan
		// change (Scale→Pro downgrade) takes effect immediately. The
		// token balance carries over so the customer does not lose
		// already-earned headroom on the downgrade.
		b.rpm, b.burst = rpm, burst
		b.tokens += now.Sub(b.last).Minutes() * b.rpm
		if b.tokens > b.burst {
			b.tokens = b.burst
		}
		b.last = now
	}

	if b.tokens >= 1 {
		b.tokens--
		return true
	}
	return false
}

// ForgetApp drops an app's bucket (e.g. on app delete) to bound
// memory. Symmetric with pkg/gateway.Limiter.Forget.
func (l *WakeRateLimiter) ForgetApp(appID string) {
	if l == nil {
		return
	}
	l.mu.Lock()
	delete(l.appBuckets, appID)
	l.mu.Unlock()
}

// ForgetAccount drops an account's bucket. Symmetric with ForgetApp.
func (l *WakeRateLimiter) ForgetAccount(accountID string) {
	if l == nil {
		return
	}
	l.mu.Lock()
	delete(l.acctBuckets, accountID)
	l.mu.Unlock()
}

// ForgetAll drops every bucket and returns the count dropped.
// /metrics and SIGHUP observability use this.
func (l *WakeRateLimiter) ForgetAll() int {
	if l == nil {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	n := len(l.appBuckets) + len(l.acctBuckets)
	l.appBuckets = map[string]*wakeBucket{}
	l.acctBuckets = map[string]*wakeBucket{}
	return n
}

// WithNoop returns a new *WakeRateLimiter sharing the original's clock
// + buckets but whose Allow* methods always return true. Test seam
// only — DO NOT expose as a config knob. Bypassing the limiter would
// silently turn the wake-rate-limit into a noop, which is exactly
// what ADR-099 Risk #1 calls out as the failure mode.
func (l *WakeRateLimiter) WithNoop() *WakeRateLimiter {
	if l == nil {
		return nil
	}
	return &WakeRateLimiter{
		appBuckets:  l.appBuckets,
		acctBuckets: l.acctBuckets,
		now:         l.now,
		noop:        true,
	}
}

// BucketCount returns the total number of buckets held across both
// scopes. /metrics uses this so operators can watch for cardinality
// drift on long-running schedd instances.
func (l *WakeRateLimiter) BucketCount() int {
	if l == nil {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.appBuckets) + len(l.acctBuckets)
}
