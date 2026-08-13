// Package gateway holds gatewayd-internal's edge logic: per-app rate limiting, the host→
// app routing cache, and the wake gate that holds requests during a cold wake
// (spec §4.1). The HTTP/TLS server wires these together; each piece here is a
// self-contained, testable unit.
package gateway

import (
	"container/list"
	"sync"
	"time"

	"github.com/onebox-faas/faas/pkg/api"
)

// LimiterEvictScan bounds how many least-recently-used buckets one
// eviction pass inspects before giving up. The scan walks back-to-front
// looking for a bucket that is safe to drop (see evictOneLocked); the
// bound keeps allowToken's worst case constant under the limiter mutex
// rather than O(n) when every bucket is mid-drain.
const LimiterEvictScan = 32

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
//
// # Bucket-map growth and optional LRU discipline
//
// A limiter built with NewLimiter / NewLimiterWithClock keeps every bucket it
// has ever created: the map shrinks only via Forget / ForgetAccount /
// ForgetAll (SIGHUP). That is sound for the per-app and per-account scopes
// because the key space is bounded by the number of apps/accounts the node
// routes for.
//
// Scopes whose key space is NOT bounded that way (issue #881's per-route
// throttle keys on appID+ruleID) must use NewLimiterWithLRU, which caps the
// map and evicts least-recently-used buckets. Eviction is only ever applied
// to buckets that carry no state — see evictOneLocked for the invariant and
// why violating it would be a limit bypass.
type Limiter struct {
	mu      sync.Mutex
	buckets map[string]*bucket
	now     func() time.Time
	// noop, when true, makes Allow always return true. Set via WithNoop;
	// intended only for load tests that need to bypass the plan rps/burst
	// to assert the underlying handler path. DO NOT expose as a config
	// knob in production.
	noop bool
	// cap is the maximum number of buckets retained when LRU discipline is
	// enabled. Zero means unbounded (the historical behaviour that Allow /
	// AllowAccount rely on); a positive value activates the recency list.
	cap int
	// ll orders buckets most-recently-used at the front. Non-nil only when
	// cap > 0, so the unbounded path pays no list bookkeeping at all.
	ll *list.List
	// elems maps bucket key → its element in ll. Kept in lockstep with
	// buckets whenever ll is non-nil.
	elems map[string]*list.Element
}

type bucket struct {
	tokens float64
	rps    float64
	burst  float64
	last   time.Time
	// key is duplicated here so evictOneLocked can delete the map entry
	// from a list element without a reverse scan.
	key string
}

// NewLimiter returns a limiter using the wall clock.
func NewLimiter() *Limiter {
	return &Limiter{buckets: map[string]*bucket{}, now: time.Now}
}

// NewLimiterWithClock returns a limiter whose internal clock is the supplied
// func. Test seam for the cmd/gatewayd-internal/backend_test.go 1001-request
// acceptance (issue #292 / ADR-040) — frozen-clock tests cannot depend on
// the package-private now field. Production code uses NewLimiter.
func NewLimiterWithClock(now func() time.Time) *Limiter {
	return &Limiter{buckets: map[string]*bucket{}, now: now}
}

// NewLimiterWithLRU returns a limiter that retains at most cap buckets,
// evicting least-recently-used entries once the map is full. Intended for
// scopes whose key space is not bounded by the app/account count — issue
// #881's per-route throttle keys on (appID, ruleID), so a customer with many
// rules across many apps would otherwise grow the map without limit.
//
// cap <= 0 yields the historical unbounded limiter, so callers can pass a
// config value through without a nil check. Pass EdgeRuleCacheCap to match
// the edge-rule cache's ceiling.
//
// Eviction never resets an in-flight throttle: only buckets that have
// refilled completely are candidates (see evictOneLocked).
func NewLimiterWithLRU(cap int) *Limiter {
	return newLimiterWithLRU(cap, time.Now)
}

// NewLimiterWithLRUClock is NewLimiterWithLRU with an injectable clock. Test
// seam mirroring NewLimiterWithClock; eviction behaviour depends on elapsed
// time, so the tests need a frozen clock to be deterministic.
func NewLimiterWithLRUClock(cap int, now func() time.Time) *Limiter {
	return newLimiterWithLRU(cap, now)
}

func newLimiterWithLRU(cap int, now func() time.Time) *Limiter {
	l := &Limiter{buckets: map[string]*bucket{}, now: now}
	if cap > 0 {
		l.cap = cap
		l.ll = list.New()
		l.elems = map[string]*list.Element{}
	}
	return l
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
//
// When the limiter was built with NewLimiterWithLRU, each call also marks the
// bucket most-recently-used and, on inserting into a full map, tries to evict.
// The refill math itself is identical in both modes.
func (l *Limiter) allowToken(id string, rps, burst float64) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	b := l.buckets[id]
	if b == nil {
		// Make room before inserting so the map never exceeds cap. If
		// nothing is safe to evict the map is allowed to overshoot —
		// correctness of in-flight throttles outranks the memory bound,
		// and BucketCount surfaces the overshoot to /metrics.
		if l.cap > 0 && len(l.buckets) >= l.cap {
			l.evictOneLocked(now)
		}
		b = &bucket{tokens: burst, rps: rps, burst: burst, last: now, key: id}
		l.buckets[id] = b
		if l.ll != nil {
			l.elems[id] = l.ll.PushFront(b)
		}
	} else {
		// A plan change updates the bucket's parameters without losing tokens.
		b.rps, b.burst = rps, burst
		b.tokens += now.Sub(b.last).Seconds() * b.rps
		if b.tokens > b.burst {
			b.tokens = b.burst
		}
		b.last = now
		if l.ll != nil {
			if el := l.elems[id]; el != nil {
				l.ll.MoveToFront(el)
			}
		}
	}

	if b.tokens >= 1 {
		b.tokens--
		return true
	}
	return false
}

// evictOneLocked drops at most one bucket to make room. Caller holds l.mu.
//
// # The safety invariant
//
// A bucket may only be evicted when it has refilled to its ceiling
// (tokens >= burst). Such a bucket carries no information: re-creating it on
// the next request produces a full bucket, which is exactly what was dropped,
// so the eviction is unobservable.
//
// Evicting a partially drained bucket would be a LIMIT BYPASS. A caller who
// has spent its tokens is meant to receive 429s until the bucket refills;
// dropping that bucket hands them a fresh full one. Because bucket creation
// is driven by request traffic, an attacker who can push the map to its cap
// could then reset their own throttle at will — turning the memory bound into
// an attack primitive. The scan therefore skips any bucket that is mid-drain,
// even though that means the map can exceed cap under sustained pressure.
//
// The walk is back-to-front (least- to most-recently-used) and inspects at
// most LimiterEvictScan entries so the mutex is never held for an O(n) sweep.
func (l *Limiter) evictOneLocked(now time.Time) {
	if l.ll == nil {
		return
	}
	el := l.ll.Back()
	for i := 0; el != nil && i < LimiterEvictScan; i++ {
		prev := el.Prev()
		b, _ := el.Value.(*bucket)
		if b != nil && bucketFull(b, now) {
			l.removeElementLocked(el)
			return
		}
		el = prev
	}
}

// bucketFull reports whether b would be at its ceiling if refilled to now.
// Mirrors allowToken's refill formula but without writing back, so a bucket
// is judged by what the next request would see rather than by the stale
// token count left over from its previous call.
func bucketFull(b *bucket, now time.Time) bool {
	if b.burst <= 0 {
		return false
	}
	tokens := b.tokens
	if dt := now.Sub(b.last).Seconds(); dt > 0 {
		tokens += dt * b.rps
	}
	return tokens >= b.burst
}

// removeElementLocked drops one element from both the recency list and the
// bucket map. Caller holds l.mu. Mirrors EdgeRuleCache.removeElement.
func (l *Limiter) removeElementLocked(el *list.Element) {
	b, _ := el.Value.(*bucket)
	l.ll.Remove(el)
	if b != nil {
		delete(l.buckets, b.key)
		delete(l.elems, b.key)
	}
}

// forgetLocked drops one key from the map and, when LRU discipline is on,
// from the recency list too. Caller holds l.mu.
func (l *Limiter) forgetLocked(id string) {
	delete(l.buckets, id)
	if l.ll != nil {
		if el := l.elems[id]; el != nil {
			l.ll.Remove(el)
			delete(l.elems, id)
		}
	}
}

// Forget drops an app's bucket (e.g. on delete) to bound memory.
func (l *Limiter) Forget(appID string) {
	l.mu.Lock()
	l.forgetLocked(appID)
	l.mu.Unlock()
}

// Peek is a non-mutating snapshot of the token bucket for appID on plan.
// Used by the response-header writer (X-RateLimit-*) and by the
// /v1/internal/quota endpoint that backs the dashboard's bucket
// indicator (issue #314 / Finding 6). The function copies (tokens,
// last) into locals, applies the standard refill formula against the
// supplied clock WITHOUT writing back, and returns the same shape
// allowToken would have produced — but never decrements.
//
// Returns:
//
//	limit         = plan.RateLimitBurst (the bucket ceiling)
//	remaining     = floor(tokens) (token-count visible to the next caller)
//	resetSeconds  = ceil((1 - fractionalTokens) / rps) when tokens < 1,
//	               else 0 (the bucket is full and resets immediately on Allow)
//	ok            = true iff the bucket exists AND the plan is known;
//	               false on unknown plan, on missing bucket (app has
//	               never been Allow'd into the limiter — distinct from
//	               "0 remaining"), and on noop mode (the noop limiter
//	               has no bucket state — callers must skip the
//	               header write rather than emit zero-valued headers
//	               that imply exhaustion).
//
// Peek holds the limiter mutex for the same duration allowToken does
// — a copy + math. Concurrent Peek/Allow under -race is safe (the
// mutex is the sync point). The refill math intentionally mirrors
// allowToken so a snapshot read on tick N agrees with the consumption
// Allow on tick N+1 would have made.
//
// Cross-process limitation (documented in the header contract): in a
// multi-gatewayd-internal fleet each gateway process keeps its own bucket; the
// values reflect THIS gatewayd-internal's view. ADR-040 already flagged this;
// this surface makes the limitation visible, not gone.
func (l *Limiter) Peek(appID string, plan api.Plan) (limit, remaining, resetSeconds int, ok bool) {
	if l == nil || l.noop {
		return 0, 0, 0, false
	}
	limits, found := api.LimitsFor(plan)
	if !found {
		return 0, 0, 0, false
	}
	limit = limits.RateLimitBurst

	l.mu.Lock()
	defer l.mu.Unlock()
	b := l.buckets[appID]
	if b == nil {
		// The bucket has never been Allow'd — the app exists in the
		// routing cache but hasn't been seen at the rate-limit edge
		// yet. Returning ok=false here distinguishes "never seen"
		// (dashboard renders "—") from "exhausted" (dashboard
		// renders "0 of N remaining").
		return limit, 0, 0, false
	}
	// Read the cached rps/burst the limiter stored when the bucket was
	// last Allow'd. allowToken refreshes these on every call (so a
	// mid-flight plan change is reflected); Peek picks up whatever
	// value is currently cached. Peek does NOT write back — leaving the
	// bucket unchanged is the non-mutating contract.
	rps, burst := b.rps, b.burst
	tokens := b.tokens
	last := b.last
	now := l.now()
	dt := now.Sub(last).Seconds()
	if dt > 0 {
		tokens += dt * rps
		if tokens > burst {
			tokens = burst
		}
	}
	if tokens < 0 {
		tokens = 0
	}
	remaining = int(tokens)
	if remaining < 0 {
		remaining = 0
	}
	if tokens < 1 {
		// ceil((1 - tokens) / rps) → seconds until the next whole
		// token becomes available. Adding 1 then floor() gives the
		// standard ceil semantics for positive denominators
		// (rps > 0 by construction in api.LimitsFor).
		resetSeconds = int((1-tokens)/rps + 1)
		if resetSeconds < 1 {
			resetSeconds = 1
		}
	}
	return limit, remaining, resetSeconds, true
}

// PeekAccount mirrors Peek for the per-account bucket (ADR-040). The
// rate units in the limits table for the per-account path are RPM,
// so allowToken divides by 60; Peek does the same so the visible
// reset time uses the same denominator the limiter actually refills
// at. Returns ok=false on noop, unknown plan, or missing bucket (the
// same "never seen" contract as Peek).
func (l *Limiter) PeekAccount(accountID string, plan api.Plan) (limit, remaining, resetSeconds int, ok bool) {
	if l == nil || l.noop {
		return 0, 0, 0, false
	}
	rpm := plan.RateLimitPerAccountRPM()
	if rpm <= 0 {
		return 0, 0, 0, false
	}
	rps := float64(rpm) / 60.0
	burst := float64(rpm)
	limit = rpm

	l.mu.Lock()
	defer l.mu.Unlock()
	b := l.buckets[accountID]
	if b == nil {
		return limit, 0, 0, false
	}
	tokens := b.tokens
	last := b.last
	now := l.now()
	dt := now.Sub(last).Seconds()
	if dt > 0 {
		tokens += dt * rps
		if tokens > burst {
			tokens = burst
		}
	}
	if tokens < 0 {
		tokens = 0
	}
	remaining = int(tokens)
	if remaining < 0 {
		remaining = 0
	}
	if tokens < 1 {
		resetSeconds = int((1-tokens)/rps + 1)
		if resetSeconds < 1 {
			resetSeconds = 1
		}
	}
	return limit, remaining, resetSeconds, true
}

// ForgetAccount drops an account's bucket (ADR-040). Symmetric with Forget
// for the per-account scope. SIGHUP uses ForgetAll which covers both scopes
// at once; ForgetAccount is the per-scope escape hatch (e.g. for an admin
// endpoint that needs to clear one customer's throttle without resetting
// the whole fleet — not wired today, but the seam is here).
func (l *Limiter) ForgetAccount(accountID string) {
	l.mu.Lock()
	l.forgetLocked(accountID)
	l.mu.Unlock()
}

// ForgetAll drops every bucket and returns the count dropped. SIGHUP and the
// apid-side "purge all" callback use this so an operator can recover memory
// after a mass-delete without bouncing the daemon.
func (l *Limiter) ForgetAll() int {
	l.mu.Lock()
	n := len(l.buckets)
	l.buckets = map[string]*bucket{}
	if l.ll != nil {
		l.ll.Init()
		l.elems = map[string]*list.Element{}
	}
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
//
// LRU state is deliberately NOT carried over: the noop limiter never inserts
// buckets, so leaving cap at zero keeps the recency list out of a path that
// would otherwise have to keep it in lockstep for no benefit.
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
