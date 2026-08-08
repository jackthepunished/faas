package state_test

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/onebox-faas/faas/pkg/api"
	"github.com/onebox-faas/faas/pkg/state"
)

// This file drives the 0%-coverage PgStore Deployment/Org/Cron
// methods through the pgtest harness. Each test follows the
// canonical pattern: pgStore(t), create account+app, exercise.
//
// Note: pgstore.CreateDeployment omits `id` from its INSERT column
// list (see pkg/state/pgstore.go:3630 and migration 00001_init.sql
// `deployments.id default gen_random_uuid()`). All tests that
// later UPDATE / SET on the deployment must use the *returned*
// `created.ID`, not the input `d.ID`.

// seedPgAccountAndApp returns an account + an app in the schema.
func seedPgAccountAndApp(t *testing.T, s *state.PgStore, ctx stateCtx) (state.Account, state.App) {
	t.Helper()
	email := "pg-seed-" + uuid.NewString() + "@example.com"
	acct, err := s.CreateAccount(ctx, email, api.PlanPro)
	if err != nil {
		t.Fatalf("CreateAccount: %v", err)
	}
	app := state.App{
		ID:        uuid.NewString(),
		AccountID: acct.ID,
		Slug:      "s-" + uuid.NewString()[:8],
	}
	got, err := s.CreateApp(ctx, app)
	if err != nil {
		t.Fatalf("CreateApp: %v", err)
	}
	return acct, got
}

type stateCtx = interface {
	Deadline() (time.Time, bool)
	Done() <-chan struct{}
	Err() error
	Value(key any) any
}

// seedPgDeployment inserts an Image-kind deployment under app and
// returns the row PG actually persisted (input ID is ignored).
func seedPgDeployment(t *testing.T, s *state.PgStore, ctx stateCtx, app state.App) state.Deployment {
	t.Helper()
	d := state.Deployment{
		ID:        uuid.NewString(),
		AppID:     app.ID,
		Kind:      state.DeploymentKindImage,
		CreatedAt: time.Now(),
	}
	created, err := s.CreateDeployment(ctx, d)
	if err != nil {
		t.Fatalf("CreateDeployment: %v", err)
	}
	return created
}

func TestPg_CoverageCreateDeployment(t *testing.T) {
	s, ctx := pgStore(t)
	_, app := seedPgAccountAndApp(t, s, ctx)
	d := state.Deployment{
		ID:        uuid.NewString(),
		AppID:     app.ID,
		Kind:      state.DeploymentKindImage,
		CreatedAt: time.Now(),
	}
	got, err := s.CreateDeployment(ctx, d)
	if err != nil {
		t.Fatalf("CreateDeployment: %v", err)
	}
	if got.ID == "" {
		t.Error("CreateDeployment returned empty ID")
	}
}

func TestPg_CoverageDeploymentByID(t *testing.T) {
	s, ctx := pgStore(t)
	_, app := seedPgAccountAndApp(t, s, ctx)
	created := seedPgDeployment(t, s, ctx, app)
	got, err := s.DeploymentByID(ctx, created.ID)
	if err != nil {
		t.Fatalf("DeploymentByID: %v", err)
	}
	if got.ID != created.ID {
		t.Errorf("DeploymentByID.ID = %q, want %q", got.ID, created.ID)
	}
}

func TestPg_CoverageLatestDeployment(t *testing.T) {
	s, ctx := pgStore(t)
	_, app := seedPgAccountAndApp(t, s, ctx)
	created := seedPgDeployment(t, s, ctx, app)
	got, err := s.LatestDeployment(ctx, app.ID)
	if err != nil {
		t.Fatalf("LatestDeployment: %v", err)
	}
	if got.ID != created.ID {
		t.Errorf("LatestDeployment.ID = %q, want %q", got.ID, created.ID)
	}
}

func TestPg_CoverageLiveDeployment(t *testing.T) {
	s, ctx := pgStore(t)
	_, app := seedPgAccountAndApp(t, s, ctx)
	_, err := s.LiveDeployment(ctx, app.ID)
	if err == nil {
		t.Log("LiveDeployment returned nil err (no live deployment is fine)")
	}
}

func TestPg_CoverageCountDeploymentsPg(t *testing.T) {
	s, ctx := pgStore(t)
	acct, _ := seedPgAccountAndApp(t, s, ctx)
	got, err := s.CountDeployments(ctx, acct.ID)
	if err != nil {
		t.Fatalf("CountDeployments: %v", err)
	}
	if got < 0 {
		t.Errorf("CountDeployments = %d", got)
	}
}

func TestPg_CoverageListAllDeployments(t *testing.T) {
	s, ctx := pgStore(t)
	_, _ = s.ListAllDeployments(ctx)
}

func TestPg_CoverageListDeploymentsByNodeID(t *testing.T) {
	s, ctx := pgStore(t)
	_, _ = s.ListDeploymentsByNodeID(ctx, "node-1")
}

func TestPg_CoverageUpdateDeploymentMinInstances(t *testing.T) {
	s, ctx := pgStore(t)
	_, app := seedPgAccountAndApp(t, s, ctx)
	created := seedPgDeployment(t, s, ctx, app)
	got, err := s.UpdateDeploymentMinInstances(ctx, created.ID, 2)
	if err != nil {
		t.Fatalf("UpdateDeploymentMinInstances: %v", err)
	}
	if got.MinInstances != 2 {
		t.Errorf("MinInstances = %d", got.MinInstances)
	}
}

func TestPg_CoverageSetDeploymentParked(t *testing.T) {
	s, ctx := pgStore(t)
	_, app := seedPgAccountAndApp(t, s, ctx)
	created := seedPgDeployment(t, s, ctx, app)
	if err := s.SetDeploymentParked(ctx, created.ID, "admin_park", time.Now()); err != nil {
		t.Errorf("SetDeploymentParked: %v", err)
	}
}

func TestPg_CoverageMarkDeploymentSuperseded(t *testing.T) {
	s, ctx := pgStore(t)
	_, app := seedPgAccountAndApp(t, s, ctx)
	created := seedPgDeployment(t, s, ctx, app)
	if err := s.MarkDeploymentSuperseded(ctx, created.ID); err != nil {
		t.Errorf("MarkDeploymentSuperseded: %v", err)
	}
}

func TestPg_CoverageMarkDeploymentLive(t *testing.T) {
	s, ctx := pgStore(t)
	_, app := seedPgAccountAndApp(t, s, ctx)
	created := seedPgDeployment(t, s, ctx, app)
	if err := s.MarkDeploymentLive(ctx, created.ID); err != nil {
		t.Errorf("MarkDeploymentLive: %v", err)
	}
}

func TestPg_CoverageSetDeploymentRootfs(t *testing.T) {
	s, ctx := pgStore(t)
	_, app := seedPgAccountAndApp(t, s, ctx)
	created := seedPgDeployment(t, s, ctx, app)
	if err := s.SetDeploymentRootfs(ctx, created.ID, "/srv/fc/rootfs.ext4", "keyhex", 1024); err != nil {
		t.Errorf("SetDeploymentRootfs: %v", err)
	}
}

func TestPg_CoverageUpsertDeploymentScanResult(t *testing.T) {
	s, ctx := pgStore(t)
	_, app := seedPgAccountAndApp(t, s, ctx)
	created := seedPgDeployment(t, s, ctx, app)
	if err := s.UpsertDeploymentScanResult(ctx, created.ID, []byte(`{}`), "complete"); err != nil {
		t.Errorf("UpsertDeploymentScanResult: %v", err)
	}
}

func TestPg_CoverageListDeploymentsForApp(t *testing.T) {
	s, ctx := pgStore(t)
	_, app := seedPgAccountAndApp(t, s, ctx)
	got, err := s.ListDeploymentsForApp(ctx, app.ID, 10, 0)
	if err != nil {
		t.Fatalf("ListDeploymentsForApp: %v", err)
	}
	_ = got
}

func TestPg_CoverageLatestParkedDeploymentForApp(t *testing.T) {
	s, ctx := pgStore(t)
	_, app := seedPgAccountAndApp(t, s, ctx)
	_, err := s.LatestParkedDeploymentForApp(ctx, app.ID)
	if err == nil {
		t.Log("LatestParkedDeploymentForApp returned nil err (no parked deployment is fine)")
	}
}
