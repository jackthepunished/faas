// Package gateway holds gatewayd's edge logic: per-app rate limiting, the host→
// app routing cache, and the wake gate that holds requests during a cold wake
// (spec §4.1). The HTTP/TLS server wires these together; each piece here is a
// self-contained, testable unit.
package gateway

import (
	"sync"
	"time"

	"github.com/onebox-faas/faas/pkg/api"
)

// Limiter is a per-app token-bucket rate limiter (spec §4.1). Each app refills at
// its plan's rps with a plan burst; an over-limit request is rejected (the caller
// returns 429). The clock is injectable so the refill math is tested precisely.
//
// The same Limiter type is reused for per-account throttling (ADR-040 / issue
// #292) via AllowAccount — bucket key is the actor ID (app or account), and the
// rps/burst parameters come from a different accessor on Plan. Two *Limiter
// instances are constructed in pkg/gateway/handler.go (one per scope) so SIGHUP
// ForgetAll can target each scope independently and load tests can bypass one
// without bypassing the other.
type Limiter struct {
	mu      sync.Mutex
	buckets map[string]*bucket
	now     func() time.Time
	// noop, when true, makes Allow always return true. Set via WithNoop;
	// intended only for load tests that need to bypass the plan rps/burst
	// to assert the underlying handler path. DO NOT expose as a config
	// knob in production.
	noop bool
}

type bucket struct {
	tokens float64
	rps    float64
	burst  float64
	last   time.Time
}

// NewLimiter returns a limiter using the wall clock.
func NewLimiter() *Limiter {
	return &Limiter{buckets: map[string]*bucket{}, now: time.Now}
}

// NewLimiterWithClock returns a limiter whose internal clock is the supplied
// func. Test seam for the cmd/gatewayd/backend_test.go 1001-request
// acceptance (issue #292 / ADR-040) — frozen-clock tests cannot depend on
// the package-private now field. Production code uses NewLimiter.
func NewLimiterWithClock(now func() time.Time) *Limiter {
	return &Limiter{buckets: map[string]*bucket{}, now: now}
}

// Allow reports whether a request for appID on plan may proceed, consuming a
// token if so. Plan rps/burst come from the limits table (never inlined).
func (l *Limiter) Allow(appID string, plan api.Plan) bool {
	if l.noop {
		return true
	}
	limits, ok := api.LimitsFor(plan)
	if !ok {
		return false
	}
	return l.allowToken(appID, float64(limits.RateLimitRPS), float64(limits.RateLimitBurst))
}

// AllowAccount consumes one token from the bucket keyed by accountID (ADR-040 /
// issue #292). Bucket parameters come from Plan.RateLimitPerAccountRPM —
// internal math divides by 60 to derive rps and uses the RPM value as the
// burst ceiling, so an account can absorb a burst of `RPM` requests before
// the per-minute refill kicks in. Distinct from Allow (per-app) so a botnet
// rotating across many apps within an account cannot evade this limiter by
// keeping per-app rps low.
func (l *Limiter) AllowAccount(accountID string, plan api.Plan) bool {
	if l.noop {
		return true
	}
	rpm := plan.RateLimitPerAccountRPM()
	if rpm <= 0 {
		return false // fail closed on unknown plan (mirrors CronLimitPerAccount)
	}
	return l.allowToken(accountID, float64(rpm)/60.0, float64(rpm))
}

// allowToken is the shared token-bucket math used by both Allow (per-app) and
// AllowAccount (per-account). Keyed by the actor ID; rps is the refill rate,
// burst is the bucket ceiling. Returns true on a consumed token, false when
// the bucket is empty (caller responds 429). Plan changes update rps/burst
// without losing in-flight tokens — see TestLimiterAllowAccount_PlanChange.
func (l *Limiter) allowToken(id string, rps, burst float64) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	b := l.buckets[id]
	if b == nil {
		b = &bucket{tokens: burst, rps: rps, burst: burst, last: now}
		l.buckets[id] = b
	} else {
		// A plan change updates the bucket's parameters without losing tokens.
		b.rps, b.burst = rps, burst
		b.tokens += now.Sub(b.last).Seconds() * b.rps
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

// Forget drops an app's bucket (e.g. on delete) to bound memory.
func (l *Limiter) Forget(appID string) {
	l.mu.Lock()
	delete(l.buckets, appID)
	l.mu.Unlock()
}

// ForgetAccount drops an account's bucket (ADR-040). Symmetric with Forget
// for the per-account scope. SIGHUP uses ForgetAll which covers both scopes
// at once; ForgetAccount is the per-scope escape hatch (e.g. for an admin
// endpoint that needs to clear one customer's throttle without resetting
// the whole fleet — not wired today, but the seam is here).
func (l *Limiter) ForgetAccount(accountID string) {
	l.mu.Lock()
	delete(l.buckets, accountID)
	l.mu.Unlock()
}

// ForgetAll drops every bucket and returns the count dropped. SIGHUP and the
// apid-side "purge all" callback use this so an operator can recover memory
// after a mass-delete without bouncing the daemon.
func (l *Limiter) ForgetAll() int {
	l.mu.Lock()
	n := len(l.buckets)
	l.buckets = map[string]*bucket{}
	l.mu.Unlock()
	return n
}

// WithNoop returns a new Limiter sharing l's clock + buckets but whose
// Allow always returns true. Test-only seam for the 1k rps hot-path load
// test (handler_load_test.go) which needs to bypass plan rps/burst to
// measure the underlying handler path. DO NOT expose this as a config knob
// — bypassing the limiter would silently turn the §4.1 rate-limit into a
// noop.
//
// The returned limiter shares l's bucket map (read-only after construction
// in tests; no Allow path mutates it). Constructing a fresh mutex is
// intentional — copying a sync.Mutex is a vet error.
func (l *Limiter) WithNoop() *Limiter {
	return &Limiter{
		buckets: l.buckets,
		now:     l.now,
		noop:    true,
	}
}

// BucketCount returns the number of buckets currently held. /metrics and the
// SIGHUP observability log use this.
func (l *Limiter) BucketCount() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.buckets)
}
