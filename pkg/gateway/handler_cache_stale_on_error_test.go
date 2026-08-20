package gateway

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/onebox-faas/faas/pkg/api"
)

// TestStaleOnError_NoRuleNoServe verifies the no-rule path
// returns false. Without a matched rule there's nothing to
// serve stale from; the caller falls through to writeWakeError.
func TestStaleOnError_NoRuleNoServe(t *testing.T) {
	h, _, _ := newTestHandler(t)
	h.WithResponseCache(NewResponseCache())
	req := httptest.NewRequest("GET", "http://jane-api.apps.dom/catalog", nil)
	w := httptest.NewRecorder()
	rec := newTestStatusRecorder(w)
	served, outcome := h.tryServeStaleOnWakeError(w, req, App{ID: "app-1", Plan: api.PlanPro}, rec)
	if served || outcome != "" {
		t.Fatalf("no-rule path: served=%v outcome=%q, want false/\"\"", served, outcome)
	}
}

// TestStaleOnError_ServesStaleOnWakeFailure verifies the happy
// path: a stale-eligible entry is served when the wake gate
// has failed. The replay carries the Warning: 110 header per
// RFC 7234 §5.5.2.
func TestStaleOnError_ServesStaleOnWakeFailure(t *testing.T) {
	now := time.Now()
	clock := func() time.Time { return now }
	cache := NewResponseCacheWithClock(DefaultResponseCacheMaxBytes, clock)
	h, _, _ := newTestHandler(t)
	h.WithResponseCache(cache)
	rule := EdgeRuleCacheResolved{
		ID:                  "rule-1",
		PathGlob:            "/catalog",
		MaxAgeSeconds:       60,
		StaleIfErrorSeconds: 300,
	}
	app := App{ID: "app-1", Plan: api.PlanPro}
	key := CacheKey{AppID: app.ID, RuleID: rule.ID, Method: "GET", NormalizedPath: "/catalog", VaryHash: hashStable("")}
	// Stale-eligible: freshUntil in the past, staleUntil in the future.
	cache.Put(key, 200,
		http.Header{"Content-Type": []string{"application/json"}},
		[]byte(`{"items":["stale"]}`),
		now.Add(-1*time.Second),
		now.Add(300*time.Second),
		rule.toStateEdgeRuleCacheAction(),
	)
	req := httptest.NewRequest("GET", "http://jane-api.apps.dom/catalog", nil)
	req = req.WithContext(withCacheRuleContext(req.Context(), &rule, app.ID, "GET", "/catalog", "", hashStable("")))
	w := httptest.NewRecorder()
	rec := newTestStatusRecorder(w)
	served, outcome := h.tryServeStaleOnWakeError(w, req, app, rec)
	if !served {
		t.Fatalf("expected stale-on-error serve")
	}
	if outcome != "stale_if_error_served" {
		t.Errorf("outcome = %q, want stale_if_error_served", outcome)
	}
	if !strings.Contains(w.Body.String(), `"stale"`) {
		t.Errorf("body = %q, want stale body", w.Body.String())
	}
	warn := w.Header().Get("Warning")
	if warn == "" || !strings.Contains(warn, "110") {
		t.Errorf("Warning header = %q, want 110", warn)
	}
}

// TestStaleOnError_BypassedOnAuth verifies authed requests
// cannot trigger a stale serve. The applier's
// Authorization-bypass predicate is duplicated here so the
// security posture is the single chokepoint.
func TestStaleOnError_BypassedOnAuth(t *testing.T) {
	now := time.Now()
	cache := NewResponseCacheWithClock(DefaultResponseCacheMaxBytes, func() time.Time { return now })
	h, _, _ := newTestHandler(t)
	h.WithResponseCache(cache)
	rule := EdgeRuleCacheResolved{ID: "rule-1", PathGlob: "/catalog", MaxAgeSeconds: 60, StaleIfErrorSeconds: 300}
	app := App{ID: "app-1", Plan: api.PlanPro}
	key := CacheKey{AppID: app.ID, RuleID: rule.ID, Method: "GET", NormalizedPath: "/catalog", VaryHash: hashStable("")}
	cache.Put(key, 200, http.Header{}, []byte("stale"), now.Add(-1*time.Second), now.Add(300*time.Second), rule.toStateEdgeRuleCacheAction())
	req := httptest.NewRequest("GET", "http://jane-api.apps.dom/catalog", nil)
	req.Header.Set("Authorization", "Bearer x")
	req = req.WithContext(withCacheRuleContext(req.Context(), &rule, app.ID, "GET", "/catalog", "", hashStable("")))
	w := httptest.NewRecorder()
	rec := newTestStatusRecorder(w)
	served, _ := h.tryServeStaleOnWakeError(w, req, app, rec)
	if served {
		t.Fatalf("authed stale-on-error serve must be a no-op")
	}
}

// TestStaleOnError_StaleDisabledOnRule verifies that a rule
// with StaleIfErrorSeconds=0 disables the stale-on-error path
// entirely. The applier still consults the cache (for fresh
// hits), but the wake-failure branch MUST NOT serve.
func TestStaleOnError_StaleDisabledOnRule(t *testing.T) {
	now := time.Now()
	cache := NewResponseCacheWithClock(DefaultResponseCacheMaxBytes, func() time.Time { return now })
	h, _, _ := newTestHandler(t)
	h.WithResponseCache(cache)
	rule := EdgeRuleCacheResolved{ID: "rule-1", PathGlob: "/catalog", MaxAgeSeconds: 60, StaleIfErrorSeconds: 0}
	app := App{ID: "app-1", Plan: api.PlanPro}
	key := CacheKey{AppID: app.ID, RuleID: rule.ID, Method: "GET", NormalizedPath: "/catalog", VaryHash: hashStable("")}
	cache.Put(key, 200, http.Header{}, []byte("stale"), now.Add(-1*time.Second), now.Add(300*time.Second), rule.toStateEdgeRuleCacheAction())
	req := httptest.NewRequest("GET", "http://jane-api.apps.dom/catalog", nil)
	req = req.WithContext(withCacheRuleContext(req.Context(), &rule, app.ID, "GET", "/catalog", "", hashStable("")))
	w := httptest.NewRecorder()
	rec := newTestStatusRecorder(w)
	served, _ := h.tryServeStaleOnWakeError(w, req, app, rec)
	if served {
		t.Fatalf("StaleIfErrorSeconds=0 must disable stale serve")
	}
}

// TestStaleOnError_NoEntryNoServe verifies that a stale-rule
// with no cache entry returns false. The cache lookup is the
// authoritative gate.
func TestStaleOnError_NoEntryNoServe(t *testing.T) {
	now := time.Now()
	cache := NewResponseCacheWithClock(DefaultResponseCacheMaxBytes, func() time.Time { return now })
	h, _, _ := newTestHandler(t)
	h.WithResponseCache(cache)
	rule := EdgeRuleCacheResolved{ID: "rule-1", PathGlob: "/catalog", MaxAgeSeconds: 60, StaleIfErrorSeconds: 300}
	app := App{ID: "app-1", Plan: api.PlanPro}
	req := httptest.NewRequest("GET", "http://jane-api.apps.dom/catalog", nil)
	req = req.WithContext(withCacheRuleContext(req.Context(), &rule, app.ID, "GET", "/catalog", "", hashStable("")))
	w := httptest.NewRecorder()
	rec := newTestStatusRecorder(w)
	served, _ := h.tryServeStaleOnWakeError(w, req, app, rec)
	if served {
		t.Fatalf("no-entry path must produce false")
	}
}

// TestStaleOnError_FreshEntryStillNoServe verifies that a
// fresh entry on a wake-failure path is NOT served via the
// stale-on-error branch. A fresh entry should have been served
// at the top of ServeHTTP by applyEdgeRuleCache — if it
// wasn't (e.g. concurrent eviction between consult and
// wake-failure), the wake-failure branch treats it the same
// as a miss and falls through. (The actual fresh-serve
// invariant is enforced by applyEdgeRuleCache's earlier
// return; this test fences against a future refactor that
// re-binds the stale branch to also serve fresh.)
func TestStaleOnError_FreshEntryStillNoServe(t *testing.T) {
	now := time.Now()
	cache := NewResponseCacheWithClock(DefaultResponseCacheMaxBytes, func() time.Time { return now })
	h, _, _ := newTestHandler(t)
	h.WithResponseCache(cache)
	rule := EdgeRuleCacheResolved{ID: "rule-1", PathGlob: "/catalog", MaxAgeSeconds: 60, StaleIfErrorSeconds: 300}
	app := App{ID: "app-1", Plan: api.PlanPro}
	key := CacheKey{AppID: app.ID, RuleID: rule.ID, Method: "GET", NormalizedPath: "/catalog", VaryHash: hashStable("")}
	cache.Put(key, 200, http.Header{}, []byte("fresh"), now.Add(60*time.Second), now.Add(360*time.Second), rule.toStateEdgeRuleCacheAction())
	req := httptest.NewRequest("GET", "http://jane-api.apps.dom/catalog", nil)
	req = req.WithContext(withCacheRuleContext(req.Context(), &rule, app.ID, "GET", "/catalog", "", hashStable("")))
	w := httptest.NewRecorder()
	rec := newTestStatusRecorder(w)
	served, _ := h.tryServeStaleOnWakeError(w, req, app, rec)
	if served {
		t.Fatalf("stale branch must not serve fresh entries")
	}
}
