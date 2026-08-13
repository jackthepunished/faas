// upstream_affinity.go — schedd's connection-aware placement bias
// (ADR-098 §9.A PR-D).
//
// The meterd probe loop (pkg/meter/upstream_probe.go) writes one
// data_upstream_probes row per (host_redacted_hash, kind, port,
// region, sampled_at) every 30 s. On wake, schedd reads the most
// recent probe row PER region for the app's captured upstreams,
// picks the region with the lowest observed RTT, and biases the
// chooser toward a node in that region.
//
// Scoring contract:
//   - scoreForUpstreams(rows) → (preferredRegion, meanMs)
//   - Empty region when no probe exists yet (cold install), or
//     when the probe set is empty (no captured upstreams). The
//     chooser handles empty by failing open — bias is skipped
//     and the legacy tie-break (RAM, vCPU, region, zone, name)
//     wins.
//   - "score" is the mean RTT in ms across the app's captured
//     upstreams for the preferred region. The chooser consults
//     the score only via Request.PreferredRegion (the bias
//     INSERT in betterCandidate matches region strings — it
//     does NOT consume the score directly).
//
// Per ADR-098 §D2 schedd reads synchronously at wake — NOT via
// LISTEN on data_upstreams_changed. A LISTEN would require schedd
// to maintain a 1-RTT cache invalidation state machine; the
// synchronous read is O(1) on the data_upstreams primary-key
// index and matches the per-wake latency budget (< 1 ms extra).
//
// The TTL on the in-process cache (api.UpstreamAffinityTTL,
// default 30 s) bounds the staleness of the cached preferred
// region between wakes of the same app; the probe loop's 30 s
// cadence matches the TTL so the cache is never more than one
// probe-cycle stale.
//
// Fail-open is the load-bearing claim: a meterd-side outage (or
// a feature-flag-off deploy) MUST NOT break the legacy chooser.
// The tests in pkg/sched/upstream_affinity_test.go + the 4 new
// placement_test.go scenarios pin the failure mode end-to-end.

package sched

import (
	"context"
	"sync"
	"time"

	"github.com/onebox-faas/faas/pkg/api"
	"github.com/onebox-faas/faas/pkg/state"
	"github.com/onebox-faas/faas/pkg/state/sqlc"
)

// UpstreamAffinity is the connection-aware placement cache. Maps
// appID → (preferredRegion, score, observedAt). Safe for concurrent
// use; the read path is the wake path so the lock contention is
// per-app (the wake holds appMu upstream so two wakes for the
// same app serialise anyway — the lock here just guards the map
// shape).
//
// Lazy TTL eviction on read matches pkg/sched.WarmAffinity's
// shape; a periodic sweeper is future work (cardinality stays
// small in practice).
type UpstreamAffinity struct {
	mu  sync.RWMutex
	ttl time.Duration
	now func() time.Time
	// store is the read-side boundary. *state.PgStore satisfies
	// it in production; tests inject a stub. nil ⇒ the cache
	// always returns ok=false (the FAAS_UPSTREAM_AFFINITY=0
	// branch — no DB read, no chooser bias).
	store UpstreamAffinityStore
	// m is keyed by appID; values carry the preferredRegion +
	// the mean RTT across the app's upstreams + the observedAt
	// stamp (so a stale entry can be evicted on read).
	m map[string]upstreamAffinityEntry
}

type upstreamAffinityEntry struct {
	preferredRegion string
	scoreMs         int
	observedAt      time.Time
}

// UpstreamAffinityStore is the minimal store surface the cache
// needs at refresh time. *state.PgStore satisfies it; tests
// inject a stub.
//
//   - ListAllAppDataUpstreams returns every captured upstream
//     for the app (no cursor/limit — schedd wants the full set
//     for the bias score; bounded by api.DataPlacementHintsPerApp).
//   - ListDataUpstreamProbesByHostRegion returns the most-recent
//     probe row per (host_redacted_hash, region) — the
//     chooser bias only cares about the freshest observation.
type UpstreamAffinityStore interface {
	ListAllAppDataUpstreams(ctx context.Context, accountID, appID string) ([]state.DataUpstream, error)
	ListDataUpstreamProbesByHostRegion(ctx context.Context, arg sqlc.ListDataUpstreamProbesByHostRegionParams) ([]state.DataUpstreamProbe, error)
}

// NewUpstreamAffinity returns a cache with the given TTL. A zero
// or negative TTL falls back to api.UpstreamAffinityTTL. store
// may be nil; Score then always returns ok=false (the FAAS_
// UPSTREAM_AFFINITY=0 branch).
func NewUpstreamAffinity(ttl time.Duration, store UpstreamAffinityStore) *UpstreamAffinity {
	if ttl <= 0 {
		ttl = api.UpstreamAffinityTTL
	}
	return &UpstreamAffinity{
		ttl:   ttl,
		now:   time.Now,
		store: store,
		m:     make(map[string]upstreamAffinityEntry),
	}
}

// Score returns the cached preferred region + score for the
// app. ok=false ⇒ the cache is cold/stale/empty; the caller
// (the wake path) must fail open and not bias the chooser.
//
// The read path takes the RLock; on a hit, the entry's
// observedAt is compared against now — a stale entry returns
// ok=false (and is evicted) so the next Score() triggers a
// Refresh.
func (u *UpstreamAffinity) Score(appID string) (region string, scoreMs int, ok bool) {
	if u == nil {
		return "", 0, false
	}
	u.mu.RLock()
	entry, found := u.m[appID]
	u.mu.RUnlock()
	if !found {
		return "", 0, false
	}
	if u.now().Sub(entry.observedAt) > u.ttl {
		u.mu.Lock()
		delete(u.m, appID)
		u.mu.Unlock()
		return "", 0, false
	}
	return entry.preferredRegion, entry.scoreMs, true
}

// Refresh reads the data_upstreams + data_upstream_probes rows
// for the app, picks the region with the lowest mean RTT across
// the app's captured upstreams, and stores the result in the
// cache. Returns ok=false when the app has no captured upstreams
// or no probe rows; the cache is left unchanged (no churn).
//
// The query is O(U × P) where U = upstreams-per-app (≤ plan
// quota) and P = probes-per-host-region (1 by construction —
// the probe loop keeps the freshest row). Total cost per
// refresh is bounded by api.UpstreamProbeMaxConcurrent = 64,
// well under the per-wake < 1 ms budget.
//
// accountID is required because pgstore.ListAllAppDataUpstreams
// is keyed on (account_id, app_id). appID is the string form
// passed at the wake call site (engine.app.id is already a
// string at that point).
func (u *UpstreamAffinity) Refresh(ctx context.Context, accountID, appID string) error {
	if u == nil || u.store == nil {
		return nil
	}
	upstreams, err := u.store.ListAllAppDataUpstreams(ctx, accountID, appID)
	if err != nil {
		return err
	}
	if len(upstreams) == 0 {
		return nil
	}
	// Collect the per-host probe set; the score is the mean
	// RTT across the app's captured upstreams. The chooser
	// cares about the region with the lowest mean.
	rows := []state.DataUpstreamProbe{}
	for _, up := range upstreams {
		region := up.DeclaredRegion
		if region == "" {
			continue
		}
		fresh, err := u.store.ListDataUpstreamProbesByHostRegion(ctx, sqlc.ListDataUpstreamProbesByHostRegionParams{
			HostRedactedHash: up.HostRedactedHash,
			Region:           region,
		})
		if err != nil {
			return err
		}
		rows = append(rows, fresh...)
	}
	bestRegion, bestMean := scoreForUpstreams(rows)
	if bestRegion == "" {
		return nil
	}
	u.mu.Lock()
	u.m[appID] = upstreamAffinityEntry{
		preferredRegion: bestRegion,
		scoreMs:         bestMean,
		observedAt:      u.now(),
	}
	u.mu.Unlock()
	return nil
}

// scoreForUpstreams is the pure-function core. Returns
// (preferredRegion, meanMs) — empty region when the slice has
// no ok=true rows. Exposed as a test seam so
// placement_test.go can drive the bias-INSERT comparator with
// canned probe rows.
func scoreForUpstreams(rows []state.DataUpstreamProbe) (string, int) {
	if len(rows) == 0 {
		return "", 0
	}
	regionScores := map[string]int{}
	regionCount := map[string]int{}
	for _, row := range rows {
		if !row.OK || row.RTTMs == nil {
			continue
		}
		regionScores[row.Region] += *row.RTTMs
		regionCount[row.Region]++
	}
	var (
		bestRegion string
		bestMean   = -1
	)
	for region, sum := range regionScores {
		count := regionCount[region]
		if count == 0 {
			continue
		}
		mean := sum / count
		if bestMean == -1 || mean < bestMean {
			bestMean = mean
			bestRegion = region
		}
	}
	return bestRegion, bestMean
}