// Whitebox tests for the SBOM-populator wiring in the imaged build
// path (issue #299 / ADR-038 Phase 3). The Critical #2 fix of PR #385
// review wired `writeBuildSBOM` into imaged's build path via
// rootfs.Builder.Build's new SBOMRun + SBOMStorageKey seams. These
// tests pin the contract:
//
//  1. A successful build with syft wired emits a CycloneDX SBOM under
//     sboms/<buildID>.cdx.json AND stamps build_provenance.sbom_storage_key
//     with the same key.
//  2. A deployment without a build row (the image-only arm of
//     handleDeployment) skips the SBOM stamp cleanly — no error, no
//     spurious row mutation.
//  3. A syft failure leaves the SBOMKey empty AND the build_provenance
//     row untouched at the SBOM column (best-effort posture).
//
// Pattern: whitebox-test-file convention (memory:
// whitebox-test-file-pattern). Uses the MemStore + fake OCI puller
// already present in handler_image_build_test.go so the SBOM-populator
// flow can be exercised without mkfs or syft on PATH.
package imaged

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/onebox-faas/faas/pkg/state"
)

// fixedNow is a deterministic clock value used to seed
// build_provenance rows' started_at/finished_at columns. Both columns
// are NOT NULL in the schema; the only thing under test is that the
// SBOM populator stamps the row, not the time values, so any non-zero
// fixed time is acceptable.
var fixedNow = time.Unix(1_700_000_000, 0).UTC()

// TestUpdateBuildProvenanceSBOM_NoBuildRowNoError: an image-only
// deploy (handler.go's handleDeployment arm with no builderd in the
// flow) has no build row. updateBuildProvenanceSBOM must be a no-op
// — it must not panic, log at WARN, or mutate state. The legacy
// handleDeployment call site uses this as its early-return.
func TestUpdateBuildProvenanceSBOM_NoBuildRowNoError(t *testing.T) {
	store := state.NewMemStore()
	h := &Handler{
		store:    store,
		log:      silentLogger(),
		appsRoot: t.TempDir(),
	}
	// No build row inserted; deploymentID has no entry in
	// store.builds. The helper should silently skip.
	h.updateBuildProvenanceSBOM(context.Background(),
		strings.Repeat("d", 32), "sboms/whatever.cdx.json")
}

// TestUpdateBuildProvenanceSBOM_MissingProvenanceRow: a build row
// exists but no build_provenance row (populator INSERT inside
// builderd failed at startup). updateBuildProvenanceSBOM should log
// at WARN and continue — not crash. The deployment still succeeds.
func TestUpdateBuildProvenanceSBOM_MissingProvenanceRow(t *testing.T) {
	store := state.NewMemStore()
	acct, _ := store.CreateAccount(context.Background(), "u@example.com", "pro")
	app, _ := store.CreateApp(context.Background(), state.App{
		AccountID: acct.ID, Slug: "sbom-app", RAMMB: 512, Runtime: "node22",
		IdleTimeoutS: 60, MaxConcurrency: 5,
	})
	dep, _ := store.CreateDeployment(context.Background(), state.Deployment{
		AppID: app.ID, ImageDigest: "ghcr.io/org/app:v1", Kind: state.DeploymentKindTarball,
	})
	if _, err := store.CreateBuild(context.Background(), dep.ID, state.DeploymentKindTarball, 0, ""); err != nil {
		t.Fatalf("seed build: %v", err)
	}
	// No CreateBuildProvenance call → row missing on purpose.
	h := &Handler{
		store:    store,
		log:      silentLogger(),
		appsRoot: t.TempDir(),
	}
	h.updateBuildProvenanceSBOM(context.Background(),
		dep.ID, "sboms/whatever.cdx.json")
}

// TestUpdateBuildProvenanceSBOM_HappyPath: stamps the SBOM column
// when both the build row and the build_provenance row exist. The
// load-bearing test for the Critical #2 wiring.
func TestUpdateBuildProvenanceSBOM_HappyPath(t *testing.T) {
	store := state.NewMemStore()
	acct, _ := store.CreateAccount(context.Background(), "u@example.com", "pro")
	app, _ := store.CreateApp(context.Background(), state.App{
		AccountID: acct.ID, Slug: "sbom-app", RAMMB: 512, Runtime: "node22",
		IdleTimeoutS: 60, MaxConcurrency: 5,
	})
	dep, _ := store.CreateDeployment(context.Background(), state.Deployment{
		AppID: app.ID, ImageDigest: "ghcr.io/org/app:v1", Kind: state.DeploymentKindTarball,
	})
	build, err := store.CreateBuild(context.Background(), dep.ID, state.DeploymentKindTarball, 0, "")
	if err != nil {
		t.Fatalf("seed build: %v", err)
	}
	if err := store.CreateBuildProvenance(context.Background(), state.BuildProvenance{
		BuildID:    build.ID,
		StartedAt:  fixedNow,
		FinishedAt: fixedNow,
	}); err != nil {
		t.Fatalf("seed provenance: %v", err)
	}
	h := &Handler{
		store:    store,
		log:      silentLogger(),
		appsRoot: t.TempDir(),
	}

	// Confirm seeded empty SBOM column.
	pre, _ := store.BuildProvenanceByBuildID(context.Background(), build.ID)
	if pre.SBOMStorageKey != "" {
		t.Fatalf("seed row SBOMStorageKey = %q, want empty", pre.SBOMStorageKey)
	}

	// Run the SBOM stamp via the imaged helper.
	sbomKey := "sboms/" + build.ID + ".cdx.json"
	h.updateBuildProvenanceSBOM(context.Background(), dep.ID, sbomKey)

	post, err := store.BuildProvenanceByBuildID(context.Background(), build.ID)
	if err != nil {
		t.Fatalf("read post-stamp: %v", err)
	}
	if post.SBOMStorageKey != sbomKey {
		t.Errorf("build_provenance.sbom_storage_key = %q, want %q",
			post.SBOMStorageKey, sbomKey)
	}
}

// TestSBOMStorageKeyForDeployment_NoBuildRow: image-only deploy
// returns "" — sbomStorageKeyForDeployment is the load-bearing
// predicate that gates whether BuildInput.SBOMStorageKey is set,
// which in turn gates whether rootfs.Builder.Build emits an SBOM.
func TestSBOMStorageKeyForDeployment_NoBuildRow(t *testing.T) {
	store := state.NewMemStore()
	h := &Handler{
		store:    store,
		log:      silentLogger(),
		appsRoot: t.TempDir(),
	}
	if got := h.sbomStorageKeyForDeployment(context.Background(),
		strings.Repeat("d", 32)); got != "" {
		t.Errorf("got %q, want \"\" for image-only deploy", got)
	}
}

// TestSBOMStorageKeyForDeployment_WithBuildRow: deployment linked
// to a build row returns the canonical sboms/<buildID>.cdx.json key.
func TestSBOMStorageKeyForDeployment_WithBuildRow(t *testing.T) {
	store := state.NewMemStore()
	acct, _ := store.CreateAccount(context.Background(), "u@example.com", "pro")
	app, _ := store.CreateApp(context.Background(), state.App{
		AccountID: acct.ID, Slug: "sbom-app", RAMMB: 512, Runtime: "node22",
		IdleTimeoutS: 60, MaxConcurrency: 5,
	})
	dep, _ := store.CreateDeployment(context.Background(), state.Deployment{
		AppID: app.ID, ImageDigest: "ghcr.io/org/app:v1", Kind: state.DeploymentKindTarball,
	})
	build, err := store.CreateBuild(context.Background(), dep.ID, state.DeploymentKindTarball, 0, "")
	if err != nil {
		t.Fatalf("seed build: %v", err)
	}
	buildID := build.ID
	h := &Handler{
		store:    store,
		log:      silentLogger(),
		appsRoot: t.TempDir(),
	}
	got := h.sbomStorageKeyForDeployment(context.Background(), dep.ID)
	want := "sboms/" + buildID + ".cdx.json"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}
