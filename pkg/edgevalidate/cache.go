// Package edgevalidate owns the per-process compiled-schema cache,
// keyed by SHA-256 of the raw schema body. The cache is consulted
// from the validate-rule applier (cmd-side) on every match: a hit
// skips the jsonschema/v6 compile; a miss triggers Compile (capped
// by MaxCompiledSchemas) and inserts at the MRU end of the LRU.
//
// The LRU primitive lives in pkg/lruutil (shared with
// pkg/openapidiff — ADR-126). The schema cache layers two things
// on top:
//
//   - TTL handling is intentionally absent: schemas are inlined
//     in edge_rules.action jsonb; no network fetch is needed at
//     any point, so there's no time-based invalidation concern.
//     Wholesale reset on db.NotifyEdgeRuleChanged is the only
//     freshness signal.
//   - The "URL" the cache key is keyed on is a 32-byte SHA-256
//     digest, not a string. The applier computes the digest on
//     the cached schema body at compile-rule time (cmd-side
//     load) and never re-hashes on the hot path.
//
// Wholesale invalidation (Reset) is wired through the
// db.NotifyEdgeRuleChanged listener — same as pkg/edgejwks.
package edgevalidate

import (
	"github.com/onebox-faas/faas/pkg/lruutil"
)

// Cache is the per-process compiled-schema cache interface.
// pkg/openapidiff's SpecCache has additional surface (TTL,
// InvalidateByApp) that doesn't apply here; we keep the
// interface narrow so the schema-cache surface stays
// reviewable.
type Cache interface {
	// Get returns the CompiledSchema for digest, or (nil, false)
	// on miss. A hit is recorded at the MRU end of the LRU.
	Get(digest [32]byte) (*CompiledSchema, bool)

	// Register inserts compiled under digest. On overflow
	// (cache len > MaxCompiledSchemas) the LRU entry is evicted
	// to make room. Calling Register with a duplicate digest
	// overwrites the existing entry and bumps it to MRU.
	Register(digest [32]byte, compiled *CompiledSchema)

	// Reset empties the cache. Called by the cmd-side on
	// db.NotifyEdgeRuleChanged to bring the cache back in line
	// with the freshly-loaded edge rules.
	Reset()

	// Len returns the current cache size. Useful for tests and
	// for the gateway_edge_rule_validate_cache_entries metric
	// (PR-B does not wire that metric; it lives in the
	// cross-cutting follow-up so PR-C can pick a stable label
	// set).
	Len() int
}

// lruCache is the only Cache impl today. It composes the
// pkg/lruutil primitive so a future fix to eviction ordering
// lands in one place. The struct is private; tests access it
// through the constructor.
type lruCache struct {
	lru *lruutil.LRU[[32]byte, *CompiledSchema]
}

// NewCache constructs an empty LRU cache with the cap
// MaxCompiledSchemas (1024). The cap is enforced lazily — Put
// may transiently hold one extra entry while it evicts.
func NewCache() Cache {
	return &lruCache{
		lru: lruutil.New[[32]byte, *CompiledSchema](MaxCompiledSchemas),
	}
}

func (c *lruCache) Get(digest [32]byte) (*CompiledSchema, bool) {
	return c.lru.Get(digest)
}

func (c *lruCache) Register(digest [32]byte, compiled *CompiledSchema) {
	c.lru.Put(digest, compiled)
}

func (c *lruCache) Reset() {
	c.lru.Reset()
}

func (c *lruCache) Len() int {
	return c.lru.Len()
}
