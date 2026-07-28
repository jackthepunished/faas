// Top-N app admission primitive (issue #301, ADR-044).
//
// Layered ABOVE the accountLabelSet (§"accountLabelSet" below) to
// provide the cardinality bound that backs
// `vmmd_cpu_throttle_seconds_total{account_id, app_id}`. The
// admission primitive is intentionally a sibling of topAccountSet
// (pkg/wire/topn.go, issue #300) not a generalisation of it:
//
//   - topAccountSet caps at topAccountSetCap (1 000) and is keyed
//     by account_id only. The "noisy customer" alert cares about
//     who, not what.
//   - topAppSet (this file) caps at topAppSetCap (100) and is keyed
//     by the composite (account_id, app_id). The fleet throttle
//     dashboard cares about which app is hot, not who owns it —
//     `app_id` is the operationally useful granularity for a
//     per-app inventory.
//
// The two-tier design (accountLabelSet → topAppSet) mirrors the
// accountLabelSet → topAccountSet chain for issue #300: an
// (account_id, app_id) pair that has been admitted by
// accountLabelSet may still be demoted below the top-N here, but
// the reverse (admitted by topAppSet but not by accountLabelSet) is
// impossible — topAppSet never grows the underlying counter
// series.
//
// Why a separate type, not extending topAccountSet:
//
//   1. The composite key has different hash semantics. Mapping
//      (account_id, app_id) → a single string key is cheap but the
//      pair of strings is 2× the storage and the cap-reset has to
//      walk the pair, not a single key. A dedicated type keeps the
//      field set tight (no string concat on every sample).
//   2. The cap is different (100 vs 1 000). Reusing topAccountSet
//      would either over-cap (waste TSDB slots) or under-cap
//      (premature "other" demotion) depending on the call site.
//   3. The window could conceivably differ in a future iteration —
//      keeping the primitives separate makes that a 1-line change
//      instead of a per-call-site parameter.
//
// Concurrency: the primitive is mutex-guarded. The sampler
// goroutine (the vmmd-side throttled-seconds-rate sampler, fed
// every 5s from pkg/fcvm/cgroupstats/reader.go) is the only writer;
// readers (prometheus.CounterVec.WithLabelValues calls) take the
// lock briefly to compute the effective label, then release before
// incrementing. The counter itself is goroutine-safe by the
// Prometheus client library.

package wire

import (
	"sort"
	"sync"
	"time"
)

// topAppSetCap is the per-(account_id, app_id) capacity bound
// (issue #301 acceptance #4). Sized to comfortably exceed the
// Scale-plan 100-deploy upper bound × 1 (one row per app; the
// hot-app granularity is the per-app count, not the per-tenant
// sum). Past the cap, (account_id, app_id) pairs collapse to
// ("other", "other") so the TSDB series set stays bounded over the
// daemon's lifetime.
const topAppSetCap = 100

// topAppOtherLabel is the overflow bucket for the composite key.
// Distinct from the topAccountOtherLabel ("other") and the
// accountLabelSet-level "__other__" because the dashboard's
// selectors are typed: a panel that wants only the top-N cached
// rows uses {app_id!="other"}; a panel that wants the account
// gauge excludes the per-app overflow via a different selector.
const topAppOtherLabel = "other"

// topAppOtherAccountLabel is the account_id slot for the overflow
// bucket. Kept as a named const so a contributor who edits the
// overflow logic finds the spelling in one place.
const topAppOtherAccountLabel = "other"

// topAppWindow is the rolling window over which the top-N is
// ranked. Same 24h as topAccountSet so the fleet abuse desk sees
// one consistent window across both primitives; the sampler
// goroutine on the vmmd side drives the reset on the same cadence.
const topAppWindow = 24 * time.Hour

// appCount is the (account_id, app_id, count) tuple stored in
// topAppSet.topN. Sorted descending by count in topN(); ties
// broken by (account_id, app_id) lex so the order is deterministic
// for tests.
type appCount struct {
	accountID string
	appID     string
	count     uint64
}

// appKey is the composite key for topAppSet. Stored as a value
// type (not a pointer) so the map can hold the key inline and the
// sort comparator can pluck fields without an indirection.
type appKey struct {
	accountID string
	appID     string
}

// topAppSet is the bounded admission primitive backing
// `vmmd_cpu_throttle_seconds_total{account_id, app_id}`. Layered
// above accountLabelSet; see the package doc for the layered
// invariant.
//
// The set is initialised once per OpsMetrics in NewOpsMetrics.
// Pointer receiver because it holds a sync.Mutex (govet copylocks).
type topAppSet struct {
	mu sync.Mutex
	// counts is the per-app rolling-window counter. Keys are the
	// composite (account_id, app_id); values are the throttle
	// observations over the last topAppWindow. Reset to zero
	// every window by resetWindow.
	counts map[appKey]uint64
	// cap is the top-N ceiling. Past this, apps fall into
	// ("other", "other") — see topN().
	cap int
	// lastReset tracks the last resetWindow time so the sampler
	// can call resetWindow() without a clock dependency.
	lastReset time.Time
	// now is the clock seam. The sampler overrides it; tests use
	// a fake clock to advance past topAppWindow deterministically.
	now func() time.Time
}

// newTopAppSet constructs a top-N admission set with the given
// capacity. Capacity must be > 0; the call panics otherwise to
// fail loud at boot rather than silently allow unbounded
// admission.
//
// The sampler drives resetWindow() from outside; the constructor
// only initialises lastReset = now(). Returning by pointer because
// topAppSet holds a sync.Mutex.
func newTopAppSet(capacity int) *topAppSet {
	if capacity <= 0 {
		panic("wire: topAppSet capacity must be positive")
	}
	return &topAppSet{
		counts:    make(map[appKey]uint64, capacity),
		cap:       capacity,
		lastReset: time.Now(),
		now:       time.Now,
	}
}

// sample increments the rolling-window counter for the given
// (account_id, app_id) pair. Cheap path: takes the lock,
// increments the count map, releases. Does NOT consult the top-N
// — that's the sampler's job (called once per 5s tick via
// topNSnapshot).
//
// The same "snapshot at the sampler, not the sample" rationale
// applies as for topAccountSet: under concurrent sampling, the
// top-N membership bounces for any given pair as more pairs
// arrive. Sampling-returns-label would create a series row for
// every pair that ever transiently held a top-N slot, blowing
// past the cardinality bound.
func (s *topAppSet) sample(accountID, appID string) {
	s.mu.Lock()
	s.counts[appKey{accountID: accountID, appID: appID}]++
	s.mu.Unlock()
}

// topNSnapshot returns the current top-N (account_id, app_id,
// count) tuples sorted descending by count, ties broken by
// (account_id, app_id) lex for deterministic ordering. The
// returned slice is a copy — callers may mutate it without
// affecting the primitive. Holds mu only across the slice copy;
// the sort runs outside the critical section.
//
// Length is at most s.cap. The sampler goroutine calls this once
// per 5s tick to drive the gauge emission; the counter is bounded
// at cap + 1 series because only the snapshot's tuples (plus the
// ("other", "other") overflow) are emitted, regardless of how
// many transient top-N bounces happened during the elapsed
// window.
//
// Read-only: does not mutate counts. Concurrent calls are safe.
func (s *topAppSet) topNSnapshot() []appCount {
	s.mu.Lock()
	raw := make([]appCount, 0, len(s.counts))
	for k, count := range s.counts {
		if count == 0 {
			continue
		}
		raw = append(raw, appCount{
			accountID: k.accountID,
			appID:     k.appID,
			count:     count,
		})
	}
	s.mu.Unlock()
	sort.Slice(raw, func(i, j int) bool {
		if raw[i].count != raw[j].count {
			return raw[i].count > raw[j].count
		}
		if raw[i].accountID != raw[j].accountID {
			return raw[i].accountID < raw[j].accountID
		}
		return raw[i].appID < raw[j].appID
	})
	if len(raw) > s.cap {
		raw = raw[:s.cap]
	}
	return raw
}

// resetWindow wipes the rolling-window counts and updates
// lastReset. Called by the sampler goroutine every topAppWindow.
// Holds mu across the wipe; the new map is allocated outside the
// lock so a caller that observes a partially-cleared state sees
// a valid (if empty) map.
//
// Same "wipe vs evict-by-age" rationale as topAccountSet: the 5s
// sampler is too coarse to track per-observation timestamps; the
// spec calls for a single 24h top-N, not a sliding window.
func (s *topAppSet) resetWindow() {
	s.mu.Lock()
	s.counts = make(map[appKey]uint64, s.cap)
	s.lastReset = s.now()
	s.mu.Unlock()
}

// shouldReset returns true if the sampler should call resetWindow
// on the next tick. Cheap read; called from the 5s sampler.
func (s *topAppSet) shouldReset() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.now().Sub(s.lastReset) >= topAppWindow
}

// TestAdvanceClock exposes a test-only seam on the OpsMetrics
// side so pkg/wire/topn_app_test.go can drive the 24h reset path
// deterministically. Mirrors topAccountSet's TestAdvanceClock
// helper (issue #300 review finding #2): concurrent sampler ticks
// must not observe a torn state (new clock but old lastReset).
//
// rewindLastReset controls whether lastReset is also set to base:
// true = fully restart the window (used to set the initial
// baseline); false = advance the clock without resetting the
// baseline (used to walk past the 24h threshold). The two
// behaviours are separate helpers because the production sampler
// never rewinds lastReset — that operation is test-only.
func (m *OpsMetrics) TestAdvanceAppClock(base time.Time, rewindLastReset bool) {
	if m == nil {
		return
	}
	set := m.TopAppSet()
	if set == nil {
		return
	}
	set.mu.Lock()
	set.now = func() time.Time { return base }
	if rewindLastReset {
		set.lastReset = base
	}
	set.mu.Unlock()
}
