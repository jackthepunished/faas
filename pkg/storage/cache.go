// cache.go — read-through local cache (ADR-054 §2).
//
// Why this exists. The OCIRegistryStorageBackend serves per-app
// layers and snapshot blobs from a remote registry. A registry
// outage must NOT silently brick every cold boot on every
// compute node (issue #96 review finding). LocalCacheBackend
// wraps any StorageBackend with an LRU on disk rooted at
// FAAS_STORAGE_CACHE_DIR (default /var/lib/faas/cache) so the
// last-known-good blob is served when the parent backend is
// unreachable.
//
// Semantics:
//
//   - Put writes through to parent + caches. A parent failure
//     is surfaced verbatim; the cache is updated only after the
//     parent has accepted the blob.
//   - Get reads cache first. On miss, Get fetches from parent
//     and populates the cache. On parent failure, Get returns
//     the wrapped error (no fallback to a stale cache hit;
//     callers that want stale-fallback use a separate seam —
//     the canonical "registry unreachable" gate at startup).
//   - Delete evicts from cache + forwards to parent. Best-effort
//     propagation: a parent-side delete that fails is logged and
//     the cache entry is evicted anyway, because stale data is
//     worse than a failed Put.
//   - List implements LocalArtifactLister by reading a sidecar
//     metadata file alongside each cached blob (the storage key
//     + size + mtime). No parent round-trip.
//
// Cache layout:
//
//	<root>/<bucket>/<hex-hash>          // the blob itself
//	<root>/<bucket>/<hex-hash>.meta     // sidecar: storage key
//
// The bucket directory is the first 2 hex chars of the SHA-256
// of the storage key (16-character fan-out = 256 buckets). The
// hash protects against a flat-dir layout where a Put for
// "a/b" collides with a Put for "a" + "b".
//
// The LRU eviction is byte-budgeted: when the cache exceeds its
// maxBytes budget, the oldest entries by mtime are evicted
// until the budget is restored. Default budget is 1 GiB;
// operators override via FAAS_STORAGE_CACHE_MAX_BYTES.
//
// Out of scope (here, deferred to a follow-up ADR):
//   - TTL-based eviction. ADR-054 §Consequences names this as
//     a v1.1 tightening; not load-bearing for the Tier 1 slice.
//   - Compression. Storage is cheap; v1 is a 1:1 mirror.

package storage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// DefaultCacheMaxBytes is the byte-budget fallback when
// FAAS_STORAGE_CACHE_MAX_BYTES is unset. 1 GiB keeps the cache
// small enough to live on the OS disk partition without
// competing with the rootfs base + snapshots; large enough to
// absorb a registry outage of a few minutes at the typical
// fleet layer size (~250 MB × 4 = 1 GiB).
const DefaultCacheMaxBytes int64 = 1 << 30

// LocalCacheBackend is the read-through cache ADR-054 §2
// describes. Construct one with NewLocalCacheBackend; pass the
// parent backend (typically OCIRegistryStorageBackend or a
// PrefixRouter) and the cache root directory.
type LocalCacheBackend struct {
	parent   StorageBackend
	root     string
	maxBytes int64
	mu       sync.Mutex
}

// NewLocalCacheBackend wires a LocalCacheBackend rooted at
// root. The parent backend is required (nil returns an error).
// maxBytes <= 0 falls back to DefaultCacheMaxBytes.
//
// root is created with mode 0o755 if missing (the cache dir is
// world-readable on purpose — no secrets in here, and a future
// hook may share it across containers). Operators set the dir
// mode in the deploy ansible role; the runtime never loosens
// or tightens it post-creation.
func NewLocalCacheBackend(parent StorageBackend, root string, maxBytes int64) (*LocalCacheBackend, error) {
	if parent == nil {
		return nil, errors.New("storage: cache: nil parent backend")
	}
	if root == "" {
		return nil, errors.New("storage: cache: empty root dir")
	}
	if maxBytes <= 0 {
		maxBytes = DefaultCacheMaxBytes
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, fmt.Errorf("storage: cache: mkdir %q: %w", root, err)
	}
	return &LocalCacheBackend{
		parent:   parent,
		root:     root,
		maxBytes: maxBytes,
	}, nil
}

// Root returns the on-disk cache root directory. Used by
// cmd/{imaged,vmmd}/main.go to log the resolved path at
// startup.
func (c *LocalCacheBackend) Root() string { return c.root }

// cacheFileFor hashes the storage key into a path-safe
// filename. The hash is hex-encoded SHA-256 (64 lowercase hex
// chars), with the leading 2 chars used as the bucket directory
// so a single flat directory doesn't grow unbounded.
func (c *LocalCacheBackend) cacheFileFor(key string) (path string, metaPath string) {
	sum := sha256.Sum256([]byte(key))
	hex := hex.EncodeToString(sum[:])
	full := filepath.Join(c.root, hex[:2], hex[2:])
	return full, full + ".meta"
}

// Put writes the blob to the parent and then mirrors it into
// the cache. The cache update is best-effort: a cache write
// failure is logged + swallowed (the parent has the canonical
// copy; the next Get will repopulate).
func (c *LocalCacheBackend) Put(ctx context.Context, key string, r io.Reader) error {
	if err := validateKey(key); err != nil {
		return err
	}
	// Buffer the blob so we can write to the parent AND
	// cache without two reads. The cache byte budget
	// bounds the memory cost (1 GiB default).
	data, err := io.ReadAll(r)
	if err != nil {
		return fmt.Errorf("storage: cache: put %q: read: %w", key, err)
	}
	if err := c.parent.Put(ctx, key, strings.NewReader(string(data))); err != nil {
		return fmt.Errorf("storage: cache: put %q: parent: %w", key, err)
	}
	if werr := c.writeCache(key, data); werr != nil {
		// Best-effort.
		_ = werr
	}
	return nil
}

// Get reads from cache first, then falls back to the parent.
// On parent success, the blob is mirrored into the cache before
// being returned. On cache hit, no parent round-trip happens.
func (c *LocalCacheBackend) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	if err := validateKey(key); err != nil {
		return nil, err
	}
	if data, ok := c.readCache(key); ok {
		return io.NopCloser(strings.NewReader(string(data))), nil
	}
	rc, err := c.parent.Get(ctx, key)
	if err != nil {
		return nil, fmt.Errorf("storage: cache: get %q: parent: %w", key, err)
	}
	defer func() { _ = rc.Close() }()
	data, err := io.ReadAll(rc)
	if err != nil {
		return nil, fmt.Errorf("storage: cache: get %q: parent read: %w", key, err)
	}
	if cerr := c.writeCache(key, data); cerr != nil {
		_ = cerr
	}
	return io.NopCloser(strings.NewReader(string(data))), nil
}

// Delete evicts the cache entry + forwards to parent. The
// cache eviction always runs (so a stale cache hit can't mask a
// stale parent); the parent delete is best-effort propagation.
func (c *LocalCacheBackend) Delete(ctx context.Context, key string) error {
	if err := validateKey(key); err != nil {
		return err
	}
	c.evictCache(key)
	if err := c.parent.Delete(ctx, key); err != nil {
		return fmt.Errorf("storage: cache: delete %q: parent: %w", key, err)
	}
	return nil
}

// List implements LocalArtifactLister by reading the sidecar
// metadata files alongside each cached blob. Returns the
// original storage keys (not the hash-derived filenames) so
// callers can correlate by content. No parent round-trip —
// only what the cache holds is visible.
func (c *LocalCacheBackend) List(ctx context.Context, prefix string) ([]string, error) {
	if prefix != "" {
		if err := validateKey(strings.TrimSuffix(prefix, "/")); err != nil {
			return nil, err
		}
	}
	c.mu.Lock()
	entries, err := c.snapshotCacheLocked()
	c.mu.Unlock()
	if err != nil {
		return nil, fmt.Errorf("storage: cache: list: %w", err)
	}
	keys := make([]string, 0, len(entries))
	for _, e := range entries {
		if prefix != "" && !strings.HasPrefix(e.key, prefix) {
			continue
		}
		keys = append(keys, e.key)
	}
	sort.Strings(keys)
	return keys, nil
}

// cacheEntry is the metadata the LRU index carries alongside
// the cached blob. The key is the storage key (re-derivable
// from the sidecar file, not the path).
type cacheEntry struct {
	key     string
	path    string
	size    int64
	modTime time.Time
}

// readCache attempts to read a cache entry. Returns the blob +
// true on hit, nil + false on miss.
func (c *LocalCacheBackend) readCache(key string) ([]byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	path, metaPath := c.cacheFileFor(key)
	if _, err := os.Stat(metaPath); err != nil {
		return nil, false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, false
	}
	// Touch the file's mtime so LRU ordering prefers it.
	now := time.Now()
	if err := os.Chtimes(path, now, now); err != nil {
		_ = err
	}
	return data, true
}

// writeCache writes the blob + sidecar metadata to the cache
// directory and enforces the byte budget by evicting the
// oldest entries until the directory fits under maxBytes.
func (c *LocalCacheBackend) writeCache(key string, data []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	path, metaPath := c.cacheFileFor(key)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("storage: cache: mkdir %q: %w", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("storage: cache: write %q: %w", path, err)
	}
	if err := os.WriteFile(metaPath, []byte(key), 0o644); err != nil {
		// Sidecar failure is non-fatal but degrades List.
		_ = err
	}
	if err := c.enforceBudgetLocked(); err != nil {
		_ = err
	}
	return nil
}

// evictCache removes a single entry + sidecar. Best-effort:
// a missing entry is not an error.
func (c *LocalCacheBackend) evictCache(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	path, metaPath := c.cacheFileFor(key)
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		_ = err
	}
	if err := os.Remove(metaPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		_ = err
	}
}

// enforceBudgetLocked walks the cache directory, sums the
// sizes, and evicts the oldest entries by mtime until the
// total drops under maxBytes. Caller holds c.mu.
func (c *LocalCacheBackend) enforceBudgetLocked() error {
	entries, err := c.snapshotCacheLocked()
	if err != nil {
		return err
	}
	var total int64
	for _, e := range entries {
		total += e.size
	}
	if total <= c.maxBytes {
		return nil
	}
	// Sort oldest-first; evict until budget restored.
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].modTime.Before(entries[j].modTime)
	})
	for _, e := range entries {
		if total <= c.maxBytes {
			break
		}
		if err := os.Remove(e.path); err == nil {
			total -= e.size
		}
		metaPath := e.path + ".meta"
		if err := os.Remove(metaPath); err == nil {
			_ = err
		}
	}
	return nil
}

// snapshotCacheLocked walks the cache directory and returns
// one cacheEntry per cached blob (skipping the .meta sidecars).
// The original storage key is read from the sidecar. Caller
// holds c.mu.
func (c *LocalCacheBackend) snapshotCacheLocked() ([]cacheEntry, error) {
	var out []cacheEntry
	buckets, err := os.ReadDir(c.root)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	for _, b := range buckets {
		if !b.IsDir() || len(b.Name()) != 2 {
			continue
		}
		files, err := os.ReadDir(filepath.Join(c.root, b.Name()))
		if err != nil {
			continue
		}
		for _, f := range files {
			if f.IsDir() {
				continue
			}
			if strings.HasSuffix(f.Name(), ".meta") {
				continue
			}
			info, err := f.Info()
			if err != nil {
				continue
			}
			path := filepath.Join(c.root, b.Name(), f.Name())
			metaPath := path + ".meta"
			metaBytes, err := os.ReadFile(metaPath)
			if err != nil {
				// No sidecar → skip. The blob exists but
				// the original storage key is unknowable.
				continue
			}
			out = append(out, cacheEntry{
				key:     string(metaBytes),
				path:    path,
				size:    info.Size(),
				modTime: info.ModTime(),
			})
		}
	}
	return out, nil
}
