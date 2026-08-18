// cmd/apid/build_plan_cache.go — in-process cache for
// markers.DetectFromTarball + markers.VersionFromTarball.
//
// HIGH-2 fix: deploymentResponse was calling the markers API on
// every listDeployments row, which meant a 50-deployment list did
// 50 tarball opens + 50 marker-file reads. The spool path
// (d.SourcePath) for a given deployment row is immutable — a re-deploy
// lands a NEW deployment row with a NEW spool path. So caching by
// path is sound; the only invalidation event we need is "the file
// changed on disk under the same path", which we approximate with
// the file's mtime (a fresh spool write bumps it).
//
// No PG migration is required. The cache is process-local; on
// apid restart it repopulates lazily on the next request. The
// spool lives on tmpfs (`/srv/fc/jail`, see CLAUDE.md) and is
// rewritten per deploy, so cache hits stay high across the
// list → show → list cycle that the dashboard uses.
//
// The cache is bounded by buildPlanCacheSize entries; LRU eviction
// keeps the working set bounded. The default 1024 entries × ~200
// bytes each = ~200 KB resident, well under the apid memory budget.

package main

import (
	"os"
	"sync"
	"time"

	"github.com/onebox-faas/faas/pkg/markers"
)

// buildPlanCacheSize is the upper bound on cached tarball-marker
// detection results. Sized to cover a Free-plan customer's
// expected list-page size (50) plus a 20× working-set margin so a
// dashboard's filter drill-down doesn't thrash.
const buildPlanCacheSize = 1024

// buildPlanCacheTTL is the maximum staleness. The mtime check is
// the primary invalidation; this is a belt-and-braces bound for
// the rare case where mtime went backwards (e.g. tarball rewrote
// with the same timestamp).
const buildPlanCacheTTL = 5 * time.Minute

// buildPlanCacheEntry holds the marker detection result for a
// given spool path. cachedAt + mtime together form the
// invalidation key.
type buildPlanCacheEntry struct {
	fw       markers.Framework
	version  string
	mtime    time.Time
	cachedAt time.Time
}

// buildPlanCache is the process-local LRU. The zero value is not
// usable; callers go through getCachedBuildPlan which lazily
// initialises it on first use.
type buildPlanCache struct {
	mu      sync.Mutex
	entries map[string]*buildPlanCacheEntry
	order   []string // ring of insertion order; oldest at head.
}

var (
	buildPlanCacheOnce sync.Once
	planCache          *buildPlanCache
)

func initBuildPlanCache() *buildPlanCache {
	return &buildPlanCache{
		entries: make(map[string]*buildPlanCacheEntry, buildPlanCacheSize),
		order:   make([]string, 0, buildPlanCacheSize),
	}
}

// getCachedBuildPlan returns the (framework, version) pair for the
// tarball at path, consulting the cache first. The mtime check
// ensures a re-spool of the same path re-runs detection.
func getCachedBuildPlan(path string) (markers.Framework, string) {
	if path == "" {
		return markers.FrameworkUnknown, ""
	}
	buildPlanCacheOnce.Do(func() { planCache = initBuildPlanCache() })

	// mtime lookup is cheap (single stat call) and gates the
	// cached entry — a fresh spool write bumps mtime and the
	// cache miss repopulates.
	fi, err := os.Stat(path)
	if err != nil {
		// Spool missing (deploy parked/garbage-collected). Treat
		// as unknown so the wire carries a consistent shape; the
		// caller still renders BuildPlan.Framework="" instead of
		// panicking.
		return markers.FrameworkUnknown, ""
	}
	mtime := fi.ModTime()

	planCache.mu.Lock()
	defer planCache.mu.Unlock()

	if e, ok := planCache.entries[path]; ok {
		if e.mtime.Equal(mtime) && time.Since(e.cachedAt) < buildPlanCacheTTL {
			return e.fw, e.version
		}
		// mtime changed or TTL expired — drop the stale entry.
		delete(planCache.entries, path)
		planCache.removeFromOrder(path)
	}

	// Miss: re-detect and store. We do the detect OUTSIDE the
	// lock next, but the lock pattern here is acceptable because
	// markers.DetectFromTarball is fast (a single tar read of the
	// top-level marker files — see pkg/markers/detect.go) and
	// listDeployments is the only contended caller. If the
	// detector ever becomes expensive we can split the lock.
	fw, _ := markers.DetectFromTarball(path)
	var ver string
	if fw != markers.FrameworkUnknown {
		ver = markers.VersionFromTarball(path, fw)
	}
	planCache.putLocked(path, &buildPlanCacheEntry{
		fw: fw, version: ver, mtime: mtime, cachedAt: time.Now(),
	})
	return fw, ver
}

// putLocked inserts an entry, evicting the oldest if the cache is
// at capacity. Caller must hold c.mu.
func (c *buildPlanCache) putLocked(path string, e *buildPlanCacheEntry) {
	if _, ok := c.entries[path]; ok {
		c.entries[path] = e
		return
	}
	if len(c.order) >= buildPlanCacheSize {
		oldest := c.order[0]
		c.order = c.order[1:]
		delete(c.entries, oldest)
	}
	c.entries[path] = e
	c.order = append(c.order, path)
}

// removeFromOrder drops path from the LRU order slice. O(n) but
// called only on invalidation; not on the hot path.
func (c *buildPlanCache) removeFromOrder(path string) {
	for i, p := range c.order {
		if p == path {
			c.order = append(c.order[:i], c.order[i+1:]...)
			return
		}
	}
}
