// cache_test.go — ADR-054 §2 read-through cache test surface.
//
// Pins the four behaviors the LocalCacheBackend has to honor:
//
//  1. Put-then-Get round-trips: data written through the cache is
//     readable from the cache without touching the parent.
//  2. Cache miss falls back to parent and populates: a Get on a
//     cold cache reads from the parent + caches the blob for the
//     next call.
//  3. LRU eviction by mtime: when total size exceeds maxBytes, the
//     oldest entries are evicted first.
//  4. List recovers original storage keys via sidecar: the on-disk
//     hash-derived filename is invisible to callers; List returns
//     the keys the caller originally Put.
//
// Plus the integration seam: wrapping a PrefixRouter with a cache
// keeps routing intact (snap/ → local, apps/ → OCI stub) and adds
// the read-through behaviour on every per-route Get.

package storage_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/onebox-faas/faas/pkg/storage"
)

// fakeBackend is a minimal in-memory StorageBackend for cache
// tests. Tracks Put/Get/Delete counts so the suite can assert
// the cache hit/miss path actually short-circuited.
type fakeBackend struct {
	puts    atomic.Int64
	gets    atomic.Int64
	deletes atomic.Int64
	blobs   map[string][]byte
	// onGet, if non-nil, runs before reading blobs. Lets tests
	// simulate transient registry failures (ADR-054 §2's
	// motivating case: a registry outage must not brick cold
	// boots when the cache is warm).
	onGet func(key string) error
}

func newFakeBackend() *fakeBackend {
	return &fakeBackend{blobs: map[string][]byte{}}
}

func (f *fakeBackend) Put(_ context.Context, key string, r io.Reader) error {
	f.puts.Add(1)
	data, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	f.blobs[key] = data
	return nil
}

func (f *fakeBackend) Get(_ context.Context, key string) (io.ReadCloser, error) {
	f.gets.Add(1)
	if f.onGet != nil {
		if err := f.onGet(key); err != nil {
			return nil, err
		}
	}
	data, ok := f.blobs[key]
	if !ok {
		return nil, storage.ErrNotFound
	}
	return io.NopCloser(strings.NewReader(string(data))), nil
}

func (f *fakeBackend) Delete(_ context.Context, key string) error {
	f.deletes.Add(1)
	delete(f.blobs, key)
	return nil
}

// TestLocalCacheBackend_PutGetRoundTrip pins the basic read-through
// contract: a Put writes through to parent + caches; a Get reads
// from cache without touching the parent.
func TestLocalCacheBackend_PutGetRoundTrip(t *testing.T) {
	tmp := t.TempDir()
	parent := newFakeBackend()
	cache, err := storage.NewLocalCacheBackend(parent, filepath.Join(tmp, "cache"), 0)
	if err != nil {
		t.Fatalf("NewLocalCacheBackend: %v", err)
	}
	ctx := context.Background()
	if err := cache.Put(ctx, "snap/abc", strings.NewReader("blob-data")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	// First Get hits cache (parent.Get count stays at 0).
	got, err := readAll(ctx, cache, "snap/abc")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got != "blob-data" {
		t.Errorf("Get = %q, want %q", got, "blob-data")
	}
	if parent.puts.Load() != 1 {
		t.Errorf("parent.puts = %d, want 1", parent.puts.Load())
	}
	if parent.gets.Load() != 0 {
		t.Errorf("parent.gets = %d, want 0 (cache should serve)", parent.gets.Load())
	}
}

// TestLocalCacheBackend_CacheMissFallsBackToParent pins the
// read-through behavior: a Get on a cold cache reads from the
// parent + populates the cache.
func TestLocalCacheBackend_CacheMissFallsBackToParent(t *testing.T) {
	tmp := t.TempDir()
	parent := newFakeBackend()
	cache, err := storage.NewLocalCacheBackend(parent, filepath.Join(tmp, "cache"), 0)
	if err != nil {
		t.Fatalf("NewLocalCacheBackend: %v", err)
	}
	ctx := context.Background()
	// Pre-seed parent only (skip the cache write path).
	if err := parent.Put(ctx, "snap/abc", strings.NewReader("from-parent")); err != nil {
		t.Fatalf("parent.Put: %v", err)
	}
	// Cold cache: Get goes to parent + populates cache.
	got, err := readAll(ctx, cache, "snap/abc")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got != "from-parent" {
		t.Errorf("Get = %q, want %q", got, "from-parent")
	}
	if parent.gets.Load() != 1 {
		t.Errorf("parent.gets after first Get = %d, want 1", parent.gets.Load())
	}
	// Second Get should hit cache (parent untouched).
	got2, err := readAll(ctx, cache, "snap/abc")
	if err != nil {
		t.Fatalf("Get #2: %v", err)
	}
	if got2 != "from-parent" {
		t.Errorf("Get #2 = %q, want %q", got2, "from-parent")
	}
	if parent.gets.Load() != 1 {
		t.Errorf("parent.gets after second Get = %d, want 1 (cache hit)", parent.gets.Load())
	}
}

// TestLocalCacheBackend_ParentFailureSurfaces pins the failure
// mode: when the parent backend fails (e.g. a registry outage),
// Get surfaces the wrapped error verbatim. The cache does NOT
// fall back to a stale entry — ADR-054 §2 keeps that contract
// explicit; the "registry unreachable" gate at startup is a
// separate seam that callers use to opt in to stale-fallback.
func TestLocalCacheBackend_ParentFailureSurfaces(t *testing.T) {
	tmp := t.TempDir()
	parent := newFakeBackend()
	parent.onGet = func(_ string) error {
		return errors.New("registry unreachable")
	}
	cache, err := storage.NewLocalCacheBackend(parent, filepath.Join(tmp, "cache"), 0)
	if err != nil {
		t.Fatalf("NewLocalCacheBackend: %v", err)
	}
	_, err = cache.Get(context.Background(), "snap/abc")
	if err == nil {
		t.Fatal("Get = nil; want error from parent failure")
	}
	if !strings.Contains(err.Error(), "registry unreachable") {
		t.Errorf("err = %q, want it to wrap 'registry unreachable'", err)
	}
}

// TestLocalCacheBackend_ListRecoversOriginalKey pins the sidecar
// metadata contract: List returns the original storage keys, not
// the hash-derived filenames on disk. This is the seam GC code
// paths (pkg/imaged, pkg/sched/disk_drift) rely on.
func TestLocalCacheBackend_ListRecoversOriginalKey(t *testing.T) {
	tmp := t.TempDir()
	parent := newFakeBackend()
	cache, err := storage.NewLocalCacheBackend(parent, filepath.Join(tmp, "cache"), 0)
	if err != nil {
		t.Fatalf("NewLocalCacheBackend: %v", err)
	}
	ctx := context.Background()
	keys := []string{
		"snap/a",
		"snap/b",
		"layers/l1",
		"layers/l2",
	}
	for _, k := range keys {
		if err := cache.Put(ctx, k, strings.NewReader("data-"+k)); err != nil {
			t.Fatalf("Put %q: %v", k, err)
		}
	}
	got, err := cache.List(ctx, "")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	sort.Strings(got)
	want := []string{"layers/l1", "layers/l2", "snap/a", "snap/b"}
	if !equalSlices(got, want) {
		t.Errorf("List = %v, want %v", got, want)
	}
	// Sub-prefix filtering.
	got, err = cache.List(ctx, "layers/")
	if err != nil {
		t.Fatalf("List layers/: %v", err)
	}
	sort.Strings(got)
	if !equalSlices(got, []string{"layers/l1", "layers/l2"}) {
		t.Errorf("List layers/ = %v, want [layers/l1 layers/l2]", got)
	}
}

// TestLocalCacheBackend_LRUEvictsOldest pins the byte-budgeted
// eviction policy: when total size exceeds maxBytes, the oldest
// entries by mtime are evicted first. The test seeds three blobs,
// sets a tight budget that fits only two, then writes a fourth to
// trigger eviction.
func TestLocalCacheBackend_LRUEvictsOldest(t *testing.T) {
	tmp := t.TempDir()
	parent := newFakeBackend()
	// 10 bytes per blob, budget 25 bytes → at most 2 blobs.
	cache, err := storage.NewLocalCacheBackend(parent, filepath.Join(tmp, "cache"), 25)
	if err != nil {
		t.Fatalf("NewLocalCacheBackend: %v", err)
	}
	ctx := context.Background()
	// Use distinct mtimes so eviction is deterministic.
	for _, k := range []string{"a", "b", "c"} {
		if err := cache.Put(ctx, "snap/"+k, strings.NewReader(strings.Repeat("x", 10))); err != nil {
			t.Fatalf("Put %q: %v", k, err)
		}
		// Stagger mtimes 1s apart so LRU ordering is stable.
		// Chtimes overrides whatever the cache's readCache
		// touch-up set on the file.
		path := hashCachePath(t, filepath.Join(tmp, "cache"), "snap/"+k)
		past := time.Now().Add(-1 * time.Hour).Add(time.Duration(k[0]-'a') * time.Second)
		if err := os.Chtimes(path, past, past); err != nil {
			t.Fatalf("Chtimes: %v", err)
		}
	}
	// Write the 4th blob — 40 bytes total > 25 byte budget,
	// so the two oldest by mtime (a, b) are evicted; c, d remain.
	if err := cache.Put(ctx, "snap/d", strings.NewReader(strings.Repeat("x", 10))); err != nil {
		t.Fatalf("Put d: %v", err)
	}
	keys, err := cache.List(ctx, "")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	sort.Strings(keys)
	want := []string{"snap/c", "snap/d"}
	if !equalSlices(keys, want) {
		t.Errorf("List after eviction = %v, want %v (eviction order is oldest-first)", keys, want)
	}
}

// TestLocalCacheBackend_DeleteEvictsAndForwards pins the
// delete-propagation contract: a Delete evicts the cache entry
// + forwards to the parent.
func TestLocalCacheBackend_DeleteEvictsAndForwards(t *testing.T) {
	tmp := t.TempDir()
	parent := newFakeBackend()
	cache, err := storage.NewLocalCacheBackend(parent, filepath.Join(tmp, "cache"), 0)
	if err != nil {
		t.Fatalf("NewLocalCacheBackend: %v", err)
	}
	ctx := context.Background()
	if err := cache.Put(ctx, "snap/abc", strings.NewReader("data")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := cache.Delete(ctx, "snap/abc"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if parent.deletes.Load() != 1 {
		t.Errorf("parent.deletes = %d, want 1", parent.deletes.Load())
	}
	// After eviction, List should not include the key.
	keys, err := cache.List(ctx, "")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, k := range keys {
		if k == "snap/abc" {
			t.Errorf("snap/abc still in cache after Delete: keys=%v", keys)
		}
	}
}

// TestLocalCacheBackend_NilParentRejected pins the constructor's
// nil-parent guard: a nil parent is rejected up front, not at
// runtime when the first Put/Get tries to use it.
func TestLocalCacheBackend_NilParentRejected(t *testing.T) {
	tmp := t.TempDir()
	if _, err := storage.NewLocalCacheBackend(nil, filepath.Join(tmp, "cache"), 0); err == nil {
		t.Fatal("NewLocalCacheBackend(nil, …) = nil err; want error")
	}
}

// TestLocalCacheBackend_EmptyRootRejected pins the empty-root
// guard: an empty cache dir is a configuration error, not a
// permissive default that puts the cache at the wrong path.
func TestLocalCacheBackend_EmptyRootRejected(t *testing.T) {
	parent := newFakeBackend()
	if _, err := storage.NewLocalCacheBackend(parent, "", 0); err == nil {
		t.Fatal("NewLocalCacheBackend(_, \"\", _) = nil err; want error")
	}
}

// TestLocalCacheBackend_WrapsPrefixRouter pins the integration
// seam: wrapping a PrefixRouter with a cache keeps routing
// intact (snap/ → local, apps/ → OCI stub) and adds read-through
// on every per-route Get.
func TestLocalCacheBackend_WrapsPrefixRouter(t *testing.T) {
	tmp := t.TempDir()
	local, err := storage.NewLocalStorageBackend(filepath.Join(tmp, "fc"))
	if err != nil {
		t.Fatalf("NewLocalStorageBackend local: %v", err)
	}
	oci := newFakeBackend()
	router, err := storage.NewPrefixRouter(
		map[string]storage.StorageBackend{
			"snap/":   local,
			"apps/":   oci,
			"base/":   local,
			"kernel/": local,
			"layers/": local,
		},
		nil,
	)
	if err != nil {
		t.Fatalf("NewPrefixRouter: %v", err)
	}
	cache, err := storage.NewLocalCacheBackend(router, filepath.Join(tmp, "cache"), 0)
	if err != nil {
		t.Fatalf("NewLocalCacheBackend: %v", err)
	}
	ctx := context.Background()
	// snap/ lands in the local backend.
	if err := cache.Put(ctx, "snap/abc", strings.NewReader("snap-data")); err != nil {
		t.Fatalf("Put snap/abc: %v", err)
	}
	got, err := readAll(ctx, cache, "snap/abc")
	if err != nil {
		t.Fatalf("Get snap/abc: %v", err)
	}
	if got != "snap-data" {
		t.Errorf("snap/abc = %q, want %q", got, "snap-data")
	}
	// apps/ lands in the OCI stub (the router routes it).
	if err := cache.Put(ctx, "apps/acme/x", strings.NewReader("apps-data")); err != nil {
		t.Fatalf("Put apps/acme/x: %v", err)
	}
	got, err = readAll(ctx, cache, "apps/acme/x")
	if err != nil {
		t.Fatalf("Get apps/acme/x: %v", err)
	}
	if got != "apps-data" {
		t.Errorf("apps/acme/x = %q, want %q", got, "apps-data")
	}
	// The cache hit-path must short-circuit the OCI backend.
	if oci.gets.Load() != 0 {
		t.Errorf("oci.gets = %d, want 0 (cache should serve)", oci.gets.Load())
	}
}

// TestLocalCacheBackend_DeterministicBucketing pins the
// bucket fan-out: the same key always lands in the same bucket.
// The cache layout uses the first 2 hex chars of SHA-256(key);
// this test ensures that contract holds (a future refactor that
// switches to a different hash or fan-out breaks the on-disk
// compatibility).
func TestLocalCacheBackend_DeterministicBucketing(t *testing.T) {
	tmp := t.TempDir()
	parent := newFakeBackend()
	cache, err := storage.NewLocalCacheBackend(parent, filepath.Join(tmp, "cache"), 0)
	if err != nil {
		t.Fatalf("NewLocalCacheBackend: %v", err)
	}
	ctx := context.Background()
	if err := cache.Put(ctx, "snap/abc", strings.NewReader("data")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	path := hashCachePath(t, filepath.Join(tmp, "cache"), "snap/abc")
	// Bucket must be exactly 2 hex chars.
	bucket := filepath.Base(filepath.Dir(path))
	if len(bucket) != 2 {
		t.Errorf("bucket = %q (len %d), want 2-char hex", bucket, len(bucket))
	}
	for _, c := range bucket {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			t.Errorf("bucket %q contains non-hex char %q", bucket, c)
		}
	}
}

// readAll pulls a blob through the cache, returning the contents
// as a string. Surfaces failures as test errors via t.Fatalf.
func readAll(ctx context.Context, b storage.StorageBackend, key string) (string, error) {
	rc, err := b.Get(ctx, key)
	if err != nil {
		return "", err
	}
	defer func() { _ = rc.Close() }()
	data, err := io.ReadAll(rc)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// hashCachePath recomputes the on-disk path the cache used for
// `key`. Mirrors cacheFileFor but as a test helper so the
// eviction tests can Chtimes the file directly. The hash is
// SHA-256(key), hex-encoded; the bucket is the first 2 chars.
func hashCachePath(t *testing.T, root, key string) string {
	t.Helper()
	sum := sha256.Sum256([]byte(key))
	hexStr := hex.EncodeToString(sum[:])
	return filepath.Join(root, hexStr[:2], hexStr[2:])
}

func equalSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
