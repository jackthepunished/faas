package targets

import (
	"sync"
	"time"
)

// RingBuffer is a per-app sliding-window max-inflight tracker
// (PR-C, issue #462). Each app owns a slice of (bucketIdx, max)
// tuples; on every Observe the buffer stores the most recent
// per-instance max-inflight value into the current bucket. The
// window is `windowSize` buckets of `bucketSize` each — the
// canonical config is 5 buckets × 1s = 5s window, matching
// api.ScaleUpWindowSeconds (mirrors pkg/sched/scaleup's RPS ring).
//
// Difference from pkg/sched/scaleup.RingBuffer: scaleup tracks
// per-app RPS sums (deltas on each Touch); targets tracks per-app
// max-inflight (one snapshot per Observe). The semantics are
// different — max-inflight is an instantaneous gauge, not a
// counter — so the ring just remembers the per-bucket max and
// returns the per-app max across the live window.
//
// The buffer is bounded: at most one entry per app per bucket, so
// the memory cost is O(numApps × windowSize). Cleanup of idle app
// entries is intentionally NOT implemented — apps come and go but
// the count is small (the platform caps at hundreds of apps per
// account, and only apps with concurrent_requests target feed the
// buffer in steady state).
//
// Concurrency: the buffer is safe for one reader (AppMaxInflight)
// and one writer (Observe) running concurrently. Multiple writers
// are NOT supported — Observe is called from schedd's loop
// goroutine only.
type RingBuffer struct {
	windowSize int
	bucketSize time.Duration
	tickInterval time.Duration

	mu sync.Mutex
	// byApp maps app_id → buffer. Each buffer is a fixed-size slice
	// of (bucketIdx, max) pairs; bucketIdx is the absolute bucket
	// number (for eviction by age). max is the per-instance max
	// observed during that bucket.
	byApp map[string]*appBuffer
}

// appBuffer is the per-app ring buffer. Held by value inside the
// map; the enclosing mutex serialises access.
type appBuffer struct {
	// buckets is the ring. buckets[i].bucket is the absolute bucket
	// number (monotonically increasing). buckets[i].max is the
	// per-instance max captured during that bucket.
	buckets []bucket
}

type bucket struct {
	bucket int64 // absolute bucket number (floor(now / bucketSize))
	max    int64 // per-instance max during this bucket
}

// NewRingBuffer constructs a RingBuffer. windowSize is the number of
// buckets retained; bucketSize is the bucket's duration.
// tickInterval is the cadence at which Observe will be called
// (currently advisory only — kept for parity with pkg/sched/scaleup's
// ring, which uses tickInterval to detect missed ticks).
func NewRingBuffer(windowSize int, bucketSize, tickInterval time.Duration) *RingBuffer {
	if windowSize <= 0 {
		windowSize = 1
	}
	if bucketSize <= 0 {
		bucketSize = time.Second
	}
	if tickInterval <= 0 {
		tickInterval = time.Second
	}
	return &RingBuffer{
		windowSize:   windowSize,
		bucketSize:   bucketSize,
		tickInterval: tickInterval,
		byApp:        map[string]*appBuffer{},
	}
}

// Observe stores the per-instance max-inflight value for appID at
// the current bucket. If a record already exists for this bucket,
// the value is updated to the max of (existing, new) so an Observe
// within the same bucket aggregates correctly. If the app is not
// yet known, a fresh buffer is allocated.
//
// Observe is safe for one concurrent caller (the loop's tick
// goroutine). Multiple writers are not synchronised — the trigger
// is the only owner.
func (r *RingBuffer) Observe(now time.Time, appID string, inflight int64) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	currentBucket := now.UnixNano() / int64(r.bucketSize)
	buf, ok := r.byApp[appID]
	if !ok {
		r.byApp[appID] = &appBuffer{
			buckets: []bucket{{bucket: currentBucket, max: inflight}},
		}
		return
	}
	last := len(buf.buckets) - 1
	if last >= 0 && buf.buckets[last].bucket == currentBucket {
		if inflight > buf.buckets[last].max {
			buf.buckets[last].max = inflight
		}
		return
	}
	// New bucket — append.
	buf.buckets = append(buf.buckets, bucket{
		bucket: currentBucket,
		max:    inflight,
	})
	// Evict buckets older than the window. The window is
	// windowSize buckets inclusive of the current one (mirrors
	// pkg/sched/scaleup's `>= cutoff`): buckets strictly less
	// than `currentBucket - windowSize + 1` are stale.
	cutoff := currentBucket - int64(r.windowSize) + 1
	first := 0
	for first < len(buf.buckets) && buf.buckets[first].bucket < cutoff {
		first++
	}
	if first > 0 {
		buf.buckets = buf.buckets[first:]
	}
}

// AppMaxInflight returns the per-app max-inflight across all
// buckets in the window. Returns (0, false) when the buffer has no
// observation for appID.
func (r *RingBuffer) AppMaxInflight(appID string, now time.Time) (int64, bool) {
	if r == nil {
		return 0, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	buf, ok := r.byApp[appID]
	if !ok || len(buf.buckets) == 0 {
		return 0, false
	}
	currentBucket := now.UnixNano() / int64(r.bucketSize)
	cutoff := currentBucket - int64(r.windowSize) + 1
	var max int64
	var found bool
	for _, b := range buf.buckets {
		if b.bucket >= cutoff {
			if !found || b.max > max {
				max = b.max
				found = true
			}
		}
	}
	if !found {
		return 0, false
	}
	return max, true
}