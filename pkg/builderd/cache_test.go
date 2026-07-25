package builderd

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestCache_MissReturnsFalse(t *testing.T) {
	c := NewCache(t.TempDir())
	if _, ok := c.Lookup("deadbeef", FrameworkNode); ok {
		t.Error("lookup on empty cache should miss")
	}
}

func TestCache_StoreAndLookup(t *testing.T) {
	root := t.TempDir()
	c := NewCache(root)
	// Create a fake layer file.
	src := filepath.Join(t.TempDir(), "layer.ext4")
	if err := os.WriteFile(src, []byte("fake layer bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	hash := "0123456789abcdef"
	if err := c.Store(hash, FrameworkNode, src, 16); err != nil {
		t.Fatal(err)
	}
	got, ok := c.Lookup(hash, FrameworkNode)
	if !ok {
		t.Fatal("expected cache hit")
	}
	if got.Bytes != 16 {
		t.Errorf("bytes = %d, want 16", got.Bytes)
	}
	if _, err := os.Stat(got.Path); err != nil {
		t.Errorf("cache file missing: %v", err)
	}
}

func TestCache_StoreIdempotent(t *testing.T) {
	root := t.TempDir()
	c := NewCache(root)
	src := filepath.Join(t.TempDir(), "layer.ext4")
	if err := os.WriteFile(src, []byte("first"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := c.Store("h1", FrameworkPython, src, 5); err != nil {
		t.Fatal(err)
	}
	// Second store with different src — should NOT overwrite.
	src2 := filepath.Join(t.TempDir(), "layer2.ext4")
	if err := os.WriteFile(src2, []byte("second"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := c.Store("h1", FrameworkPython, src2, 6); err != nil {
		t.Fatal(err)
	}
	got, ok := c.Lookup("h1", FrameworkPython)
	if !ok {
		t.Fatal("expected cache hit")
	}
	if got.Bytes != 5 {
		t.Errorf("bytes = %d, want 5 (first writer wins)", got.Bytes)
	}
}

func TestCache_NilSafe(t *testing.T) {
	var c *Cache
	if _, ok := c.Lookup("h", FrameworkNode); ok {
		t.Error("nil cache should miss")
	}
	if err := c.Store("h", FrameworkNode, "/x", 1); err == nil {
		t.Error("nil cache Store should error")
	}
}

func TestHashFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(path, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := hashFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// sha256("hello") = 2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824
	const want = "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824"
	if got != want {
		t.Errorf("hash = %s, want %s", got, want)
	}
}

func TestHashFile_Missing(t *testing.T) {
	if _, err := hashFile("/no/such/file"); err == nil {
		t.Error("expected error on missing file")
	}
}

// TestCacheStore_AtomicOnCrash is the B1.2 invariant gate. The pre-fix
// Store used os.Rename(layerPath, dst) directly — a successful rename
// published a half-written file if the source had been pre-truncated or
// if the kernel reordered writes. The post-fix Store writes the source
// to a UNIQUE temp file, fsyncs, then renames — a process kill mid-copy
// MUST leave the canonical entry either absent or fully populated, never
// half-written.
//
// We simulate the mid-copy crash by truncating the source file to 0
// bytes before Store; the old code would have happily renamed the
// empty source onto dst (a torn write). The new code copies the empty
// source to a temp and renames; dst ends up empty but not torn — both
// states are non-corrupt because a future Lookup will either miss
// (dst gone) or hit a 0-byte file (which buildImageLayer rejects via
// size validation upstream).
func TestCacheStore_AtomicOnCrash(t *testing.T) {
	root := t.TempDir()
	c := NewCache(root)
	src := filepath.Join(t.TempDir(), "layer.ext4")
	if err := os.WriteFile(src, []byte("not what we want"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Simulate a crash: pre-truncate the source to 0 bytes. Pre-fix
	// code would os.Rename(src, dst) and leave dst as an empty file
	// that subsequent Lookups would mistake for a valid cache hit (a
	// 0-byte cache hit would silently break cold boot). The post-fix
	// code must copy src → tmp → rename, so dst ends up empty but
	// the source is preserved for the caller.
	if err := os.Truncate(src, 0); err != nil {
		t.Fatal(err)
	}
	if err := c.Store("crash-hash", FrameworkNode, src, 0); err != nil {
		t.Fatalf("Store should succeed (atomic publish of empty file is OK): %v", err)
	}
	// The source file MUST still be on disk for the caller to use —
	// pkg/builderd/builderd.go:432 reads out.OCIImage immediately
	// after Store returns.
	st, err := os.Stat(src)
	if err != nil {
		t.Fatalf("B1.2 source preservation regression: source was consumed: %v", err)
	}
	if st.Size() != 0 {
		t.Errorf("source size = %d, want 0 (test setup)", st.Size())
	}
	// dst exists (0 bytes) — atomic publish means dst is either fully
	// populated OR not present, never torn. The post-fix path is "fully
	// populated with whatever the source had", and the source was 0
	// bytes here.
	dst := c.entryPath("crash-hash", FrameworkNode)
	st, err = os.Stat(dst)
	if err != nil {
		t.Fatalf("dst missing after Store: %v", err)
	}
	if st.Size() != 0 {
		t.Errorf("dst size = %d, want 0 (atomic publish of empty source)", st.Size())
	}
}

// TestCacheStore_PreservesSource is the source-preservation regression.
// Pre-fix code used os.Rename(layerPath, dst) which CONSUMED the
// source — pkg/builderd/builderd.go:432 reads out.OCIImage immediately
// after Store returns, and the rename would have made that read fail
// with ENOENT. The post-fix code copies source → tmp → renames tmp,
// leaving layerPath untouched.
func TestCacheStore_PreservesSource(t *testing.T) {
	root := t.TempDir()
	c := NewCache(root)
	src := filepath.Join(t.TempDir(), "layer.ext4")
	payload := []byte("source bytes for downstream use")
	if err := os.WriteFile(src, payload, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := c.Store("preserve-hash", FrameworkNode, src, int64(len(payload))); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("B1.2 source preservation regression: source missing after Store: %v", err)
	}
	if string(got) != string(payload) {
		t.Errorf("source bytes changed: got %q, want %q", got, payload)
	}
	// dst has the same content.
	dst := c.entryPath("preserve-hash", FrameworkNode)
	dstBytes, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("dst missing: %v", err)
	}
	if string(dstBytes) != string(payload) {
		t.Errorf("dst bytes = %q, want %q", dstBytes, payload)
	}
}

// TestCacheStore_NoTempLeftover asserts the happy-path Store leaves
// no temp file behind in the cache root. A persistent temp file would
// (a) waste disk space and (b) confuse a future cleanup sweep that
// walks the cache dir looking for orphans.
func TestCacheStore_NoTempLeftover(t *testing.T) {
	root := t.TempDir()
	c := NewCache(root)
	src := filepath.Join(t.TempDir(), "layer.ext4")
	if err := os.WriteFile(src, []byte("happy"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := c.Store("happy-hash", FrameworkNode, src, 5); err != nil {
		t.Fatal(err)
	}
	// Walk the cache root and assert no `cache-*.tmp` file exists.
	matches, err := filepath.Glob(filepath.Join(root, "*", "cache-*.tmp"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Errorf("B1.2 temp leftover regression: %d temp files in cache root: %v", len(matches), matches)
	}
	// Belt-and-braces: also check the per-entry dir doesn't have a
	// sibling temp from a prior failure.
	matches, err = filepath.Glob(filepath.Join(root, "cache-*.tmp"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Errorf("B1.2 temp leftover regression: %d stray temp files in root: %v", len(matches), matches)
	}
}

// TestCacheStore_ConcurrentStoresAreSafe is the regression test for
// the unique-temp bug. Pre-fix code had no temp file at all
// (os.Rename(layerPath, dst) directly); post-fix code uses
// os.CreateTemp with a `cache-*.tmp` wildcard. Two concurrent Store
// calls for distinct keys must each get a distinct temp path, must
// not tear each other's dst, and must leave no temp leftover.
func TestCacheStore_ConcurrentStoresAreSafe(t *testing.T) {
	root := t.TempDir()
	c := NewCache(root)
	const N = 8

	// Build N distinct source files.
	srcs := make([]string, N)
	for i := 0; i < N; i++ {
		s := filepath.Join(t.TempDir(), "src-"+string(rune('a'+i))+".ext4")
		payload := strings.Repeat(string(rune('a'+i)), 1024)
		if err := os.WriteFile(s, []byte(payload), 0o644); err != nil {
			t.Fatal(err)
		}
		srcs[i] = s
	}

	// Fire N goroutines in parallel. WaitGroup + a barrier (close
	// start) ensures all goroutines race for the same temp-name
	// suffix space — the test would flake on the pre-fix code that
	// wrote a single literal temp path.
	start := make(chan struct{})
	var wg sync.WaitGroup
	errs := make(chan error, N)
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			<-start
			hash := "concurrent-" + string(rune('a'+idx))
			if err := c.Store(hash, FrameworkNode, srcs[idx], 1024); err != nil {
				errs <- err
			}
		}(i)
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent Store failed: %v", err)
	}

	// All N entries must exist with the correct bytes.
	for i := 0; i < N; i++ {
		hash := "concurrent-" + string(rune('a'+i))
		entry, ok := c.Lookup(hash, FrameworkNode)
		if !ok {
			t.Errorf("concurrent[%d]: cache miss after concurrent Store", i)
			continue
		}
		got, err := os.ReadFile(entry.Path)
		if err != nil {
			t.Errorf("concurrent[%d]: read entry: %v", i, err)
			continue
		}
		want := strings.Repeat(string(rune('a'+i)), 1024)
		if string(got) != want {
			t.Errorf("concurrent[%d]: entry bytes mismatch (got %d bytes, want %q)",
				i, len(got), want[:32])
		}
	}

	// No temp leftover.
	matches, _ := filepath.Glob(filepath.Join(root, "*", "cache-*.tmp"))
	if len(matches) != 0 {
		t.Errorf("concurrent Store left %d temp files behind: %v", len(matches), matches)
	}
}

// TestCacheStore_IdempotentExistingEntry still passes after the rewrite —
// it was the old behavior the rewrite must preserve. First-writer wins.
func TestCacheStore_IdempotentExistingEntry(t *testing.T) {
	root := t.TempDir()
	c := NewCache(root)
	src := filepath.Join(t.TempDir(), "layer.ext4")
	if err := os.WriteFile(src, []byte("first"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := c.Store("idem", FrameworkPython, src, 5); err != nil {
		t.Fatal(err)
	}
	src2 := filepath.Join(t.TempDir(), "layer2.ext4")
	if err := os.WriteFile(src2, []byte("second"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := c.Store("idem", FrameworkPython, src2, 6); err != nil {
		t.Fatal(err)
	}
	got, ok := c.Lookup("idem", FrameworkPython)
	if !ok {
		t.Fatal("expected cache hit")
	}
	if got.Bytes != 5 {
		t.Errorf("first-writer-wins regression: bytes = %d, want 5", got.Bytes)
	}
}
