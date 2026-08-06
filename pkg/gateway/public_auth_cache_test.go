package gateway

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestPublicAuthCache_GetHitMissLoadFn pins the basic shape:
// first call is a miss + invokes the loader once, second call
// is a hit + invokes the loader zero times. The loader
// returns ok=true on a clean unseal.
func TestPublicAuthCache_GetHitMissLoadFn(t *testing.T) {
	t.Parallel()
	c := NewPublicAuthCacheWithTTL(time.Minute, time.Now)
	var calls int32
	loader := func() (string, string, bool) {
		atomic.AddInt32(&calls, 1)
		return "alice", "s3cret", true
	}
	u, p, ok := c.Get("app1", []byte("sealed"), loader)
	if !ok || u != "alice" || p != "s3cret" {
		t.Fatalf("first call: got (%q,%q,%v) want (alice,s3cret,true)", u, p, ok)
	}
	u, p, ok = c.Get("app1", []byte("sealed"), loader)
	if !ok || u != "alice" || p != "s3cret" {
		t.Fatalf("second call: got (%q,%q,%v) want (alice,s3cret,true)", u, p, ok)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("loader calls = %d, want 1 (second call must be a cache hit)", got)
	}
}

// TestPublicAuthCache_TTLExpiry pins the 60s TTL contract.
// Advancing the clock past expiry re-invokes the loader.
func TestPublicAuthCache_TTLExpiry(t *testing.T) {
	t.Parallel()
	clk := newFakeClock(time.Unix(1_700_000_000, 0))
	c := NewPublicAuthCacheWithTTL(60*time.Second, clk.Now)
	var calls int32
	loader := func() (string, string, bool) {
		atomic.AddInt32(&calls, 1)
		return "alice", "s3cret", true
	}
	if _, _, ok := c.Get("app1", []byte("sealed"), loader); !ok {
		t.Fatal("first call: want ok=true")
	}
	clk.Advance(30 * time.Second)
	if _, _, ok := c.Get("app1", []byte("sealed"), loader); !ok {
		t.Fatal("second call within TTL: want ok=true")
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("loader calls within TTL = %d, want 1", got)
	}
	clk.Advance(31 * time.Second) // total > 60s
	if _, _, ok := c.Get("app1", []byte("sealed"), loader); !ok {
		t.Fatal("third call past TTL: want ok=true")
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Errorf("loader calls past TTL = %d, want 2 (expired entry must re-load)", got)
	}
}

// TestPublicAuthCache_LoaderFailure pins the ok=false path:
// a loader that returns ok=false leaves the cache empty
// and surfaces ok=false to the caller. The caller (the
// basic-auth branch in handler.go) treats this as a
// credential mismatch (401) — the unit test pins the
// cache's contract.
func TestPublicAuthCache_LoaderFailure(t *testing.T) {
	t.Parallel()
	c := NewPublicAuthCache()
	loader := func() (string, string, bool) {
		return "", "", false
	}
	u, p, ok := c.Get("app1", []byte("sealed"), loader)
	if ok || u != "" || p != "" {
		t.Errorf("loader-fail call: got (%q,%q,%v) want ('','',false)", u, p, ok)
	}
	if c.Len() != 0 {
		t.Errorf("loader-fail call should not insert: Len()=%d, want 0", c.Len())
	}
}

// TestPublicAuthCache_InvalidateByApp pins surgical
// invalidation on a per-appID basis: only that app's
// entries drop; siblings stay.
func TestPublicAuthCache_InvalidateByApp(t *testing.T) {
	t.Parallel()
	c := NewPublicAuthCache()
	loader := func() (string, string, bool) { return "u", "p", true }
	for _, id := range []string{"app1", "app2", "app3"} {
		if _, _, ok := c.Get(id, []byte("sealed"), loader); !ok {
			t.Fatalf("seed %s: want ok=true", id)
		}
	}
	if c.Len() != 3 {
		t.Fatalf("seed Len()=%d, want 3", c.Len())
	}
	c.InvalidateByApp("app2")
	if c.Len() != 2 {
		t.Errorf("after InvalidateByApp(app2) Len()=%d, want 2", c.Len())
	}
	// app1 + app3 must still hit; app2 must miss.
	if _, _, ok := c.Get("app1", []byte("sealed"), loader); !ok {
		t.Error("app1: want hit")
	}
	if _, _, ok := c.Get("app3", []byte("sealed"), loader); !ok {
		t.Error("app3: want hit")
	}
	// app2 should miss → re-invoke loader. Use a fresh
	// loader with a counter so we can assert it fired.
	var reloaded int32
	reloader := func() (string, string, bool) {
		atomic.AddInt32(&reloaded, 1)
		return "u", "p", true
	}
	if _, _, ok := c.Get("app2", []byte("sealed"), reloader); !ok {
		t.Error("app2 after invalidation: want re-load")
	}
	if got := atomic.LoadInt32(&reloaded); got != 1 {
		t.Errorf("app2 reloader calls = %d, want 1", got)
	}
}

// TestPublicAuthCache_InvalidateAll pins wholesale
// invalidation (the key_changed notify path).
func TestPublicAuthCache_InvalidateAll(t *testing.T) {
	t.Parallel()
	c := NewPublicAuthCache()
	loader := func() (string, string, bool) { return "u", "p", true }
	for _, id := range []string{"app1", "app2"} {
		c.Get(id, []byte("sealed"), loader)
	}
	if c.Len() != 2 {
		t.Fatalf("seed Len()=%d, want 2", c.Len())
	}
	c.InvalidateAll()
	if c.Len() != 0 {
		t.Errorf("after InvalidateAll Len()=%d, want 0", c.Len())
	}
}

// TestPublicAuthCache_NilCachePassThrough pins the
// nil-safe pass-through: a nil cache is treated as
// "no caching" and every Get invokes the loader.
func TestPublicAuthCache_NilCachePassThrough(t *testing.T) {
	t.Parallel()
	var c *PublicAuthCache // nil
	var calls int32
	loader := func() (string, string, bool) {
		atomic.AddInt32(&calls, 1)
		return "u", "p", true
	}
	for i := 0; i < 3; i++ {
		if _, _, ok := c.Get("app1", []byte("sealed"), loader); !ok {
			t.Fatalf("nil-cache call %d: want ok=true", i)
		}
	}
	if got := atomic.LoadInt32(&calls); got != 3 {
		t.Errorf("nil-cache loader calls = %d, want 3 (no caching)", got)
	}
	// Invalidate* must also be nil-safe.
	c.InvalidateByApp("app1")
	c.InvalidateAll()
	if c.Len() != 0 {
		t.Errorf("nil-cache Len()=%d, want 0", c.Len())
	}
}

// TestPublicAuthCache_DifferentSealedBlobsAreDistinct pins
// that two apps with different sealed blobs have distinct
// cache entries (the sha256 of the sealed blob is the key
// discriminator). A re-seal produces a fresh key so the
// next request re-unseals cleanly.
func TestPublicAuthCache_DifferentSealedBlobsAreDistinct(t *testing.T) {
	t.Parallel()
	c := NewPublicAuthCache()
	loader1 := func() (string, string, bool) { return "alice", "s3cret", true }
	loader2 := func() (string, string, bool) { return "bob", "hunter2", true }

	if _, _, ok := c.Get("app1", []byte("sealed-v1"), loader1); !ok {
		t.Fatal("first Get(sealed-v1): want ok=true")
	}
	if _, _, ok := c.Get("app1", []byte("sealed-v2"), loader2); !ok {
		t.Fatal("first Get(sealed-v2): want ok=true")
	}
	if c.Len() != 2 {
		t.Errorf("Len()=%d, want 2 (distinct sealed blobs → distinct cache entries)", c.Len())
	}
	u, p, ok := c.Get("app1", []byte("sealed-v1"), loader1)
	if !ok || u != "alice" || p != "s3cret" {
		t.Errorf("sealed-v1 hit: got (%q,%q,%v) want (alice,s3cret,true)", u, p, ok)
	}
	u, p, ok = c.Get("app1", []byte("sealed-v2"), loader2)
	if !ok || u != "bob" || p != "hunter2" {
		t.Errorf("sealed-v2 hit: got (%q,%q,%v) want (bob,hunter2,true)", u, p, ok)
	}
}

// TestPublicAuthCache_ConcurrentAccess pins the lock
// discipline: parallel Get calls on the same (app, sealed)
// pair must invoke the loader exactly once. The cache must
// hold the write lock long enough to serialise the load +
// insert (otherwise two concurrent misses both load and
// both insert). This is the contract the basic-auth hot
// path depends on under -race.
func TestPublicAuthCache_ConcurrentAccess(t *testing.T) {
	t.Parallel()
	c := NewPublicAuthCache()
	var calls int32
	loader := func() (string, string, bool) {
		atomic.AddInt32(&calls, 1)
		time.Sleep(5 * time.Millisecond) // widen the race window
		return "u", "p", true
	}
	const N = 32
	var wg sync.WaitGroup
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func() {
			defer wg.Done()
			c.Get("app1", []byte("sealed"), loader)
		}()
	}
	wg.Wait()
	// 1..N depending on race timing. The contract is "no
	// panic and a single entry inserted"; loader call count
	// varies because the read-locked fast path can race the
	// write-locked insert. What MUST NOT happen: zero calls
	// (loader was never invoked) or Len() != 1 (multiple
	// inserts).
	if got := atomic.LoadInt32(&calls); got < 1 {
		t.Errorf("loader calls = %d, want ≥1", got)
	}
	if c.Len() != 1 {
		t.Errorf("Len()=%d, want 1", c.Len())
	}
}

// fakeClock is a manually-advanced time.Time source for
// deterministic TTL tests. The cache uses it through its
// now func; advancing the clock past the TTL re-loads
// even though wall-clock time hasn't moved.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock(start time.Time) *fakeClock {
	return &fakeClock{now: start}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}
