// tenant_surfaces_routing_test.go — unit tests for the pgRouter
// tenant-surface branch (issue #879 / ADR-100 PR-B). The full
// routing-only e2e is deferred to PR-C because cmd/e2e currently
// has no gatewayd-internal boot harness; routing-layer coverage
// at the routing-layer unit level is the right granularity for
// PR-B and matches the existing TestPgRouter_* family in
// backend_test.go.
//
// These tests deliberately use the MemStore path so they run
// under `make test` (no Postgres required). The PgStore
// counterpart is in pkg/state/pgstore_tenant_surface_test.go.
package main

import (
	"context"
	"testing"

	"github.com/onebox-faas/faas/pkg/api"
	"github.com/onebox-faas/faas/pkg/state"
)

// seedSurface creates an account + app + tenant surface + a
// single verified hostname in the surface. Returns the app so
// tests can assert the routing outcome.
func seedSurface(t *testing.T, store state.Store, slug, hostname string, status state.SurfaceStatus) (app state.App) {
	t.Helper()
	ctx := context.Background()
	app = seedApp(t, store, slug, api.PlanPro)
	lim := api.Limits{
		TenantSurfacesPerAccount:  5,
		TenantHostnamesPerSurface: 10,
		TenantSurfacesAllowed:     true,
	}
	surf, err := store.CreateTenantSurfaceIfUnderQuota(ctx, state.CreateTenantSurfaceParams{
		AccountID: app.AccountID,
		AppID:     app.ID,
		Name:      "test-surface",
	}, lim)
	if err != nil {
		t.Fatalf("CreateTenantSurfaceIfUnderQuota: %v", err)
	}
	if _, err := store.CreateTenantHostnameIfUnderQuota(ctx, state.CreateTenantHostnameParams{
		SurfaceID:      surf.ID,
		Hostname:       hostname,
		ChallengeToken: "tok",
	}, lim); err != nil {
		t.Fatalf("CreateTenantHostnameIfUnderQuota: %v", err)
	}
	if status != state.SurfaceStatusPending {
		if err := store.UpdateTenantSurfaceStatus(ctx, surf.ID, status); err != nil {
			t.Fatalf("UpdateTenantSurfaceStatus: %v", err)
		}
	}
	return app
}

// TestPgRouter_TenantSurfaceRouted pins the happy path: feature
// flag on, well-formed customer-zone host with an active surface
// must route to the surface's app. This is the load-bearing
// behaviour PR-B ships.
func TestPgRouter_TenantSurfaceRouted(t *testing.T) {
	t.Setenv("FAAS_TENANT_SURFACES_ENABLED", "true")
	store := state.NewMemStore()
	app := seedSurface(t, store, "blog", "api.customer-a.com", state.SurfaceStatusActive)
	r := pgRouter{store: store, appsSuffix: ".apps.gregale.dev"}

	got, ok, err := r.ResolveHost(context.Background(), "api.customer-a.com")
	if err != nil || !ok {
		t.Fatalf("ResolveHost ok=%v err=%v, want true/nil", ok, err)
	}
	if got.ID != app.ID {
		t.Errorf("resolved = %+v, want id=%s", got, app.ID)
	}
}

// TestPgRouter_TenantSurfaceFlagOff confirms the dark-launch
// contract: with the flag off, the routing branch is a no-op
// and a surface-bearing host falls through to legacy (which
// 404s because there is no custom_domains row). The point is
// that the surface row is NOT consulted when the flag is off,
// so a stale surface schema does not affect production behaviour.
func TestPgRouter_TenantSurfaceFlagOff(t *testing.T) {
	t.Setenv("FAAS_TENANT_SURFACES_ENABLED", "false")
	store := state.NewMemStore()
	_ = seedSurface(t, store, "blog", "api.customer-a.com", state.SurfaceStatusActive)
	r := pgRouter{store: store, appsSuffix: ".apps.gregale.dev"}

	if _, ok, _ := r.ResolveHost(context.Background(), "api.customer-a.com"); ok {
		t.Fatal("surface routed while feature flag is off")
	}
}

// TestPgRouter_TenantSurfaceSuspendedFallsThrough: a surface
// exists, but the customer has suspended it (a customer-visible
// state). The routing branch must route-around so a legacy
// custom_domains row that pre-dates the surface remains the
// source of truth. This pins the soft-delete + suspend semantics
// from pkg/state/tenant_surface.go.
func TestPgRouter_TenantSurfaceSuspendedFallsThrough(t *testing.T) {
	t.Setenv("FAAS_TENANT_SURFACES_ENABLED", "true")
	store := state.NewMemStore()
	_ = seedSurface(t, store, "blog", "api.customer-a.com", state.SurfaceStatusSuspended)
	r := pgRouter{store: store, appsSuffix: ".apps.gregale.dev"}

	// No legacy domain row either → 404, NOT a route to the
	// suspended surface.
	if _, ok, _ := r.ResolveHost(context.Background(), "api.customer-a.com"); ok {
		t.Fatal("suspended surface was still routed")
	}
}

// TestPgRouter_TenantSurfaceUnknownHostFallsThrough: a
// well-formed customer-zone host with no surface row must
// fall through to the legacy custom_domains path. This is
// the dispatch arm correctness test — the branch in
// ResolveHost must NOT 404 prematurely when only the surface
// lookup misses.
func TestPgRouter_TenantSurfaceUnknownHostFallsThrough(t *testing.T) {
	t.Setenv("FAAS_TENANT_SURFACES_ENABLED", "true")
	store := state.NewMemStore()
	// App exists; no surface exists for api.unknown.com.
	_ = seedApp(t, store, "blog", api.PlanPro)
	r := pgRouter{store: store, appsSuffix: ".apps.gregale.dev"}

	if _, ok, _ := r.ResolveHost(context.Background(), "api.unknown.com"); ok {
		t.Fatal("unknown host routed")
	}
}

// TestPgRouter_TenantSurfaceNonSurfaceHostIgnored: a host that
// is not even well-formed as a surface host (e.g. "localhost"
// with no apex) must skip the surface branch entirely. The
// skip is by parser rejection, not by store lookup. This pins
// the parser→routing hand-off.
func TestPgRouter_TenantSurfaceNonSurfaceHostIgnored(t *testing.T) {
	t.Setenv("FAAS_TENANT_SURFACES_ENABLED", "true")
	store := state.NewMemStore()
	_ = seedApp(t, store, "blog", api.PlanPro)
	r := pgRouter{store: store, appsSuffix: ".apps.gregale.dev"}

	// "localhost" is a single label → SurfaceParse returns ok=false.
	// The routing branch is skipped; the legacy path is also a
	// miss (no custom_domains row). 404.
	if _, ok, _ := r.ResolveHost(context.Background(), "localhost"); ok {
		t.Fatal("non-surface host routed")
	}
}

// TestPgRouter_TenantSurfaceOrderingWithPreview: a PR-preview
// host (pr-{N}.slug.apps.zone) is shape-disjoint with a
// customer-zone host. The surface branch must not consume a
// preview host. PR #872's preview routing must continue to work
// after PR-B. This test pins the parser-rejects-multi-label-prefix
// behaviour for hosts that look like neither.
func TestPgRouter_TenantSurfaceOrderingWithPreview(t *testing.T) {
	t.Setenv("FAAS_TENANT_SURFACES_ENABLED", "true")
	store := state.NewMemStore()
	_ = seedApp(t, store, "blog", api.PlanPro)
	r := pgRouter{store: store, appsSuffix: ".apps.gregale.dev"}

	// "pr-42.blog.apps.gregale.dev" is preview-shaped. Without
	// a preview app row, the surface branch must skip (host is
	// not a surface candidate) and the preview branch must 404.
	// The assertion pins that the surface branch did not
	// intercept.
	if _, ok, _ := r.ResolveHost(context.Background(), "pr-42.blog.apps.gregale.dev"); ok {
		t.Fatal("surface branch consumed a preview host")
	}
}
