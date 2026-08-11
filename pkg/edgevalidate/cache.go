package edgevalidate

import (
	"container/list"
	"sync"
)

// Cache is the per-process compiled-schema cache, keyed by SHA-256
// of the raw schema body. The cache is consulted from the
// validate-rule applier (cmd-side) on every match: a hit skips the
// jsonschema/v6 compile; a miss triggers Compile (capped by
// MaxCompiledSchemas) and inserts at the MRU end of the LRU.
//
// Mirrors pkg/edgejwks.Cache shape (Get / Register / Reset) so the
// gateway-side and cmd-side code stays symmetric. Differences from
// pkg/edgejwks:
//
//   - No fetch+TTL (schemas are inlined in edge_rules.action jsonb;
//     no network fetch is needed at any point).
//   - The "URL" the cache key is keyed on is a 32-byte SHA-256
//     digest, not a string. The applier computes the digest on
//     the cached schema body at compile-rule time (cmd-side
//     load) and never re-hashes on the hot path.
//
// Wholesale invalidation (Reset) is wired through the
// db.NotifyEdgeRuleChanged listener — same as pkg/edgejwks.
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

// lruCache is the only Cache impl today. The struct is private;
// tests access it through the constructor.
type lruCache struct {
	mu sync.Mutex
	// list is doubly-linked; front = MRU, back = LRU.
	list *list.List
	// index maps digest → *list.Element{ key, value=*CompiledSchema }.
	index map[[32]byte]*list.Element
}

type cacheEntry struct {
	key   [32]byte
	value *CompiledSchema
}

// NewCache constructs an empty LRU cache with the cap
// MaxCompiledSchemas (1024). The cap is enforced lazily — Register
// may transiently hold one extra entry while it evicts.
func NewCache() Cache {
	return &lruCache{
		list:  list.New(),
		index: make(map[[32]byte]*list.Element, MaxCompiledSchemas),
	}
}

func (c *lruCache) Get(digest [32]byte) (*CompiledSchema, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.index[digest]
	if !ok {
		return nil, false
	}
	c.list.MoveToFront(el)
	return el.Value.(*cacheEntry).value, true
}

func (c *lruCache) Register(digest [32]byte, compiled *CompiledSchema) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.index[digest]; ok {
		// Duplicate: overwrite + bump to MRU.
		el.Value.(*cacheEntry).value = compiled
		c.list.MoveToFront(el)
		return
	}
	el := c.list.PushFront(&cacheEntry{key: digest, value: compiled})
	c.index[digest] = el
	// Cap enforcement. We evict the LRU end, not the new entry,
	// so the freshly-Registered item survives.
	for c.list.Len() > MaxCompiledSchemas {
		victim := c.list.Back()
		if victim == nil {
			break
		}
		c.list.Remove(victim)
		delete(c.index, victim.Value.(*cacheEntry).key)
	}
}

func (c *lruCache) Reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.list.Init()
	c.index = make(map[[32]byte]*list.Element, MaxCompiledSchemas)
}

func (c *lruCache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.list.Len()
}