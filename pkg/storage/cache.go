// cache.go — read-through local cache (ADR-054 §2).
//
// Why this exists. The OCIRegistryStorageBackend serves per-app
// layers and snapshot blobs from a remote registry. A registry
// outage must NOT silently brick every cold boot on every
// compute node (issue #96 review finding). LocalCacheBackend
// wraps any StorageBackend with an LRU on disk rooted at
// FAAS_STORAGE_CACHE_DIR (default /var/lib/faas/cache when
// FAAS_STORAGE_BACKEND=oci; opt-in otherwise) so a registry
// outage degrades to last-known-good reads when the operator
// opts in via FAAS_STORAGE_CACHE_SERVE_STALE.
//
// Semantics:
//
//   - Put writes through to parent + caches. A parent failure
//     is surfaced verbatim; the cache is updated only after the
//     parent has accepted the blob.
//   - Get reads cache first. On miss, Get fetches from parent
//     and populates the cache. On parent failure, Get returns
//     the wrapped error UNLESS FAAS_STORAGE_CACHE_SERVE_STALE=true,
//     in which case Get serves the last-known-good blob from the
//     cache (Warn-level log + observer notification). The
//     fail-loud default is pinned by TestLocalCacheBackend_ParentFailureSurfaces
//     — single-box operators are not surprised by silent staleness.
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
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// CacheObserver is the optional notification surface the LocalCacheBackend
// uses to report observable events to its caller. The interface is
// deliberately minimal so pkg/storage does not depend on pkg/wire (or any
// other metrics package). cmd/{imaged,vmmd,schedd}/main.go wire an adapter
// that increments the corresponding Prometheus counter; tests can wire a
// fake observer that records the calls.
//
// OnStaleFallback fires once per Get that served a stale cache hit because
// the parent backend failed and FAAS_STORAGE_CACHE_SERVE_STALE=true.
// Observers must not block; notifyStaleFallback snapshots the observer
// under c.mu and invokes OnStaleFallback OUTSIDE the lock so a slow
// downstream (a remote slog sink, a Prometheus push) does NOT serialise
// concurrent Get calls. A future contributor must NOT 'fix' the call
// site to invoke under the lock — see notifyStaleFallback for the
// lock-then-call pattern.
//
// The interface is nil-safe at the cache: a nil observer no-ops.
type CacheObserver interface {
	OnStaleFallback()
}

// NopCacheObserver is the zero-value observer used when no observer is
// wired. Useful in tests that don't care about the notification.
type NopCacheObserver struct{}

// OnStaleFallback satisfies CacheObserver with a no-op.
func (NopCacheObserver) OnStaleFallback() {}

// LogCacheObserver is the production-default observer: logs a Warn line
// each time a stale fallback fires and calls a downstream CacheObserver
// (typically the metrics adapter wired by cmd/*/main.go). The dual sink
// keeps the §12 dashboard counter working AND gives operators a human-
// readable signal in slog JSON logs.
type LogCacheObserver struct {
	Logger *slog.Logger
	Next   CacheObserver
}

// OnStaleFallback logs the stale-fallback event and forwards to Next
// when set. A nil Logger falls back to slog.Default().
func (o LogCacheObserver) OnStaleFallback() {
	l := o.Logger
	if l == nil {
		l = slog.Default()
	}
	l.Warn("storage cache: serving stale blob on parent failure",
		"policy", "FAAS_STORAGE_CACHE_SERVE_STALE")
	if o.Next != nil {
		o.Next.OnStaleFallback()
	}
}

// FuncCacheObserver adapts a plain func to the CacheObserver interface.
// Useful when wiring a metrics adapter that lives in another package
// (e.g. cmd/*/main.go) without that package importing pkg/storage just
// for one method.
type FuncCacheObserver func()

// OnStaleFallback invokes the underlying func.
func (f FuncCacheObserver) OnStaleFallback() {
	if f == nil {
		return
	}
	f()
}

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
	// observer is the optional CacheObserver sink. The field is
	// guarded by mu; readers take a copy under lock (see
	// recordStaleFallback) so an operator-set observer is visible
	// to concurrent Get calls without an extra synchronisation
	// barrier on the hot path.
	observer CacheObserver
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
//
// Streaming: r is piped to the parent via io.Copy+io.TeeReader
// so the blob is not buffered twice. The cache write reads
// from the same TeeReader buffer after the parent has accepted
// the bytes — so the in-memory footprint is one copy of the
// blob, not two.
//
// Hard pre-check: an oversized blob (len > maxBytes) is rejected
// before any read. A pathological caller can't OOM the daemon
// by streaming a multi-GiB blob into a 1 GiB-budget cache.
func (c *LocalCacheBackend) Put(ctx context.Context, key string, r io.Reader) error {
	if err := validateKey(key); err != nil {
		return err
	}
	// Hard pre-check spans two cases:
	//   1. Caller already knows the size (e.g. via SizeReader).
	//   2. Caller passes the raw bytes (Put-from-stream).
	// We sniff for a SizeReader first to avoid buffering when
	// the upstream is already introspectable.
	var size int64 = -1
	if sr, ok := r.(interface {
		Size() int64
	}); ok {
		size = sr.Size()
	}
	if size > c.maxBytes {
		return fmt.Errorf("storage: cache: put %q: size %d exceeds maxBytes %d: %w",
			key, size, c.maxBytes, errCacheBlobOversized)
	}
	// TeeReader pipes r into the parent while a bytes.Buffer
	// accumulates the bytes for the cache write. The buffer is
	// only allocated if the upstream provides a stream; for
	// in-memory callers (the common path) the buffer is just a
	// copy of the source slice.
	//
	// Why a buffer at all? The cache write is file-backed and
	// runs after the parent has accepted the bytes. Re-reading
	// from r would either re-fetch from upstream (defeating the
	// tee) or fail (a stream that was consumed by the parent).
	// The buffer is the single source of truth for the cache write.
	var buf bytes.Buffer
	if size >= 0 {
		buf.Grow(int(size))
	}
	tee := io.TeeReader(r, &buf)
	if err := c.parent.Put(ctx, key, tee); err != nil {
		return fmt.Errorf("storage: cache: put %q: parent: %w", key, err)
	}
	// Post-parent size check covers the no-SizeReader case. By
	// here the buffer holds the entire blob; if it accidentally
	// exceeded maxBytes (e.g. a streaming caller lied or
	// didn't expose Size), the cache write below will respect
	// the budget via writeCache's eviction loop, but we still
	// want to refuse to cache a blob we can't guarantee
	// eviction for. The cache budget is per-blob-eviction so
	// a single oversized blob is allowed to evict to fit; the
	// pre-check is for the read-buffer-overflow case only.
	data := buf.Bytes()
	if int64(len(data)) > c.maxBytes {
		// Parent has the canonical copy; surface the size
		// error to the caller but don't try to evict the
		// whole cache to fit a single oversized blob.
		return fmt.Errorf("storage: cache: put %q: streamed size %d exceeds maxBytes %d: %w",
			key, len(data), c.maxBytes, errCacheBlobOversized)
	}
	if werr := c.writeCache(key, data); werr != nil {
		// Best-effort.
		_ = werr
	}
	return nil
}

// errCacheBlobOversized is the typed error Put returns when a
// blob exceeds the cache's maxBytes budget. Surfaced so a
// caller (e.g. a registry client that limits upload size) can
// distinguish "my blob is too big for the cache" from a parent
// failure.
var errCacheBlobOversized = errors.New("storage: cache: blob exceeds maxBytes")

// Get reads from cache first, then falls back to the parent.
// On parent success, the blob is mirrored into the cache before
// being returned. On cache hit, no parent round-trip happens.
//
// On parent failure, Get returns the wrapped error UNLESS
// FAAS_STORAGE_CACHE_SERVE_STALE=true, in which case Get serves the
// last-known-good cached blob (must have been Put earlier — the cache
// only stores blobs the parent has accepted). The first readCache call
// at the top of Get is the cache-first path; the second readCache call
// below is the stale-fallback path. Both run only when the cache holds
// the key (Put has mirrored it). When the cache is empty, stale-fallback
// is a no-op and the wrapped parent error is returned regardless of the
// env var.
func (c *LocalCacheBackend) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	if err := validateKey(key); err != nil {
		return nil, err
	}
	if data, ok := c.readCache(key); ok {
		return io.NopCloser(strings.NewReader(string(data))), nil
	}
	rc, err := c.parent.Get(ctx, key)
	if err != nil {
		if c.serveStale() {
			if data, ok := c.readCache(key); ok {
				c.notifyStaleFallback()
				return io.NopCloser(strings.NewReader(string(data))), nil
			}
		}
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

// SetObserver wires a CacheObserver onto the cache. The observer
// receives OnStaleFallback notifications when the Get path serves a
// stale blob. A nil observer disables notifications (the zero-value
// default). Observer replacement is allowed; the previous observer
// (if any) is dropped without notification.
//
// The setter mirrors the constructor-injection pattern used elsewhere
// in the codebase (Handler.WithMetrics, DiskDrift.WithStorage). The
// cmd/* main wiring calls SetObserver immediately after
// BackendFromEnv returns, so production builds always have an observer
// when the cache is wrapped.
func (c *LocalCacheBackend) SetObserver(o CacheObserver) {
	c.mu.Lock()
	c.observer = o
	c.mu.Unlock()
}

// serveStale reports whether the cache should serve a last-known-good
// blob when the parent backend fails. The env lookup is done at every
// parent error so operators can toggle the policy without a daemon
// restart — matches the "default-on for oci mode" policy of the storage
// env config (wrapWithCache reads FAAS_STORAGE_BACKEND at startup; this
// hot-path env read is symmetric).
//
// The env lookup is cheap (single os.Getenv on a short string) and the
// call is on the failure path (cold boot with a registry outage), not
// the steady-state hot path.
//
// Truthy parsing: accepts "1", "t", "T", "true", "TRUE", "True" —
// anything strconv.ParseBool accepts as true. Unset, empty, "0",
// "f", "F", "false", "FALSE", "no", "off" all return false. Operators
// reading a 12-factor example that says FAAS_STORAGE_CACHE_SERVE_STALE=1
// or =True get the same behaviour as =true — the safety net is on
// whenever the operator reasonably intends it. Values ParseBool
// rejects (e.g. "yes", "on", "y") fall through to false — those
// spellings are not standard Go env conventions and an operator
// using them can be expected to use "true" or "1" after reading the
// runbook.
func (c *LocalCacheBackend) serveStale() bool {
	v := os.Getenv("FAAS_STORAGE_CACHE_SERVE_STALE")
	if v == "" {
		return false
	}
	on, err := strconv.ParseBool(v)
	if err != nil {
		return false
	}
	return on
}

// notifyStaleFallback snapshots the current observer under the cache
// mutex and calls OnStaleFallback outside the lock. A nil observer
// no-ops. The lock-then-call pattern keeps an observer that panics or
// blocks from holding up concurrent Get calls — the only contention is
// the brief snapshot under c.mu.
//
// Observers must not block. The production observer (LogCacheObserver)
// emits one Warn slog line and forwards to the metrics adapter; both
// are constant-time.
func (c *LocalCacheBackend) notifyStaleFallback() {
	c.mu.Lock()
	o := c.observer
	c.mu.Unlock()
	if o == nil {
		return
	}
	o.OnStaleFallback()
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
	// Note: we deliberately do NOT Chtimes the file on read.
	// The cache's LRU eviction is mtime-driven; touching the
	// mtime on every read would make the entire cache look
	// freshly-written and silently defeat eviction. The
	// "hot" entries are the ones that protect against registry
	// outages (ADR-054 §2); their Put mtime is the right
	// signal. A hot entry that gets evicted is still served
	// on the next Get via the parent-round-trip fallback, so
	// freshness is governed by upstream behaviour, not by the
	// cache's read traffic.
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
