// leader_resolver_pg_test.go — coverage for the production
// CachedLeaderResolver. The fake `fakeLeaderResolver` in
// writegate_test.go exercises the gate's request-level
// classification; THIS file exercises the cache, the
// singleflight coalescing, and the error propagation in
// the production resolver.
//
// Coverage:
//   - Fresh cache hit (no store call).
//   - Cache miss → store round-trip → snapshot installed.
//   - Refresh signal drains; next call refreshes regardless
//     of TTL.
//   - TTL expiry forces a refresh on the next call.
//   - Empty active set → (name="", isMe=false, err=nil)
//     (NOT an error — the gate maps name=="" to
//     OutcomeLeaderUnreachable via the err branch's
//     equivalence, but the contract here is explicit).
//   - Store error → (name="", isMe=false, err=non-nil).
//   - Singleflight coalesces N parallel callers on cache miss.
//   - isMe is true iff local node name == leader name.
//   - Goroutine-safe under -race (concurrent reads + a single
//     signal-driven refresh).
package writegate

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/onebox-faas/faas/pkg/gateway/leader"
)

// fakeStore is an in-memory leader.LeaderStore. Lets tests
// dial the active-node set deterministically without spinning
// up Postgres.
type fakeStore struct {
	mu    sync.Mutex
	nodes []leader.ComputeNode
	err   error
	calls atomic.Int64 // number of ListActiveComputeNodes invocations
}

func (s *fakeStore) ListActiveComputeNodes(_ context.Context) ([]leader.ComputeNode, error) {
	s.calls.Add(1)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return nil, s.err
	}
	out := make([]leader.ComputeNode, len(s.nodes))
	copy(out, s.nodes)
	return out, nil
}

func (s *fakeStore) set(nodes []leader.ComputeNode, err error) {
	s.mu.Lock()
	s.nodes = nodes
	s.err = err
	s.mu.Unlock()
}

func TestCachedLeaderResolver_CacheHitReturnsCachedValue(t *testing.T) {
	store := &fakeStore{}
	store.set([]leader.ComputeNode{
		{Name: "node-a", NodeID: "uuid-a", Active: true},
	}, nil)
	refresh := make(chan struct{}, 1)
	r := NewCachedLeaderResolver(store, "node-b", time.Minute, refresh)

	// First call: cache miss → store round-trip → snapshot.
	name, isMe, err := r.Current(context.Background())
	if err != nil {
		t.Fatalf("first Current: %v", err)
	}
	if name != "node-a" {
		t.Errorf("first name = %q, want node-a", name)
	}
	if isMe {
		t.Errorf("first isMe = true, want false (local node-b != leader node-a)")
	}
	if got := store.calls.Load(); got != 1 {
		t.Errorf("after first call, store calls = %d, want 1", got)
	}

	// Second call within TTL: cache hit, no store call.
	name, isMe, err = r.Current(context.Background())
	if err != nil {
		t.Fatalf("second Current: %v", err)
	}
	if name != "node-a" || isMe {
		t.Errorf("second = (%q, %v), want (node-a, false)", name, isMe)
	}
	if got := store.calls.Load(); got != 1 {
		t.Errorf("after second call, store calls = %d, want 1 (cache hit, no extra call)", got)
	}
}

func TestCachedLeaderResolver_LocalLeaderIsMe(t *testing.T) {
	store := &fakeStore{}
	store.set([]leader.ComputeNode{
		{Name: "node-a", NodeID: "uuid-a", Active: true},
	}, nil)
	refresh := make(chan struct{}, 1)
	r := NewCachedLeaderResolver(store, "node-a", time.Minute, refresh)

	name, isMe, err := r.Current(context.Background())
	if err != nil {
		t.Fatalf("Current: %v", err)
	}
	if name != "node-a" || !isMe {
		t.Errorf("local-leader Current = (%q, %v), want (node-a, true)", name, isMe)
	}
}

func TestCachedLeaderResolver_TTLExpiryForcesRefresh(t *testing.T) {
	store := &fakeStore{}
	store.set([]leader.ComputeNode{
		{Name: "node-a", Active: true},
	}, nil)
	refresh := make(chan struct{}, 1)
	// TTL = 1 millisecond — every subsequent call after a
	// 2ms sleep is a miss. (1ns was tried first but is
	// racy on fast machines — both calls can land on the
	// same monotonic tick before the resolver reads the
	// cachedAt timestamp.)
	r := NewCachedLeaderResolver(store, "node-b", time.Millisecond, refresh)

	if _, _, err := r.Current(context.Background()); err != nil {
		t.Fatalf("first Current: %v", err)
	}
	time.Sleep(2 * time.Millisecond) // ensure TTL elapsed
	if _, _, err := r.Current(context.Background()); err != nil {
		t.Fatalf("second Current: %v", err)
	}
	if got := store.calls.Load(); got < 2 {
		t.Errorf("store calls = %d, want ≥ 2 (TTL expired between calls)", got)
	}
}

func TestCachedLeaderResolver_RefreshSignalDrains(t *testing.T) {
	store := &fakeStore{}
	store.set([]leader.ComputeNode{
		{Name: "node-a", Active: true},
	}, nil)
	refresh := make(chan struct{}, 1)
	// Long TTL so the cache would otherwise serve stale.
	r := NewCachedLeaderResolver(store, "node-b", time.Hour, refresh)

	// Prime the cache.
	if _, _, err := r.Current(context.Background()); err != nil {
		t.Fatalf("prime Current: %v", err)
	}
	// Now flip the store, send a refresh signal, and confirm
	// the next call sees the new leader without waiting for
	// TTL.
	store.set([]leader.ComputeNode{
		{Name: "node-c", Active: true},
	}, nil)
	refresh <- struct{}{}
	name, _, err := r.Current(context.Background())
	if err != nil {
		t.Fatalf("post-refresh Current: %v", err)
	}
	if name != "node-c" {
		t.Errorf("post-refresh name = %q, want node-c (signal should have forced a refresh)", name)
	}
}

func TestCachedLeaderResolver_EmptyActiveSetIsNotAnError(t *testing.T) {
	store := &fakeStore{}
	store.set(nil, nil) // empty active set
	refresh := make(chan struct{}, 1)
	r := NewCachedLeaderResolver(store, "node-b", time.Minute, refresh)

	name, isMe, err := r.Current(context.Background())
	if err != nil {
		t.Fatalf("Current with empty active set should not error: %v", err)
	}
	if name != "" {
		t.Errorf("name = %q, want empty", name)
	}
	if isMe {
		t.Errorf("isMe = true, want false (no leader exists)")
	}
}

func TestCachedLeaderResolver_StoreErrorPropagates(t *testing.T) {
	store := &fakeStore{}
	wantErr := errors.New("simulated pg outage")
	store.set(nil, wantErr)
	refresh := make(chan struct{}, 1)
	r := NewCachedLeaderResolver(store, "node-b", time.Minute, refresh)

	_, _, err := r.Current(context.Background())
	if !errors.Is(err, wantErr) {
		t.Errorf("Current err = %v, want %v", err, wantErr)
	}
}

func TestCachedLeaderResolver_SingleflightCoalescesParallelCalls(t *testing.T) {
	// Slow store: forces parallel callers to queue at the
	// singleflight barrier rather than racing into the
	// store.
	store := &slowFakeStore{delay: 50 * time.Millisecond}
	store.set([]leader.ComputeNode{
		{Name: "node-a", Active: true},
	}, nil)
	refresh := make(chan struct{}, 1)
	r := NewCachedLeaderResolver(store, "node-b", time.Minute, refresh)

	const n = 32
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			if _, _, err := r.Current(context.Background()); err != nil {
				t.Errorf("parallel Current: %v", err)
			}
		}()
	}
	wg.Wait()

	// Singleflight collapses N callers into ONE store
	// round-trip; the parallel herd should NOT cause N
	// separate calls.
	if got := store.calls.Load(); got > 2 {
		t.Errorf("store calls under singleflight = %d, want ≤ 2 (one for the herd, possibly one for the after-queue re-check)", got)
	}
}

func TestCachedLeaderResolver_RaceSafe(t *testing.T) {
	store := &fakeStore{}
	store.set([]leader.ComputeNode{
		{Name: "node-a", Active: true},
	}, nil)
	refresh := make(chan struct{}, 1)
	r := NewCachedLeaderResolver(store, "node-b", time.Millisecond, refresh)

	// Hammer the resolver: 100 goroutines, 100 iterations
	// each, with periodic publisher signals. Run under -race
	// to catch any unprotected access.
	const goroutines = 100
	const iterations = 100
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < iterations; j++ {
				if _, _, err := r.Current(context.Background()); err != nil {
					t.Errorf("hammer Current: %v", err)
					return
				}
			}
		}()
	}
	// Send a refresh signal every 5ms — should be drained
	// without races.
	go func() {
		for i := 0; i < 50; i++ {
			select {
			case refresh <- struct{}{}:
			default:
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()
	wg.Wait()
}

// slowFakeStore wraps fakeStore with an artificial delay so
// the singleflight coalescing test can produce a herd of
// waiting callers (the in-process fake is otherwise so fast
// that N goroutines would never overlap).
type slowFakeStore struct {
	fakeStore
	delay time.Duration
}

func (s *slowFakeStore) ListActiveComputeNodes(ctx context.Context) ([]leader.ComputeNode, error) {
	select {
	case <-time.After(s.delay):
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return s.fakeStore.ListActiveComputeNodes(ctx)
}
