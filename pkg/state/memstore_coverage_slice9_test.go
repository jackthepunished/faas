package state

import (
	"errors"
	"testing"
	"time"
)

// This slice closes the remaining real-branch gaps in MemStore that the
// earlier slices left at <100%: build-claim FIFO + terminal CAS, account
// provider-ID remapping, scan-source downgrade, and the empty/missing
// paths of the list helpers.

func TestMemStoreCoverageClaimNextQueuedBuild(t *testing.T) {
	m, ctx, _, _, deployment := memCoverageSlice4Fixture(t)
	// The slice4 fixture already has one queued build on the deployment;
	// add two more with deterministic, distinct EnqueuedAt values so
	// FIFO is observable (CreateBuild stamps EnqueuedAt=now, so we set
	// them directly on the internal map to avoid timestamp ties).
	queued1, err := m.CreateBuild(ctx, deployment.ID, DeploymentKindImage, 10, "/tmp/q1.log")
	if err != nil {
		t.Fatal(err)
	}
	queued2, err := m.CreateBuild(ctx, deployment.ID, DeploymentKindImage, 10, "/tmp/q2.log")
	if err != nil {
		t.Fatal(err)
	}
	// A running build must not be picked.
	running, err := m.CreateBuild(ctx, deployment.ID, DeploymentKindImage, 10, "/tmp/q3.log")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.ClaimQueuedBuild(ctx, running.ID); err != nil {
		t.Fatal(err)
	}
	// Backdate the two queued rows so the order is deterministic.
	base := time.Now().Add(-time.Hour)
	m.mu.Lock()
	b1 := m.builds[queued1.ID]
	b1.EnqueuedAt = base
	m.builds[queued1.ID] = b1
	b2 := m.builds[queued2.ID]
	b2.EnqueuedAt = base.Add(time.Minute)
	m.builds[queued2.ID] = b2
	m.mu.Unlock()

	// FIFO: the earliest EnqueuedAt wins. queued1 (base) before queued2
	// (base+1m) before the fixture build (now).
	picked, err := m.ClaimNextQueuedBuild(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if picked.ID != queued1.ID || picked.Status != BuildRunning {
		t.Fatalf("picked = %s/%s, want %s/running", picked.ID, picked.Status, queued1.ID)
	}
	second, err := m.ClaimNextQueuedBuild(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if second.ID != queued2.ID || second.Status != BuildRunning {
		t.Fatalf("second = %s/%s, want %s/running", second.ID, second.Status, queued2.ID)
	}
	// Queue still has the fixture build (created first, EnqueuedAt=now
	// at fixture time — strictly older than nothing, so it's next).
	if _, err := m.ClaimNextQueuedBuild(ctx); err != nil {
		t.Fatalf("fixture build claim = %v", err)
	}
	// Queue drained → ErrNotFound.
	if _, err := m.ClaimNextQueuedBuild(ctx); !errors.Is(err, ErrNotFound) {
		t.Fatalf("drained = %v", err)
	}
}

func TestMemStoreCoverageUpdateBuildStatusCAS(t *testing.T) {
	m, ctx, _, _, deployment := memCoverageSlice4Fixture(t)
	build, err := m.CreateBuild(ctx, deployment.ID, DeploymentKindImage, 10, "/tmp/cas.log")
	if err != nil {
		t.Fatal(err)
	}
	// Terminal transition from queued (not running) → ErrNotFound.
	if err := m.UpdateBuildStatus(ctx, build.ID, BuildSucceeded, "", false, true); !errors.Is(err, ErrNotFound) {
		t.Fatalf("terminal from queued = %v", err)
	}
	// Flip to running, then succeed with failure class + timestamps.
	if _, err := m.ClaimQueuedBuild(ctx, build.ID); err != nil {
		t.Fatal(err)
	}
	if err := m.UpdateBuildStatus(ctx, build.ID, BuildFailed, FailureTimeout, false, true); err != nil {
		t.Fatal(err)
	}
	got, err := m.BuildByID(ctx, build.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != BuildFailed || got.FailureClass != FailureTimeout || got.FinishedAt.IsZero() {
		t.Fatalf("failed build = %+v", got)
	}
	// Missing build → ErrNotFound.
	if err := m.UpdateBuildStatus(ctx, "missing", BuildSucceeded, "", false, true); !errors.Is(err, ErrNotFound) {
		t.Fatalf("update missing = %v", err)
	}
}

func TestMemStoreCoverageDomainsAndKeys(t *testing.T) {
	m, ctx, account, app, _ := memCoverageSlice4Fixture(t)
	// ListDomainsForApp populated + empty.
	dom, err := m.CreateCustomDomain(ctx, "app.example.com", app.ID, "tok")
	if err != nil {
		t.Fatal(err)
	}
	_ = dom
	if got, err := m.ListDomainsForApp(ctx, app.ID); err != nil || len(got) != 1 {
		t.Fatalf("domains for app = %+v, %v", got, err)
	}
	if got, err := m.ListDomainsForApp(ctx, "missing"); err != nil || len(got) != 0 {
		t.Fatalf("domains missing = %+v, %v", got, err)
	}
	// API key: ListAPIKeys + TouchKeyLastUsed (hit/miss).
	hash := []byte("slice9-key-hash")
	key, err := m.CreateAPIKey(ctx, account.ID, hash, "slice9", []string{"read"})
	if err != nil {
		t.Fatal(err)
	}
	if got, err := m.ListAPIKeys(ctx, account.ID); err != nil || len(got) != 1 || got[0].ID != key.ID {
		t.Fatalf("list keys = %+v, %v", got, err)
	}
	if got, err := m.ListAPIKeys(ctx, "missing"); err != nil || len(got) != 0 {
		t.Fatalf("list keys missing = %+v, %v", got, err)
	}
	if err := m.TouchKeyLastUsed(ctx, key.ID); err != nil {
		t.Fatal(err)
	}
	if err := m.TouchKeyLastUsed(ctx, "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("touch missing = %v", err)
	}
}

func TestMemStoreCoverageAccountProviderRemap(t *testing.T) {
	m, ctx, account, _, _ := memCoverageSlice4Fixture(t)
	if err := m.UpdateAccountProviderCustomerID(ctx, account.ID, "cus_1"); err != nil {
		t.Fatal(err)
	}
	if got, err := m.AccountByProviderCustomerID(ctx, "cus_1"); err != nil || got.ID != account.ID {
		t.Fatalf("by provider = %+v, %v", got, err)
	}
	// Re-map to a new customer id — the old index entry must go.
	if err := m.UpdateAccountProviderCustomerID(ctx, account.ID, "cus_2"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.AccountByProviderCustomerID(ctx, "cus_1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("old provider still resolvable = %v", err)
	}
	if got, err := m.AccountByProviderCustomerID(ctx, "cus_2"); err != nil || got.ID != account.ID {
		t.Fatalf("new provider = %+v, %v", got, err)
	}
	// Missing account / missing provider.
	if err := m.UpdateAccountProviderCustomerID(ctx, "missing", "cus_3"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("update missing = %v", err)
	}
	if _, err := m.AccountByProviderCustomerID(ctx, "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("by provider missing = %v", err)
	}
	// UpdateAccountStatus (hit/miss).
	if err := m.UpdateAccountStatus(ctx, account.ID, AccountSuspended); err != nil {
		t.Fatal(err)
	}
	acct, _ := m.AccountByID(ctx, account.ID)
	if acct.Status != AccountSuspended {
		t.Fatalf("status = %s", acct.Status)
	}
	if err := m.UpdateAccountStatus(ctx, "missing", AccountActive); !errors.Is(err, ErrNotFound) {
		t.Fatalf("status missing = %v", err)
	}
	// AuthenticateKey — missing hash.
	if _, _, err := m.AuthenticateKey(ctx, []byte("nope")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("authenticate missing = %v", err)
	}
	// AccountByID — missing.
	if _, err := m.AccountByID(ctx, "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("account missing = %v", err)
	}
}

func TestMemStoreCoverageSetProjectScanSourceDowngrade(t *testing.T) {
	m, ctx, account, _, _ := memCoverageSlice4Fixture(t)
	proj, err := m.CreateProject(ctx, Project{AccountID: account.ID, Slug: "scan-proj", ScanSource: ProjectScanSourceConvention})
	if err != nil {
		t.Fatal(err)
	}
	// Upgrade → allowed.
	if got, err := m.SetProjectScanSource(ctx, proj.ID, ProjectScanSourceCompose); err != nil || got.ScanSource != ProjectScanSourceCompose {
		t.Fatalf("upgrade = %+v, %v", got, err)
	}
	// Downgrade → ErrScanSourceDowngrade.
	if _, err := m.SetProjectScanSource(ctx, proj.ID, ProjectScanSourceSingle); !errors.Is(err, ErrScanSourceDowngrade) {
		t.Fatalf("downgrade = %v", err)
	}
	// Same tier → no-op.
	if got, err := m.SetProjectScanSource(ctx, proj.ID, ProjectScanSourceCompose); err != nil || got.ScanSource != ProjectScanSourceCompose {
		t.Fatalf("same tier = %+v, %v", got, err)
	}
	// Missing project.
	if _, err := m.SetProjectScanSource(ctx, "missing", ProjectScanSourceSingle); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing project = %v", err)
	}
}

func TestMemStoreCoverageDeleteProjectWithRepo(t *testing.T) {
	m, ctx, account, _, _ := memCoverageSlice4Fixture(t)
	proj, err := m.CreateProject(ctx, Project{AccountID: account.ID, Slug: "repo-proj", InstallID: 55, RepoFullName: "acme/repo"})
	if err != nil {
		t.Fatal(err)
	}
	// DeleteProject must drop the by-(install, repo) index.
	if err := m.DeleteProject(ctx, proj.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := m.ProjectByRepo(ctx, "", 55, "acme/repo"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("repo lookup after delete = %v", err)
	}
	// A standalone project (no install) also deletes cleanly.
	standalone, err := m.CreateProject(ctx, Project{AccountID: account.ID, Slug: "standalone-proj"})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.DeleteProject(ctx, standalone.ID); err != nil {
		t.Fatal(err)
	}
}

func TestMemStoreCoverageListUnplacedAndAllApps(t *testing.T) {
	m, ctx, _, app, _ := memCoverageSlice4Fixture(t)
	// The fixture app has no node owner → unplaced.
	if got, err := m.ListUnplacedApps(ctx); err != nil || len(got) != 1 || got[0].ID != app.ID {
		t.Fatalf("unplaced = %+v, %v", got, err)
	}
	// Claim it → no longer unplaced.
	if err := m.SetAppNodeID(ctx, app.ID, DefaultLocalNodeName); err != nil {
		t.Fatal(err)
	}
	if got, err := m.ListUnplacedApps(ctx); err != nil || len(got) != 0 {
		t.Fatalf("unplaced after claim = %+v, %v", got, err)
	}
	// ListAllApps includes deleted apps? It excludes them — soft-delete
	// and confirm the count drops.
	if got, err := m.ListAllApps(ctx); err != nil || len(got) != 1 {
		t.Fatalf("all apps = %+v, %v", got, err)
	}
	if _, err := m.SoftDeleteAppCascade(ctx, app.ID); err != nil {
		t.Fatal(err)
	}
	if got, err := m.ListAllApps(ctx); err != nil || len(got) != 0 {
		t.Fatalf("all apps after delete = %+v, %v", got, err)
	}
	// ListAppsByNodeID — after soft-delete the app is excluded; assert
	// empty.
	if got, err := m.ListAppsByNodeID(ctx, DefaultLocalNodeName); err != nil || len(got) != 0 {
		t.Fatalf("apps by node = %+v, %v", got, err)
	}
}
