// upstream_affinity.go — schedd's connection-aware placement bias
// (ADR-098 §9.A PR-D + amendment issue #954).
//
// The meterd probe loop (pkg/meter/upstream_probe.go) writes one
// data_upstream_probes row per (host_redacted_hash, kind, port,
// region, sampled_at) every 30 s. On wake, schedd reads the most
// recent probe row PER region for the app's captured upstreams,
// picks the region with the lowest observed RTT, and biases the
// chooser toward a node in that region.
//
// Per-deployment cache isolation (ADR-098 amendment issue #954):
// the cache key widens to (appID, deploymentScope) so staging
// and production deployments of the same app read independent
// probe bias. The dedupe key on data_upstreams widens to
// (app_id, scope, deployment_scope, kind, host, port) — see
// migration 00281. pgstore.ListAppUpstreamProbeScopes (the JOIN-
// collapsed read at refresh time) scopes its probe scan via the
// deployment_scope predicate; the in-process map keys on
// appDeploymentKeyOf(appID, deploymentScope) so a staging refresh
// cannot shadow a production entry.
//
// Pipe-payload forward-compat: the data_upstreams_changed
// notification now carries 7 fields (was 6) —
// app_id|scope|deployment_scope|kind|host|port|op. schedd does
// NOT currently LISTEN on data_upstreams_changed (per ADR §D2
// the chooser reads synchronously at wake), so no parser lives
// in this package today. A future LISTEN consumer must parse
// the 7-field layout — splitting on "|" yields 7 tokens, the
// 3rd being deployment_scope.
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
//   - ListAppUpstreamProbeScores returns one row per
//     (data_upstreams.id, region) for the app, with the
//     freshest probe's RTT. Single round-trip on the wake
//     path (replaces the legacy N+1 design — see
//     pkg/state/pgstore.go::ListAppUpstreamProbeScores). ADR-098
//     amendment (issue #954) scopes the probe scan to one
//     deployment_scope so staging-vs-prod bias doesn't bleed
//     across deployments.
type UpstreamAffinityStore interface {
	ListAppUpstreamProbeScores(ctx context.Context, accountID, appID, deploymentScope string) ([]state.AppUpstreamProbeScore, error)
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
// (app, deployment) tuple. ok=false ⇒ the cache is cold/stale/
// empty; the caller (the wake path) must fail open and not bias
// the chooser.
//
// The read path takes the RLock; on a hit, the entry's
// observedAt is compared against now — a stale entry returns
// ok=false (and is evicted) so the next Score() triggers a
// Refresh. ADR-098 amendment (issue #954) widens the cache key
// to (appID, deploymentScope) so each deployment reads its own
// probe bias — staging and prod stay independent.
func (u *UpstreamAffinity) Score(appID, deploymentScope string) (region string, scoreMs int, ok bool) {
	if u == nil {
		return "", 0, false
	}
	key := appDeploymentKeyOf(appID, deploymentScope)
	u.mu.RLock()
	entry, found := u.m[key]
	u.mu.RUnlock()
	if !found {
		return "", 0, false
	}
	if u.now().Sub(entry.observedAt) > u.ttl {
		u.mu.Lock()
		delete(u.m, key)
		u.mu.Unlock()
		return "", 0, false
	}
	return entry.preferredRegion, entry.scoreMs, true
}

// appDeploymentKeyOf is the composite cache key for
// UpstreamAffinity. Matches the pkg/sched/admission.go:294
// per-deployment ledger shape (single string concat with the
// NUL separator so two appIDs cannot alias by substring).
// ADR-098 amendment (issue #954) widens the key from appID
// alone to (appID, deploymentScope).
const appDeploymentKeySep = "\x00"

func appDeploymentKeyOf(appID, deploymentScope string) string {
	if deploymentScope == "" {
		// defaultDeploymentScope is the cold-path fallback for
		// legacy wake callers (pkg/sched/engine.go passes
		// "default" when dep is nil). Mirror the SQL DEFAULT
		// stamp on the deployment_scope column.
		deploymentScope = defaultDeploymentScope
	}
	return appID + appDeploymentKeySep + deploymentScope
}

// defaultDeploymentScope is the deployment-scope stamp every pre-
// #954 row + every single-deployment app carries. Mirrors the SQL
// DEFAULT 'default' on data_upstreams.deployment_scope (migration
// 00281) so a wake that hasn't threaded dep.ID lands on the same
// key the apid writer used at INSERT time.
const defaultDeploymentScope = "default"

// Refresh reads the data_upstreams + data_upstream_probes rows
// for the app, picks the region with the lowest mean RTT across
// the app's captured upstreams, and stores the result in the
// cache. Returns ok=false when the app has no captured upstreams
// or no probe rows; the cache is left unchanged (no churn).
//
// The read is a single PG round-trip via the JOIN-collapsed
// ListAppUpstreamProbeScores query (replaces the legacy N+1
// design that ran 1 ListAllAppDataUpstreams + N
// ListDataUpstreamProbesByHostRegion queries per wake — for a
// Scale plan app with DataPlacementHintsPerApp=50, that's 51
// round-trips on the wake goroutine under appMu, far past
// the < 1 ms wake-budget claim).
//
// accountID is required because pgstore.ListAppUpstreamProbeScores
// is keyed on (account_id, app_id). appID is the string form
// passed at the wake call site (engine.app.id is already a
// string at that point). deploymentScope scopes the probe scan
// to a single customer deployment (ADR-098 amendment issue
// #954); empty string falls back to defaultDeploymentScope via
// appDeploymentKeyOf so a cold-path wake without depID still
// reads the existing default-scoped bias.
func (u *UpstreamAffinity) Refresh(ctx context.Context, accountID, appID, deploymentScope string) error {
	if u == nil || u.store == nil {
		return nil
	}
	scores, err := u.store.ListAppUpstreamProbeScores(ctx, accountID, appID, deploymentScope)
	if err != nil {
		return err
	}
	if len(scores) == 0 {
		return nil
	}
	// Adapter: the JOIN-collapsed rows are shaped like the
	// legacy DataUpstreamProbe struct so the pure-function
	// scoreForUpstreams (placement-go's comparator) doesn't
	// need to change. The OK + RTTMs pair is the
	// scoreForUpstreams filter — same contract as before.
	rows := make([]state.DataUpstreamProbe, 0, len(scores))
	for _, s := range scores {
		row := state.DataUpstreamProbe{
			HostRedactedHash: s.HostRedactedHash,
			Region:           s.Region,
			Kind:             s.Kind,
			OK:               s.OK,
			RTTMs:            s.RTTMs,
		}
		rows = append(rows, row)
	}
	bestRegion, bestMean := scoreForUpstreams(rows)
	if bestRegion == "" {
		return nil
	}
	u.mu.Lock()
	u.m[appDeploymentKeyOf(appID, deploymentScope)] = upstreamAffinityEntry{
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
