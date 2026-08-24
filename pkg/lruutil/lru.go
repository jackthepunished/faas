// Package lruutil holds the small in-process LRU primitive shared
// by pkg/edgevalidate (compiled-schema cache) and
// pkg/openapidiff (auto-gen spec cache, ADR-126). Both caches
// previously re-implemented the same container/list + map-index
// MRU/evict recipe; this package factors the recipe into one
// place so a regression in eviction ordering can't drift between
// the two callers.
//
// Synchronous, single-mutex, panic-safe: callers that need TTL
// or prefix-key invalidation layer them on top.
package lruutil

import (
	"container/list"
	"sync"
)

// LRU is a key-value LRU cache with a hard cap. K and V are the
// caller-defined key/value types — the cache itself does not
// introspect them. Get moves the hit entry to the MRU end; Put
// inserts or overwrites; cap is enforced on every Put via tail
// eviction. All methods are safe for concurrent use.
type LRU[K comparable, V any] struct {
	mu    sync.Mutex
	cap   int
	index map[K]*list.Element
	lru   *list.List
}

// New returns an empty LRU capped at maxEntries. maxEntries <= 0
// is treated as 1 (a degenerate "single entry" cache).
func New[K comparable, V any](maxEntries int) *LRU[K, V] {
	if maxEntries <= 0 {
		maxEntries = 1
	}
	return &LRU[K, V]{
		cap:   maxEntries,
		index: make(map[K]*list.Element, maxEntries),
		lru:   list.New(),
	}
}

// entry is the list-element payload. Holds the key so cap-evict
// can drop the map entry without a separate reverse-lookup.
type entry[K comparable, V any] struct {
	key K
	val V
}

// Get returns the value for key and true on hit; (zero, false)
// on miss. A hit moves the entry to the MRU end.
func (c *LRU[K, V]) Get(key K) (V, bool) {
	var zero V
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.index[key]
	if !ok {
		return zero, false
	}
	c.lru.MoveToFront(el)
	return el.Value.(*entry[K, V]).val, true
}

// Put inserts (or overwrites) value at key. On overwrite the
// entry is bumped to the MRU end. Cap enforcement evicts from
// the tail (LRU end) when the list exceeds the cap, so the
// freshly-Put entry survives.
func (c *LRU[K, V]) Put(key K, value V) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.index[key]; ok {
		el.Value.(*entry[K, V]).val = value
		c.lru.MoveToFront(el)
		return
	}
	el := c.lru.PushFront(&entry[K, V]{key: key, val: value})
	c.index[key] = el
	for c.lru.Len() > c.cap {
		victim := c.lru.Back()
		if victim == nil {
			break
		}
		c.lru.Remove(victim)
		delete(c.index, victim.Value.(*entry[K, V]).key)
	}
}

// Delete removes the entry for key. No-op when the key is not
// present. Used by TTL-aware callers (pkg/openapidiff) to evict
// an expired entry on read without resetting the whole cache.
func (c *LRU[K, V]) Delete(key K) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.index[key]
	if !ok {
		return
	}
	c.lru.Remove(el)
	delete(c.index, key)
}

// Keys returns a snapshot of all keys currently in the cache,
// in MRU-first order. The slice is owned by the caller; safe
// to mutate. Used by prefix-invalidation callers
// (pkg/openapidiff.InvalidateByApp) to walk the cache without
// taking two locks at once. The snapshot is best-effort — a
// concurrent Put/Delete between the snapshot and the caller's
// processing is not synchronised.
func (c *LRU[K, V]) Keys() []K {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]K, 0, c.lru.Len())
	for el := c.lru.Front(); el != nil; el = el.Next() {
		out = append(out, el.Value.(*entry[K, V]).key)
	}
	return out
}

// Len returns the current cache size. Useful for tests and for
// the cache-entries Prometheus metric.
func (c *LRU[K, V]) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lru.Len()
}

// Reset empties the cache. Used by Wholesale-invalidation
// listeners (the cmd-side and apid-side each subscribe to
// db.NotifyEdgeRuleChanged / NotifyAppOpenAPIDocChanged).
func (c *LRU[K, V]) Reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lru.Init()
	c.index = make(map[K]*list.Element, c.cap)
}
