package gateway

import (
	"crypto/sha256"
	"sync"
	"time"
)

// PublicAuthCache (issue #477 / ADR-079) caches the
// unsealed basic-auth credential per app so the hot path
// doesn't invoke the secretbox unseal on every request.
// Three properties matter:
//
//  1. 60s TTL (issue / ADR). Plain expiry; no jitter
//     (matches pkg/gateway/routes.go's route-cache
//     precedent). At worst a credential flip is visible
//     within 60s through the TTL; the per-key invalidation
//     path brings it down to ~1s for the cases that matter
//     (a key rotation that affects a public-auth-locked
//     app).
//  2. Per-app invalidation. A db.NotifyAppChanged payload
//     triggers InvalidateByApp(appID); a db.NotifyKeyChanged
//     triggers InvalidateAll (the cache maps (appID,
//     sealed-hash) → entry without a per-entry key-tag, so
//     a key rotation has to drop everything — see the
//     InvalidateByAPIKey doc-comment for the TODO that
//     adds the per-entry key-tag when a real workload
//     signals thrash).
//  3. Lazy expiry on Get. Expired entries are dropped on
//     the next read; no background sweeper goroutine. This
//     keeps the cache allocation-light and avoids the
//     goroutine-leak surface across daemon reloads. The
//     TTL window bounds the staleness either way.
//
// The cache key is appID + sealed-hash. The sealed-hash is
// the sha-256 of the BasicSealed blob so a re-seal (a new
// secretbox key, ADR-057) generates a fresh key and the
// cache miss path re-unseals cleanly.
//
// The on-miss loader closure is invoked synchronously
// inside Get; pkg/gateway's basic-auth branch threads its
// PublicAuthUnsealer through the closure so pkg/gateway
// stays free of any pkg/secretbox import (the same
// zero-dep posture enforceRequireAuthn has on
// pkg/auth.Middleware).
type PublicAuthCache struct {
	ttl  time.Duration
	now  func() time.Time // injectable for tests
	mu   sync.RWMutex
	data map[publicAuthCacheKey]publicAuthCacheEntry
}

// publicAuthCacheKey is the map key. appID identifies
// which app the unsealed creds belong to; sealedHash is
// the sha-256 of the secretbox blob so a re-seal produces
// a fresh key. Hashing the blob (rather than the blob
// itself) bounds the map key size + avoids keeping
// ciphertext in memory twice.
type publicAuthCacheKey struct {
	appID      string
	sealedHash [32]byte
}

// publicAuthCacheEntry holds the unsealed credential + the
// absolute expiry timestamp. The expiry is captured at
// Put() time using the cache's now() clock so test code
// can advance time without a real timer.
type publicAuthCacheEntry struct {
	username  string
	password  string
	expiresAt time.Time
}

// publicAuthDefaultTTL is the issue / ADR-specified TTL.
// 60s balances cache hit rate against credential-flip
// staleness. Operators that need tighter bounds can drive
// the cache through per-key invalidation
// (InvalidateByAPIKey) instead of waiting for the TTL.
const publicAuthDefaultTTL = 60 * time.Second

// NewPublicAuthCache constructs a cache with the default
// 60s TTL and the wall-clock time source. Production
// wires this from cmd/gatewayd-internal/main.go; tests
// prefer NewPublicAuthCacheWithClock so they can advance
// time deterministically.
func NewPublicAuthCache() *PublicAuthCache {
	return NewPublicAuthCacheWithTTL(publicAuthDefaultTTL, time.Now)
}

// NewPublicAuthCacheWithTTL lets tests inject a custom
// TTL + clock. ttl=0 collapses to a no-op cache (every Get
// is a miss); clock=nil falls back to time.Now. The
// clock-shaped now func means tests can advance the wall
// clock by reassigning the field directly (mutating the
// returned cache is safe; the mu lock guards Get/Put).
func NewPublicAuthCacheWithTTL(ttl time.Duration, now func() time.Time) *PublicAuthCache {
	if now == nil {
		now = time.Now
	}
	return &PublicAuthCache{
		ttl:  ttl,
		now:  now,
		data: make(map[publicAuthCacheKey]publicAuthCacheEntry),
	}
}

// PublicAuthLoader is the on-miss closure
// PublicAuthCache.Get invokes. Returning ok=false signals
// a hard failure (sealed blob tampered, unsealer wired
// wrong, etc.); the caller treats this as a credential
// mismatch so a brute-forcer can't tell the difference
// between "no creds configured" and "wrong creds".
type PublicAuthLoader func() (username, password string, ok bool)

// Get returns the unsealed credential for the
// (appID, sealed) pair, calling loader on a cache miss.
// Expired entries are dropped on read (the next Put
// re-seeds). The boolean is false on miss + loader-failed;
// the caller falls through to the credential-mismatch
// 401 path.
func (c *PublicAuthCache) Get(appID string, sealed []byte, loader PublicAuthLoader) (username, password string, ok bool) {
	if c == nil {
		// Nil-cache pass-through: every Get is a miss; the
		// caller falls through to the per-request unseal
		// path. Used by unit tests that don't want to thread
		// a cache through the constructor.
		return loader()
	}
	key := publicAuthCacheKey{appID: appID, sealedHash: sealedSHA(sealed)}
	// Fast path: read-locked hit.
	c.mu.RLock()
	entry, found := c.data[key]
	c.mu.RUnlock()
	if found {
		if c.now().Before(entry.expiresAt) {
			return entry.username, entry.password, true
		}
		// Expired — drop on the floor and re-load below.
		c.mu.Lock()
		// Re-check under write lock so a concurrent Put
		// doesn't get clobbered.
		if cur, ok := c.data[key]; ok && c.now().Before(cur.expiresAt) {
			c.mu.Unlock()
			return cur.username, cur.password, true
		}
		delete(c.data, key)
		c.mu.Unlock()
	}
	// Slow path: write-locked load + insert.
	u, p, ok := loader()
	if !ok {
		return "", "", false
	}
	c.mu.Lock()
	c.data[key] = publicAuthCacheEntry{
		username:  u,
		password:  p,
		expiresAt: c.now().Add(c.ttl),
	}
	c.mu.Unlock()
	return u, p, true
}

// InvalidateByApp drops every cached entry for appID.
// Used by tests; production wires the apid commit path's
// app_changed emit to trigger this so a credential flip
// (PATCH public_auth) is visible within ~1s instead of
// waiting for the 60s TTL.
func (c *PublicAuthCache) InvalidateByApp(appID string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for k := range c.data {
		if k.appID == appID {
			delete(c.data, k)
		}
	}
}

// InvalidateAll drops every cached entry. Used by the
// db.NotifyKeyChanged handler (cmd/gatewayd-internal) —
// the cache maps (appID, sealed-hash) → entry without a
// per-entry key-tag, so a key rotation has to drop
// everything. Operators who rotate a key on a busy
// account pay at most one cache rebuild across all
// public-auth-locked apps; acceptable because key
// rotations are rare and the next request re-unseals
// cleanly.
//
// TODO(ADR-079 follow-up): per-entry key-tag would let us
// drop only the affected entries. Held until a real
// workload signals thrash; the simpler "drop all on key
// rotation" path is correct, just less surgical.
func (c *PublicAuthCache) InvalidateAll() {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.data = make(map[publicAuthCacheKey]publicAuthCacheEntry)
	c.mu.Unlock()
}

// Len returns the current entry count. Used by tests +
// future metrics; not on the hot path.
func (c *PublicAuthCache) Len() int {
	if c == nil {
		return 0
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.data)
}

// sealedSHA hashes the secretbox-sealed blob so the cache
// key is fixed-size + doesn't keep ciphertext in memory
// twice.
func sealedSHA(sealed []byte) [32]byte {
	return sha256.Sum256(sealed)
}
