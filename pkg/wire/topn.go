// Top-N tenant admission primitive (issue #300).
//
// Layered ABOVE the accountLabelSet (§"accountLabelSet" below) to provide
// the gauge-cardinality bound that <prefix>_top_tenant_rps{account_id}
// exposes. The two-tier design is deliberate:
//
//   - accountLabelSet bounds the per-OpsMetrics counter / histogram set
//     at maxAccountLabelValues (10 000). It's a plain map + mutex and
//     never evicts: an evicted LRU would let re-admitted ids grow the
//     Prometheus TSDB series set unbounded over the daemon's lifetime.
//
//   - topAccountSet (this file) bounds the gauge-cardinality view at
//     topAccountSetCap (1 000) ordered by the last 24h's request count.
//     It DEMOTES an account out of the top-N set when a quieter account
//     overtakes it; this is safe because the gauge is a presentation
//     view over an already-bounded counter, not the source of truth.
//     The cap-reset is keyed on a sliding 24h window driven by the
//     5s sampler (cmd/apid/topn.go / cmd/gatewayd/listener.go).
//
// Why a separate type, not extending accountLabelSet:
//
//   1. accountLabelSet is non-evicting; topAccountSet is demote-on-
//      under-count. Different invariants, different semantics.
//   2. accountLabelSet doesn't care about ordering — the underlying
//      Prometheus counter keys by hash; topAccountSet needs a sorted
//      view keyed on 24h request count.
//   3. accountLabelSet's "other" bucket is "__other__"; topAccountSet's
//      "other" bucket is literally "other" (issue #300 acceptance #4).
//      Distinct labels so a panel can filter one without filtering the
//      other.
//
// Concurrency: the primitive is mutex-guarded. The sampler goroutine
// (5s tick) is the only writer; readers (prometheus.GaugeVec.WithLabelValues
// calls) take the lock briefly to compute the effectiveLabel, then
// release before incrementing. The gauge itself is goroutine-safe by
// the Prometheus client library.

package wire

import (
	"sort"
	"sync"
	"time"
)

// topAccountSetCap is the per-<prefix>_top_tenant_rps gauge cardinality
// bound (issue #300 acceptance #4). Sized to comfortably exceed the
// Scale-plan 100-deploy upper bound × ~10 (the natural fan-out: each
// deploying customer is a few noisy apps) while staying inside the
// Prometheus "tens of thousands of series per metric" guideline. Past
// the cap, accounts collapse to "other" so the TSDB series set stays
// bounded over the daemon's lifetime.
const topAccountSetCap = 1_000

// topAccountOtherLabel is the gauge-side overflow bucket. Distinct
// from the accountLabelSet-level "otherAccountLabel" ("__other__")
// because the gauge is a presentation view over the underlying
// counter; the two-tier bound keeps the panel selectors
// ({account_id!="__other__"} vs {account_id!="other"}) orthogonal so
// an operator can graph either view without filtering the other.
const topAccountOtherLabel = "other"

// topAccountWindow is the rolling window over which the top-N is
// ranked. Reset every 24h by the sampler goroutine (cmd/apid/topn.go).
// 24h matches the issue #300 acceptance #4 contract: "the top-1000
// customers (by 24h request count)".
const topAccountWindow = 24 * time.Hour

// accountCount is the (id, count) tuple stored in topAccountSet.topN.
// Sorted descending by count in topN(); ties broken by id (lex) so
// the order is deterministic for tests.
type accountCount struct {
	id    string
	count uint64
}

// topAccountSet is the bounded admission primitive backing the
// <prefix>_top_tenant_rps{account_id} gauge. Layered above
// accountLabelSet: an account_id that has been admitted by
// accountLabelSet may still be demoted below the top-N here. The
// reverse (admitted by topAccountSet but not by accountLabelSet)
// is impossible — topAccountSet never grows the underlying counter
// series.
//
// The set is initialised once per OpsMetrics in NewOpsMetrics.
// Pointer receiver because it holds a sync.Mutex (govet copylocks).
type topAccountSet struct {
	mu sync.Mutex
	// counts is the per-account rolling-window counter. Keys are the
	// effective (post-accountLabelSet) account_id; values are the
	// request count over the last topAccountWindow. Reset to zero
	// every window by resetWindow.
	counts map[string]uint64
	// cap is the top-N ceiling. Past this, accounts fall into
	// "other" — see topN().
	cap int
	// lastReset tracks the last resetWindow time so the sampler can
	// call resetWindow() without a clock dependency.
	lastReset time.Time
	// now is the clock seam. The sampler overrides it; tests use a
	// fake clock to advance past topAccountWindow deterministically.
	now func() time.Time
}

// newTopAccountSet constructs a top-N admission set with the given
// capacity. Capacity must be > 0; the call panics otherwise to fail
// loud at boot rather than silently allow unbounded admission.
//
// The sampler drives resetWindow() from outside; the constructor
// only initialises lastReset = now(). Returning by pointer because
// topAccountSet holds a sync.Mutex.
func newTopAccountSet(capacity int) *topAccountSet {
	if capacity <= 0 {
		panic("wire: topAccountSet capacity must be positive")
	}
	return &topAccountSet{
		counts:    make(map[string]uint64, capacity),
		cap:       capacity,
		lastReset: time.Now(),
		now:       time.Now,
	}
}

// sample increments the rolling-window counter for the given account
// id. Cheap path: takes the lock, increments the count map, releases.
// Does NOT consult the top-N — that's the sampler's job (called once
// per 5s tick from cmd/apid/topn.go via topNSnapshot).
//
// Why a separate sample vs topNSnapshot pair (instead of the obvious
// sample-returns-label design): under concurrent sampling, the
// top-N membership bounces for any given id as more ids arrive.
// A sample path that ALSO returned the resolved label would create
// a gauge row for that id on every sample where the id happened
// to be in the top-N at that instant — even if the id was about
// to be evicted on the next call. The gauge would then accumulate
// series for every id that ever transiently held a top-N slot,
// blowing past the cardinality bound.
//
// Pushing the top-N read to a once-per-tick snapshot from a single
// goroutine (the sampler) bounds the gauge series set to at most
// cap + 1, deterministically.
func (s *topAccountSet) sample(safeLabel string) {
	s.mu.Lock()
	s.counts[safeLabel]++
	s.mu.Unlock()
}

// topNSnapshot returns the current top-N (id, count) tuples sorted
// descending by count, ties broken by id (lex) for deterministic
// ordering. The returned slice is a copy — callers may mutate it
// without affecting the primitive. Holds mu only across the slice
// copy; the sort runs outside the critical section.
//
// Length is at most s.cap. The sampler goroutine (cmd/apid/topn.go)
// calls this once per 5s tick to drive the gauge emission; the gauge
// is bounded at cap + 1 series because only the snapshot's tuples
// (plus the "other" overflow) are emitted, regardless of how many
// transient top-N bounces happened during the elapsed window.
//
// Read-only: does not mutate counts. Concurrent calls are safe.
func (s *topAccountSet) topNSnapshot() []accountCount {
	s.mu.Lock()
	raw := make([]accountCount, 0, len(s.counts))
	for id, count := range s.counts {
		if count == 0 {
			continue
		}
		raw = append(raw, accountCount{id: id, count: count})
	}
	s.mu.Unlock()
	sort.Slice(raw, func(i, j int) bool {
		if raw[i].count != raw[j].count {
			return raw[i].count > raw[j].count
		}
		return raw[i].id < raw[j].id
	})
	if len(raw) > s.cap {
		raw = raw[:s.cap]
	}
	return raw
}

// resetWindow wipes the rolling-window counts and updates lastReset.
// Called by the sampler goroutine every topAccountWindow. Holds mu
// across the wipe; the new map is allocated outside the lock so a
// caller that observes a partially-cleared state sees a valid (if
// empty) map.
//
// Why wipe instead of evict-by-age: the 5s sampler is too coarse to
// track per-request timestamps; the spec calls for a single 24h
// top-N, not a sliding window. A coarser reset is also cheaper and
// keeps the gauge-side cardinality predictable.
func (s *topAccountSet) resetWindow() {
	s.mu.Lock()
	s.counts = make(map[string]uint64, s.cap)
	s.lastReset = s.now()
	s.mu.Unlock()
}

// shouldReset returns true if the sampler should call resetWindow
// on the next tick. Cheap read; called from the 5s sampler.
func (s *topAccountSet) shouldReset() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.now().Sub(s.lastReset) >= topAccountWindow
}
