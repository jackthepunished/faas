//go:build !no_pg

// pgstore_sidecar_layers_test.go — round-trip tests for the
// per-workload filesystem handle table added by migration 00119
// (issue #463 / ADR-069 / PR-B).
//
// Build tag: !no_pg matches the rest of the pgstore-side tests; set
// FAAS_SKIP_PG_TESTS=1 to opt out locally without rebuilding.
package state_test

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/onebox-faas/faas/pkg/api"
	"github.com/onebox-faas/faas/pkg/state"
)

// pgSidecarLayersFixture creates an account + app + deployment to
// exercise the deployment_sidecar_layers methods against a real
// Postgres schema. Mirrors pgSidecarsFixture's shape but adds the
// deployment up front (the Layer methods FK-target a deployment
// row, unlike the sidecars jsonb column which lives on the
// deployment itself).
func pgSidecarLayersFixture(t *testing.T) (*state.PgStore, context.Context, state.App, state.Deployment) {
	t.Helper()
	s, ctx, app := pgSidecarsFixtureAccountApp(t)
	dep, err := s.CreateDeployment(ctx, state.Deployment{
		AppID:       app.ID,
		Kind:        state.DeploymentKindImage,
		ImageDigest: "sha256:sidecar-layer-fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	return s, ctx, app, dep
}

// pgSidecarsFixtureAccountApp is the (account, app) tuple behind
// pgSidecarsFixture, exported to this file so the layer fixture
// can reuse it without round-tripping the full sidecars fixture.
func pgSidecarsFixtureAccountApp(t *testing.T) (*state.PgStore, context.Context, state.App) {
	t.Helper()
	s, pool, ctx := pgStoreWithPool(t)
	account, err := s.CreateAccount(ctx, "pg-sidecar-layers-"+uuid.NewString()+"@example.com", api.PlanPro)
	if err != nil {
		t.Fatal(err)
	}
	app, err := s.CreateApp(ctx, state.App{
		AccountID: account.ID, Slug: "pg-sidecar-layers-" + uuid.NewString(),
		Type: state.AppTypeApp, RAMMB: 512, MaxConcurrency: 5, IdleTimeoutS: 60,
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = pool // keep the pool alive for the test's lifetime via t.Cleanup in pgStoreWithPool
	return s, ctx, app
}

// TestPgStore_DeploymentSidecarLayer_UpsertRoundTrip pins the
// upsert contract: writing one row, reading it back, then
// re-writing the same (deployment_id, sidecar_name) pair with
// different content_digest + bytes values. The read must surface
// the UPDATED row, not the original — this is the imaged rebuild
// path that PR-B relies on for re-deploys.
func TestPgStore_DeploymentSidecarLayer_UpsertRoundTrip(t *testing.T) {
	s, ctx, _, dep := pgSidecarLayersFixture(t)

	first := state.DeploymentSidecarLayer{
		DeploymentID:  dep.ID,
		SidecarName:   "migrator",
		StorageKey:    "apps/migrator.ext4",
		Bytes:         1024,
		ContentDigest: "sha256:0000000000000000000000000000000000000000000000000000000000000001",
	}
	got, err := s.SetDeploymentSidecarLayer(ctx, first)
	if err != nil {
		t.Fatalf("first upsert: %v", err)
	}
	if got.StorageKey != first.StorageKey || got.Bytes != 1024 {
		t.Errorf("first upsert round-trip: got %+v", got)
	}
	if got.CreatedAt.IsZero() || got.UpdatedAt.IsZero() {
		t.Errorf("CreatedAt/UpdatedAt zero on initial insert: %+v", got)
	}

	// Second write with the same PK, different content. The
	// contract is "UPSERT overwrites" — the key + bytes + digest
	// change, and updated_at advances.
	second := state.DeploymentSidecarLayer{
		DeploymentID:  dep.ID,
		SidecarName:   "migrator",
		StorageKey:    "apps/migrator-v2.ext4",
		Bytes:         2048,
		ContentDigest: "sha256:0000000000000000000000000000000000000000000000000000000000000002",
	}
	got2, err := s.SetDeploymentSidecarLayer(ctx, second)
	if err != nil {
		t.Fatalf("second upsert: %v", err)
	}
	if got2.StorageKey != second.StorageKey || got2.Bytes != 2048 {
		t.Errorf("second upsert round-trip: got %+v", got2)
	}
	if !got2.UpdatedAt.After(got.UpdatedAt) && !got2.UpdatedAt.Equal(got.UpdatedAt) {
		t.Errorf("UpdatedAt did not advance on conflict update: first=%v second=%v", got.UpdatedAt, got2.UpdatedAt)
	}
	// CreatedAt is preserved across the conflict (the schema
	// stamps it on the initial INSERT only).
	if !got2.CreatedAt.Equal(got.CreatedAt) {
		t.Errorf("CreatedAt drifted on update: first=%v second=%v", got.CreatedAt, got2.CreatedAt)
	}
}

// TestPgStore_DeploymentSidecarLayer_ListOrder verifies the
// deterministic by-sidecar_name ordering the Wake path depends
// on (snapshot hashing must produce stable drive sets).
func TestPgStore_DeploymentSidecarLayer_ListOrder(t *testing.T) {
	s, ctx, _, dep := pgSidecarLayersFixture(t)

	want := []state.DeploymentSidecarLayer{
		{DeploymentID: dep.ID, SidecarName: "scraper", StorageKey: "apps/s.ext4", Bytes: 256,
			ContentDigest: "sha256:0000000000000000000000000000000000000000000000000000000000000011"},
		{DeploymentID: dep.ID, SidecarName: "migrator", StorageKey: "apps/m.ext4", Bytes: 512,
			ContentDigest: "sha256:0000000000000000000000000000000000000000000000000000000000000012"},
	}
	for _, l := range want {
		if _, err := s.SetDeploymentSidecarLayer(ctx, l); err != nil {
			t.Fatalf("seed %q: %v", l.SidecarName, err)
		}
	}

	// Insert in NON-alphabetical order to prove the SELECT
	// orders, not the insertion order.
	out, err := s.ListDeploymentSidecarLayers(ctx, dep.ID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("list returned %d rows, want 2", len(out))
	}
	if out[0].SidecarName != "migrator" || out[1].SidecarName != "scraper" {
		t.Errorf("ordering: got [%q, %q]; want [migrator, scraper]", out[0].SidecarName, out[1].SidecarName)
	}
}

// TestPgStore_DeploymentSidecarLayer_EmptyReturnsEmptySlice pins
// the "no sidecars returns [] not nil" contract so callers can
// range over the result without a nil check.
func TestPgStore_DeploymentSidecarLayer_EmptyReturnsEmptySlice(t *testing.T) {
	s, ctx, _, dep := pgSidecarLayersFixture(t)
	out, err := s.ListDeploymentSidecarLayers(ctx, dep.ID)
	if err != nil {
		t.Fatalf("list empty: %v", err)
	}
	if out == nil {
		t.Errorf("empty list returned nil; want empty slice")
	}
	if len(out) != 0 {
		t.Errorf("empty list returned %d rows; want 0", len(out))
	}
}

// TestPgStore_DeploymentSidecarLayer_NotFoundOnMissingDeployment
// pins the ErrNotFound-on-missing-parent path. imaged's rebuild
// loop should fail closed (not 23503 FK violation) when the
// deployment was deleted between build cycles.
func TestPgStore_DeploymentSidecarLayer_NotFoundOnMissingDeployment(t *testing.T) {
	s, ctx, _, _ := pgSidecarLayersFixture(t)
	_, err := s.SetDeploymentSidecarLayer(ctx, state.DeploymentSidecarLayer{
		DeploymentID:  "00000000-0000-0000-0000-deadbeef0000",
		SidecarName:   "orphan",
		StorageKey:    "apps/orphan.ext4",
		Bytes:         128,
		ContentDigest: "sha256:0000000000000000000000000000000000000000000000000000000000000fff",
	})
	if err != state.ErrNotFound {
		t.Errorf("missing deployment: got %v; want state.ErrNotFound", err)
	}
}
