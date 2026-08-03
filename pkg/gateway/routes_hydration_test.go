package gateway

import (
	"context"
	"errors"
	"testing"
)

// TestRouteCacheHydration_DefaultsNotHydrated pins the zero-value
// contract: a fresh tracker reports not-hydrated.
func TestRouteCacheHydration_DefaultsNotHydrated(t *testing.T) {
	var h RouteCacheHydration
	ok, _ := h.Hydrated()
	if ok {
		t.Errorf("zero-value RouteCacheHydration reports hydrated")
	}
}

// TestRouteCacheHydration_MarkHydrated flips the bit on.
func TestRouteCacheHydration_MarkHydrated(t *testing.T) {
	var h RouteCacheHydration
	h.MarkHydrated()
	ok, reason := h.Hydrated()
	if !ok {
		t.Errorf("MarkHydrated did not flip bit")
	}
	if reason != "" {
		t.Errorf("MarkHydrated reason = %q, want \"\"", reason)
	}
}

// TestRouteCacheHydration_MarkUnhydrated keeps the bit off and
// surfaces the reason (used by the /readyz body).
func TestRouteCacheHydration_MarkUnhydrated(t *testing.T) {
	var h RouteCacheHydration
	h.MarkHydrated()
	h.MarkUnhydrated("pg unreachable at boot")
	ok, reason := h.Hydrated()
	if ok {
		t.Errorf("MarkUnhydrated did not flip bit off")
	}
	if reason != "pg unreachable at boot" {
		t.Errorf("reason = %q, want \"pg unreachable at boot\"", reason)
	}
}

// TestRouteCacheHydration_MarkHydratedIdempotent verifies repeated
// MarkHydrated calls do not flip the reason to "" when it was
// already empty — defensive against accidental "MarkHydrated clears
// reason" semantics.
func TestRouteCacheHydration_MarkHydratedIdempotent(t *testing.T) {
	var h RouteCacheHydration
	h.MarkHydrated()
	h.MarkHydrated()
	ok, reason := h.Hydrated()
	if !ok || reason != "" {
		t.Errorf("idempotent MarkHydrated broke: ok=%v reason=%q", ok, reason)
	}
}

// fakeLoader is a stub RouteCacheLoader used by the contract test.
// It records the order of hydration transitions and writes to the
// cache the way the production loader does.
type fakeLoader struct {
	hydrateAfterPut  bool
	hydrateOnSuccess bool
	fail             bool
}

func (f *fakeLoader) LoadRouteCache(ctx context.Context, cache *RouteCache, hydration *RouteCacheHydration) error {
	if f.fail {
		hydration.MarkUnhydrated("loader failed: synthetic error")
		return errors.New("synthetic error")
	}
	cache.Put("a.apps.dom", "app-a")
	cache.Put("b.apps.dom", "app-b")
	if f.hydrateAfterPut {
		// Wrong order — loader should hydrate BEFORE returning.
		hydration.MarkHydrated()
	}
	if f.hydrateOnSuccess {
		hydration.MarkHydrated()
	}
	return nil
}

// TestRouteCacheLoader_Contract_OnSuccess flips hydration on and
// populates the cache.
func TestRouteCacheLoader_Contract_OnSuccess(t *testing.T) {
	cache := NewRouteCache(10)
	hydration := NewRouteCacheHydration()
	loader := &fakeLoader{hydrateOnSuccess: true}
	if err := loader.LoadRouteCache(context.Background(), cache, hydration); err != nil {
		t.Fatalf("LoadRouteCache returned %v, want nil", err)
	}
	ok, _ := hydration.Hydrated()
	if !ok {
		t.Errorf("hydration bit not flipped after successful load")
	}
	if _, hit := cache.Get("a.apps.dom"); !hit {
		t.Errorf("route cache missing a.apps.dom after load")
	}
	if _, hit := cache.Get("b.apps.dom"); !hit {
		t.Errorf("route cache missing b.apps.dom after load")
	}
}

// TestRouteCacheLoader_Contract_OnFailure keeps hydration off and
// surfaces the reason so /readyz reflects the failure.
func TestRouteCacheLoader_Contract_OnFailure(t *testing.T) {
	cache := NewRouteCache(10)
	hydration := NewRouteCacheHydration()
	loader := &fakeLoader{fail: true}
	err := loader.LoadRouteCache(context.Background(), cache, hydration)
	if err == nil {
		t.Fatalf("LoadRouteCache returned nil, want error")
	}
	ok, reason := hydration.Hydrated()
	if ok {
		t.Errorf("hydration bit flipped after failed load")
	}
	if reason == "" {
		t.Errorf("hydration reason empty after failed load")
	}
}

// TestRouteCacheHydration_ConcurrentReads verifies the bit is safe
// for concurrent reads on the /readyz hot path.
func TestRouteCacheHydration_ConcurrentReads(t *testing.T) {
	var h RouteCacheHydration
	h.MarkHydrated()
	done := make(chan struct{})
	go func() {
		for i := 0; i < 1000; i++ {
			h.MarkUnhydrated("flip")
			h.MarkHydrated()
		}
		close(done)
	}()
	for i := 0; i < 1000; i++ {
		// No assertion on the value (it's racy by design) — we
		// only assert that the call does not panic or deadlock.
		_, _ = h.Hydrated()
	}
	<-done
}
