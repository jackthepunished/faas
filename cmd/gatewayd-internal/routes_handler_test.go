// Whitebox tests for /v1/internal/apps/{slug}/routes — ADR-093
// control-listener reader. The handler is the seam between
// gatewayd-internal's in-process route-set state (Handler.RoutesFor)
// and the apid reverse-proxy hop.
package main

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/onebox-faas/faas/pkg/gateway"
)

// TestInternalRoutesHandler_KnownAppReturnsAdmittedRoutes seeds
// the Handler with two admitted routes and asserts the JSON
// shape the apid reverse-proxy decodes.
func TestInternalRoutesHandler_KnownAppReturnsAdmittedRoutes(t *testing.T) {
	h := &gateway.Handler{}
	set := h.RouteSetForTest("app-1")
	set.AdmitForTest("GET /users")
	set.AdmitForTest("POST /orders")
	lookup := gateway.ResolveSlugFn(func(slug string) (string, bool) {
		if slug == "jane-api" {
			return "app-1", true
		}
		return "", false
	})
	srv := httptest.NewServer(internalRoutesHandler(h, lookup, slog.New(slog.NewJSONHandler(io.Discard, nil))))
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/v1/internal/apps/jane-api/routes")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var body routesResponseJSON
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Slug != "jane-api" {
		t.Errorf("Slug = %q, want jane-api", body.Slug)
	}
	if body.AppID != "app-1" {
		t.Errorf("AppID = %q, want app-1", body.AppID)
	}
	if !contains(body.Routes, "GET /users") || !contains(body.Routes, "POST /orders") {
		t.Errorf("Routes = %v, want to include admitted labels", body.Routes)
	}
}

// TestInternalRoutesHandler_UnknownSlugRendersEmpty renders the
// "unknown slug" branch as an empty Routes array (not a 404)
// so the dashboard doesn't distinguish "unknown slug" from
// "no traffic yet" — an enumeration oracle on the loopback
// surface.
func TestInternalRoutesHandler_UnknownSlugRendersEmpty(t *testing.T) {
	h := &gateway.Handler{}
	lookup := gateway.ResolveSlugFn(func(slug string) (string, bool) {
		return "", false
	})
	srv := httptest.NewServer(internalRoutesHandler(h, lookup, nil))
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/v1/internal/apps/never-seen/routes")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (empty-routes-not-404 contract)", resp.StatusCode)
	}
	var body routesResponseJSON
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Routes) != 0 {
		t.Errorf("Routes = %v, want empty", body.Routes)
	}
}

// TestInternalRoutesHandler_MissingSlug400 asserts the
// missing-slug branch (no path segment after /v1/internal/apps/)
// returns 400, not a 200 with the apid-loopback's other app.
func TestInternalRoutesHandler_MissingSlug400(t *testing.T) {
	h := &gateway.Handler{}
	lookup := gateway.ResolveSlugFn(func(slug string) (string, bool) {
		t.Fatalf("lookup called for empty slug; should 400 first")
		return "", false
	})
	srv := httptest.NewServer(internalRoutesHandler(h, lookup, nil))
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/v1/internal/apps//routes")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

// TestInternalRoutesHandler_NilLookupReturns503 covers the
// mis-wired control listener path (production always passes a
// real ResolveSlugFn; nil is the unit-test seam).
func TestInternalRoutesHandler_NilLookupReturns503(t *testing.T) {
	h := &gateway.Handler{}
	srv := httptest.NewServer(internalRoutesHandler(h, nil, nil))
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/v1/internal/apps/anything/routes")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
}

// TestInternalRoutesHandler_CapHitSetWhenOverflow is the
// PR-B1 regression test for the cap_hit flag (ADR-093 Tier B
// item #1). When the app's routeLabelSet has hit the cap and
// additional routes are collapsing into __route_other__, the
// loopback wire shape carries cap_hit=true so the apid
// reverse-proxy can surface it on the customer-facing
// AppRoutesResponse.
func TestInternalRoutesHandler_CapHitSetWhenOverflow(t *testing.T) {
	h := &gateway.Handler{}
	set := h.RouteSetForTest("app-cap")
	// Admit 60 distinct real routes so the routeLabelSet is
	// past the 50-cap. The pre-admitted __route_other__ bucket
	// will surface as the collapse target on overflow.
	for i := 0; i < 60; i++ {
		set.AdmitForTest("GET /probe/" + itoaRoutes(i))
	}
	lookup := gateway.ResolveSlugFn(func(slug string) (string, bool) {
		if slug == "cap-api" {
			return "app-cap", true
		}
		return "", false
	})
	srv := httptest.NewServer(internalRoutesHandler(h, lookup, nil))
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/v1/internal/apps/cap-api/routes")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var body routesResponseJSON
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !body.CapHit {
		t.Errorf("CapHit = false after 60 admits, want true (cap=50 hit)")
	}
}

// TestInternalRoutesHandler_CapHitFalseWhenUnderCap is the
// inverse pin — cap_hit must be false when the app is below
// cap, even if the pre-admitted __route_other__ bucket has been
// observed (the bucket is pre-admitted, so presence in the
// snapshot is not the overflow signal).
func TestInternalRoutesHandler_CapHitFalseWhenUnderCap(t *testing.T) {
	h := &gateway.Handler{}
	set := h.RouteSetForTest("app-1")
	set.AdmitForTest("GET /users")
	set.AdmitForTest("POST /orders")
	lookup := gateway.ResolveSlugFn(func(slug string) (string, bool) {
		if slug == "jane-api" {
			return "app-1", true
		}
		return "", false
	})
	srv := httptest.NewServer(internalRoutesHandler(h, lookup, nil))
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/v1/internal/apps/jane-api/routes")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var body routesResponseJSON
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.CapHit {
		t.Errorf("CapHit = true at 2 admits, want false (under cap=50)")
	}
}

// TestInternalRoutesHandler_MissingRoutesSuffix404 guards against
// the prefix-match trap: ServeMux mounts the handler at the prefix
// /v1/internal/apps/, so a request to /v1/internal/apps/foo (no
// /routes suffix) reaches this handler. Before the trim-validator
// it returned 200 with foo's routes; now it 404s so a typo can't
// silently leak another app's labels through the loopback surface.
func TestInternalRoutesHandler_MissingRoutesSuffix404(t *testing.T) {
	h := &gateway.Handler{}
	set := h.RouteSetForTest("app-1")
	set.AdmitForTest("GET /users")
	// Lookup returns the same app the prefix would have
	// addressed — proves the test exercises the "wrong but
	// would-have-answered" path, not the lookup miss path.
	lookup := gateway.ResolveSlugFn(func(slug string) (string, bool) {
		if slug == "foo" {
			return "app-1", true
		}
		return "", false
	})
	srv := httptest.NewServer(internalRoutesHandler(h, lookup, nil))
	defer srv.Close()

	// No /routes suffix → must NOT serve foo's routes.
	resp, err := http.Get(srv.URL + "/v1/internal/apps/foo")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (prefix without /routes must 404)", resp.StatusCode)
	}

	// Trailing junk after /routes → also 404.
	resp2, err := http.Get(srv.URL + "/v1/internal/apps/foo/routes/extra")
	if err != nil {
		t.Fatalf("GET extra: %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (extra path after /routes must 404)", resp2.StatusCode)
	}
}

// itoaRoutes is a tiny allocation-bounded int-to-string helper for
// the CapHit regression tests. Mirrors observe_route_test.go's
// itoaForTest but lives here to keep the routes-handler tests
// self-contained.
func itoaRoutes(n int) string {
	if n == 0 {
		return "0"
	}
	digits := "0123456789"
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = digits[n%10]
		n /= 10
	}
	return string(buf[i:])
}

func contains(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}
