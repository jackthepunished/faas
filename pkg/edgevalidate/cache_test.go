package edgevalidate_test

import (
	"testing"

	edgevalidate "github.com/onebox-faas/faas/pkg/edgevalidate"
)

// sampleSchema is a tiny schema used by the cache tests. The body
// contents don't matter for cache-shape assertions; only the digest
// identity does.
const sampleSchema = `{"type":"object","properties":{"name":{"type":"string"}}}`

func TestCache_RoundTrip(t *testing.T) {
	t.Parallel()
	c := edgevalidate.NewCache()
	compiled, err := edgevalidate.Compile([]byte(sampleSchema), false)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	if _, ok := c.Get(compiled.Digest); ok {
		t.Fatal("cache hit on empty cache")
	}
	c.Register(compiled.Digest, compiled)
	got, ok := c.Get(compiled.Digest)
	if !ok {
		t.Fatal("cache miss after Register")
	}
	if got != compiled {
		t.Fatal("cache returned a different CompiledSchema pointer")
	}
	if c.Len() != 1 {
		t.Fatalf("Len: want 1, got %d", c.Len())
	}
}

func TestCache_OverwriteOnDuplicateRegister(t *testing.T) {
	t.Parallel()
	c := edgevalidate.NewCache()
	a, err := edgevalidate.Compile([]byte(sampleSchema), false)
	if err != nil {
		t.Fatalf("Compile a: %v", err)
	}
	b, err := edgevalidate.Compile([]byte(sampleSchema), false)
	if err != nil {
		t.Fatalf("Compile b: %v", err)
	}
	c.Register(a.Digest, a)
	c.Register(b.Digest, b) // same digest, different pointer
	got, ok := c.Get(a.Digest)
	if !ok {
		t.Fatal("miss after duplicate Register")
	}
	// The most recent Register wins — b overwrites a.
	if got != b {
		t.Fatal("duplicate Register did not overwrite with the new pointer")
	}
	if c.Len() != 1 {
		t.Fatalf("Len: want 1 (no duplicates), got %d", c.Len())
	}
}

func TestCache_LRUEvictionAtCap(t *testing.T) {
	t.Parallel()
	// Cap is edgevalidate.MaxCompiledSchemas = 1024. We don't
	// want to compile 1025 real schemas (each Compile takes
	// ~µs but the test wall-clock shouldn't depend on it); the
	// LRU cap is the only invariant we care about here.
	//
	// We fake the cap by registering with a hand-crafted digest
	// + a nil *CompiledSchema (the cache doesn't deref it on
	// Register, only on Get, and Get isn't called in this test).
	c := edgevalidate.NewCache()
	// Reach MaxCompiledSchemas + 1 registrations.
	for i := 0; i < edgevalidate.MaxCompiledSchemas+1; i++ {
		var d [32]byte
		d[0] = byte(i)
		d[1] = byte(i >> 8)
		d[2] = byte(i >> 16)
		d[3] = byte(i >> 24)
		c.Register(d, nil)
	}
	if c.Len() != edgevalidate.MaxCompiledSchemas {
		t.Fatalf("Len after overflow: want %d, got %d",
			edgevalidate.MaxCompiledSchemas, c.Len())
	}
}

func TestCache_Reset(t *testing.T) {
	t.Parallel()
	c := edgevalidate.NewCache()
	compiled, err := edgevalidate.Compile([]byte(sampleSchema), false)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	c.Register(compiled.Digest, compiled)
	if c.Len() != 1 {
		t.Fatalf("setup: Len=%d, want 1", c.Len())
	}
	c.Reset()
	if c.Len() != 0 {
		t.Fatalf("Len after Reset: want 0, got %d", c.Len())
	}
	if _, ok := c.Get(compiled.Digest); ok {
		t.Fatal("cache hit after Reset")
	}
	// After Reset the cache is still usable.
	c.Register(compiled.Digest, compiled)
	if _, ok := c.Get(compiled.Digest); !ok {
		t.Fatal("cache miss after Reset+Register")
	}
}
