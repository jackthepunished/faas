// upstream_affinity_test.go — connection-aware placement bias
// regression tests (ADR-098 §9.A PR-D + amendment issue #954).
// Four load-bearing claims:
//
//   - Fail-open: a cold cache (no probe yet, no captured
//     upstreams) returns ok=false → chooser falls back to the
//     legacy tie-break (no false preference).
//   - TTL expiry: a stale entry is evicted on read so a
//     recovered meterd probe re-populates the cache.
//   - scoreForUpstreams pure-function correctness: the
//     region with the lowest mean RTT wins.
//   - Per-deployment cache isolation (ADR-098 amendment issue
//     #954): the cache key widens to (appID, deploymentScope);
//     staging and production deployments of the same app must
//     read independent probe bias.

package sched

import (
	"context"
	"testing"
	"time"

	"github.com/onebox-faas/faas/pkg/state"
)

// TestScore_FailsOpenOnEmpty asserts that Score returns ok=false
// when the cache is cold (no entry yet, no captured upstreams).
func TestScore_FailsOpenOnEmpty(t *testing.T) {
	u := NewUpstreamAffinity(time.Minute, nil)
	region, score, ok := u.Score("app-1", defaultDeploymentScope)
	if ok {
		t.Errorf("Score = (%q, %d, true), want ok=false on cold cache", region, score)
	}
}

// TestScore_TTLExpiry asserts that a stale entry is evicted on
// read.
func TestScore_TTLExpiry(t *testing.T) {
	u := NewUpstreamAffinity(100*time.Millisecond, &fakeUpstreamAffinityStore{})
	clock := time.Unix(1700000000, 0)
	u.now = func() time.Time { return clock }
	// Manually stamp an entry that's now in the past. Uses the
	// composite (appID, deploymentScope) key so the regression
	// also pins the issue #954 widening.
	u.mu.Lock()
	u.m[appDeploymentKeyOf("app-1", defaultDeploymentScope)] = upstreamAffinityEntry{
		preferredRegion: "us-east",
		scoreMs:         12,
		observedAt:      clock.Add(-1 * time.Second), // 1s old, TTL = 100ms
	}
	u.mu.Unlock()
	region, score, ok := u.Score("app-1", defaultDeploymentScope)
	if ok {
		t.Errorf("Score = (%q, %d, true), want ok=false on stale entry", region, score)
	}
	// Eviction must have happened.
	u.mu.RLock()
	_, present := u.m[appDeploymentKeyOf("app-1", defaultDeploymentScope)]
	u.mu.RUnlock()
	if present {
		t.Errorf("stale entry not evicted")
	}
}

// TestScore_PerDeploymentIsolation (ADR-098 amendment issue #954)
// asserts that staging and production deployments of the same
// app read independent probe bias. A refresh keyed on
// deploymentScope="staging" must NOT shadow or overwrite the
// production entry — staging wakes bias toward staging, prod
// wakes bias toward prod, both keyed off the same appID.
func TestScore_PerDeploymentIsolation(t *testing.T) {
	u := NewUpstreamAffinity(time.Minute, &fakeUpstreamAffinityStore{})
	clock := time.Unix(1700000000, 0)
	u.now = func() time.Time { return clock }

	// Stamp two entries: one for the staging deployment, one
	// for production. The keys MUST differ (composite
	// appID+NUL+deploymentScope) so the staging refresh can't
	// land on the production entry.
	u.mu.Lock()
	u.m[appDeploymentKeyOf("app-1", "staging")] = upstreamAffinityEntry{
		preferredRegion: "eu-fra",
		scoreMs:         40,
		observedAt:      clock,
	}
	u.m[appDeploymentKeyOf("app-1", "production")] = upstreamAffinityEntry{
		preferredRegion: "us-east",
		scoreMs:         80,
		observedAt:      clock,
	}
	u.mu.Unlock()

	region, score, ok := u.Score("app-1", "staging")
	if !ok {
		t.Fatalf("Score(staging) = ok=false; want staging entry to be present")
	}
	if region != "eu-fra" || score != 40 {
		t.Errorf("Score(staging) = (%q, %d), want (eu-fra, 40)", region, score)
	}

	region, score, ok = u.Score("app-1", "production")
	if !ok {
		t.Fatalf("Score(production) = ok=false; want production entry to be present")
	}
	if region != "us-east" || score != 80 {
		t.Errorf("Score(production) = (%q, %d), want (us-east, 80)", region, score)
	}

	// A query for an unseen deployment ("canary") must fail
	// open, not alias to one of the stamped entries.
	_, _, ok = u.Score("app-1", "canary")
	if ok {
		t.Errorf("Score(canary) = ok=true; want ok=false (no cross-deployment bleed)")
	}
}

// TestScore_DefaultFallback asserts that an empty deploymentScope
// string falls back to defaultDeploymentScope (matches the SQL
// DEFAULT 'default' on data_upstreams.deployment_scope, migration
// 00281). Legacy callers that haven't threaded dep.ID still hit
// the existing default-scoped cache key.
func TestScore_DefaultFallback(t *testing.T) {
	u := NewUpstreamAffinity(time.Minute, &fakeUpstreamAffinityStore{})
	clock := time.Unix(1700000000, 0)
	u.now = func() time.Time { return clock }

	u.mu.Lock()
	u.m[appDeploymentKeyOf("app-1", defaultDeploymentScope)] = upstreamAffinityEntry{
		preferredRegion: "us-east",
		scoreMs:         80,
		observedAt:      clock,
	}
	u.mu.Unlock()

	// Empty-string fallback to defaultDeploymentScope — the
	// cold-path wake without dep.ID.
	region, score, ok := u.Score("app-1", "")
	if !ok {
		t.Fatalf("Score(\"\") = ok=false; want ok=true via defaultDeploymentScope fallback")
	}
	if region != "us-east" || score != 80 {
		t.Errorf("Score(\"\") = (%q, %d), want (us-east, 80)", region, score)
	}
}

// TestScoreForUpstreams_RegionLowestMeanWins asserts the
// pure-function scoring: the region with the lowest mean RTT
// across the app's captured upstreams wins.
func TestScoreForUpstreams_RegionLowestMeanWins(t *testing.T) {
	rows := []state.DataUpstreamProbe{
		{Region: "eu-fra", OK: true, RTTMs: ptrInt(40)},
		{Region: "eu-fra", OK: true, RTTMs: ptrInt(60)},
		{Region: "us-east", OK: true, RTTMs: ptrInt(80)},
		{Region: "us-east", OK: true, RTTMs: ptrInt(100)},
		{Region: "ap-tokyo", OK: true, RTTMs: ptrInt(200)},
	}
	region, mean := scoreForUpstreams(rows)
	if region != "eu-fra" {
		t.Errorf("region = %q, want eu-fra (lowest mean RTT)", region)
	}
	if mean != 50 { // (40 + 60) / 2
		t.Errorf("mean = %d, want 50", mean)
	}
}

// TestScoreForUpstreams_SkipsFailing asserts the ok=false +
// RTTMs-nil rows are excluded from the mean computation.
func TestScoreForUpstreams_SkipsFailing(t *testing.T) {
	rows := []state.DataUpstreamProbe{
		{Region: "eu-fra", OK: true, RTTMs: ptrInt(40)},
		{Region: "us-east", OK: false, RTTMs: ptrInt(5)}, // ignored
		{Region: "us-east", OK: true, RTTMs: nil},        // NULL RTT
		{Region: "us-east", OK: true, RTTMs: ptrInt(80)},
	}
	region, mean := scoreForUpstreams(rows)
	if region != "eu-fra" {
		t.Errorf("region = %q, want eu-fra (us-east has only 1 valid probe = 80ms)", region)
	}
	if mean != 40 {
		t.Errorf("mean = %d, want 40", mean)
	}
}

// TestScoreForUpstreams_EmptyReturnsEmpty asserts the fail-open
// path on an empty slice.
func TestScoreForUpstreams_EmptyReturnsEmpty(t *testing.T) {
	region, mean := scoreForUpstreams(nil)
	if region != "" || mean != 0 {
		t.Errorf("scoreForUpstreams(nil) = (%q, %d), want (\"\", 0)", region, mean)
	}
}

// fakeUpstreamAffinityStore satisfies the UpstreamAffinityStore
// interface. We only need the stubs to compile; the
// ListAppUpstreamProbeScores path is exercised only when
// Refresh runs end-to-end (not covered here — Refresh is an
// integration concern, pinned by the C9 e2e).
type fakeUpstreamAffinityStore struct{}

func (s *fakeUpstreamAffinityStore) ListAppUpstreamProbeScores(_ context.Context, _, _, _ string) ([]state.AppUpstreamProbeScore, error) {
	return nil, nil
}
