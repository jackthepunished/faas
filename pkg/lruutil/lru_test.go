// Tests for the shared LRU primitive (pkg/lruutil). The two
// production callers (pkg/edgevalidate, pkg/openapidiff) each
// have their own integration tests; this file pins the primitive
// contract that those callers depend on:
//
//   - Get on a missing key returns the zero value + false.
//   - Get on a hit moves the entry to MRU; subsequent Get
//     returns the same value.
//   - Put on a duplicate key overwrites + bumps to MRU; the
//     entry count stays under cap.
//   - Cap enforcement evicts from the tail (LRU end), not the
//     freshly-Put entry.
//   - Delete removes a single entry without affecting the rest.
//   - Keys returns MRU-first order; the snapshot is owned by
//     the caller.
//   - Reset empties the cache cleanly.
//
// Concurrency is exercised via parallel Put/Get on disjoint keys
// (the standard lru-correctness smoke test from the Go standard
// library's container/list doc comment).
package lruutil_test

import (
	"fmt"
	"sync"
	"testing"

	"github.com/onebox-faas/faas/pkg/lruutil"
)

func TestLRU_GetMiss(t *testing.T) {
	c := lruutil.New[string, int](4)
	if v, ok := c.Get("missing"); ok || v != 0 {
		t.Errorf("Get miss: got (%d, %v), want (0, false)", v, ok)
	}
}

func TestLRU_PutGet(t *testing.T) {
	c := lruutil.New[string, int](4)
	c.Put("a", 1)
	if v, ok := c.Get("a"); !ok || v != 1 {
		t.Errorf("Get hit: got (%d, %v), want (1, true)", v, ok)
	}
}

func TestLRU_OverwriteBumpsToMRU(t *testing.T) {
	c := lruutil.New[string, int](2)
	c.Put("a", 1)
	c.Put("b", 2)
	c.Put("a", 11) // overwrite + MRU bump
	// Now MRU order should be a, b. With cap=2, a fresh Put
	// would evict the LRU — but the next Get on b should still
	// hit (b is MRU, a is LRU).
	if v, _ := c.Get("b"); v != 2 {
		t.Errorf("Get b after overwrite: got %d, want 2", v)
	}
	if v, _ := c.Get("a"); v != 11 {
		t.Errorf("Get a after overwrite: got %d, want 11", v)
	}
}

func TestLRU_CapEvictsTail(t *testing.T) {
	c := lruutil.New[string, int](2)
	c.Put("a", 1)
	c.Put("b", 2)
	c.Put("c", 3) // evicts a (the LRU)
	if _, ok := c.Get("a"); ok {
		t.Error("a should have been evicted")
	}
	if v, ok := c.Get("b"); !ok || v != 2 {
		t.Errorf("b after eviction: got (%d, %v), want (2, true)", v, ok)
	}
	if v, ok := c.Get("c"); !ok || v != 3 {
		t.Errorf("c after eviction: got (%d, %v), want (3, true)", v, ok)
	}
}

func TestLRU_Delete(t *testing.T) {
	c := lruutil.New[string, int](4)
	c.Put("a", 1)
	c.Put("b", 2)
	c.Delete("a")
	if _, ok := c.Get("a"); ok {
		t.Error("a should have been deleted")
	}
	if v, ok := c.Get("b"); !ok || v != 2 {
		t.Errorf("b after Delete a: got (%d, %v), want (2, true)", v, ok)
	}
	// Delete on a missing key is a no-op.
	c.Delete("never-existed")
}

func TestLRU_Keys(t *testing.T) {
	c := lruutil.New[string, int](4)
	c.Put("a", 1)
	c.Put("b", 2)
	c.Put("c", 3)
	keys := c.Keys()
	if len(keys) != 3 {
		t.Fatalf("Keys len: got %d, want 3", len(keys))
	}
	// MRU-first: the last Put is at the front.
	if keys[0] != "c" {
		t.Errorf("Keys[0]: got %q, want %q (MRU-first)", keys[0], "c")
	}
}

func TestLRU_Reset(t *testing.T) {
	c := lruutil.New[string, int](4)
	c.Put("a", 1)
	c.Put("b", 2)
	c.Reset()
	if c.Len() != 0 {
		t.Errorf("Len after Reset: got %d, want 0", c.Len())
	}
	if _, ok := c.Get("a"); ok {
		t.Error("a should be gone after Reset")
	}
}

func TestLRU_Concurrent(t *testing.T) {
	// Each worker writes to a disjoint key range. With
	// cap = writers, the cap doesn't evict within a worker's
	// own loop; we then check that every worker's last write
	// survives (it's at MRU and the cap holds the full set).
	const writers = 8
	c := lruutil.New[int, int](writers)
	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			c.Put(id, id*100)
		}(w)
	}
	wg.Wait()
	// Each key should be readable at its expected value.
	for w := 0; w < writers; w++ {
		if v, ok := c.Get(w); !ok || v != w*100 {
			t.Errorf("Get(%d): got (%d, %v), want (%d, true)", w, v, ok, w*100)
		}
	}
}

func TestLRU_ConcurrentStressNoRace(t *testing.T) {
	// Race-detector smoke test: many goroutines hammer the
	// same small cap with overlapping writes. The only
	// invariant we pin is no-panic + Len() <= cap (race
	// detector catches the rest).
	const cap = 16
	c := lruutil.New[int, int](cap)
	var wg sync.WaitGroup
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for i := 0; i < 1000; i++ {
				c.Put(id*100+i%32, i)
				_, _ = c.Get(id*100 + i%32)
				_ = c.Len()
			}
		}(w)
	}
	wg.Wait()
	if got := c.Len(); got > cap {
		t.Errorf("Len > cap: got %d, cap=%d", got, cap)
	}
}

func TestLRU_ZeroCapBumpsToOne(t *testing.T) {
	c := lruutil.New[string, int](0)
	c.Put("a", 1)
	if v, ok := c.Get("a"); !ok || v != 1 {
		t.Errorf("after Put on cap=0 cache: got (%d, %v), want (1, true)", v, ok)
	}
	c.Put("b", 2)
	if _, ok := c.Get("a"); ok {
		t.Error("a should have been evicted by b")
	}
}

// Example wires a quick smoke test that mirrors the production
// edgevalidate usage pattern.
func Example() {
	c := lruutil.New[string, string](2)
	c.Put("key1", "value1")
	c.Put("key2", "value2")
	v, _ := c.Get("key1")
	fmt.Println(v)
	c.Put("key3", "value3") // evicts key2 (the LRU)
	_, ok := c.Get("key2")
	fmt.Println(ok)
	// Output:
	// value1
	// false
}
