// upstream_affinity_test.go — connection-aware placement bias
// regression tests (ADR-098 §9.A PR-D). Three load-bearing
// claims:
//
//   - Fail-open: a cold cache (no probe yet, no captured
//     upstreams) returns ok=false → chooser falls back to the
//     legacy tie-break (no false preference).
//   - TTL expiry: a stale entry is evicted on read so a
//     recovered meterd probe re-populates the cache.
//   - scoreForUpstreams pure-function correctness: the
//     region with the lowest mean RTT wins.

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
	region, score, ok := u.Score("app-1")
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
	// Manually stamp an entry that's now in the past.
	u.mu.Lock()
	u.m["app-1"] = upstreamAffinityEntry{
		preferredRegion: "us-east",
		scoreMs:         12,
		observedAt:      clock.Add(-1 * time.Second), // 1s old, TTL = 100ms
	}
	u.mu.Unlock()
	region, score, ok := u.Score("app-1")
	if ok {
		t.Errorf("Score = (%q, %d, true), want ok=false on stale entry", region, score)
	}
	// Eviction must have happened.
	u.mu.RLock()
	_, present := u.m["app-1"]
	u.mu.RUnlock()
	if present {
		t.Errorf("stale entry not evicted")
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

func (s *fakeUpstreamAffinityStore) ListAppUpstreamProbeScores(_ context.Context, _, _ string) ([]state.AppUpstreamProbeScore, error) {
	return nil, nil
}
