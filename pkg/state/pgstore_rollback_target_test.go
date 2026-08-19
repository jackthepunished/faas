package state_test

// Unit tests for GetDeploymentByIDScopedToSuperseded (SAFE-RELEASES-G,
// issue #976, PR-G). Covers the three return paths: success (row found
// + status='superseded'), ErrNoRollbackTarget (row missing or belongs
// to a different app), and ErrRollbackTargetAlreadyLive (row exists
// but status != 'superseded'). Also covers empty-input validation.
//
// Lives outside the consolidated coverage sweep because the contract
// here is the SAFE-RELEASES-G surface that the rollback handler will
// branch on; we want a dedicated regression test rather than just a
// coverage line. Mirrors the unit-test pattern at
// pgstore_coverage_sweep_test.go:TestPg_CoverageSweepDeployments but
// stands alone so a future PR that splits the deployment sweep doesn't
// accidentally delete this assertion.

import (
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/onebox-faas/faas/pkg/state"
)

// TestPg_GetDeploymentByIDScopedToSuperseded exercises all three return
// paths of PgStore.GetDeploymentByIDScopedToSuperseded against a real
// Postgres schema (the pgStore harness migrates up).
func TestPg_GetDeploymentByIDScopedToSuperseded(t *testing.T) {
	s, ctx := pgStore(t)

	t.Run("EmptyArgsRejected", func(t *testing.T) {
		if _, err := s.GetDeploymentByIDScopedToSuperseded(ctx, "", "x"); err == nil {
			t.Error("empty appID: expected error, got nil")
		}
		if _, err := s.GetDeploymentByIDScopedToSuperseded(ctx, "x", ""); err == nil {
			t.Error("empty deploymentID: expected error, got nil")
		}
	})

	t.Run("MissingRowReturnsErrNoRollbackTarget", func(t *testing.T) {
		_, app := seedPgAccountAndApp(t, s, ctx)
		// Random UUID that does not match any row.
		_, err := s.GetDeploymentByIDScopedToSuperseded(ctx, app.ID, uuid.NewString())
		if !errors.Is(err, state.ErrNoRollbackTarget) {
			t.Fatalf("missing row: got %v, want ErrNoRollbackTarget", err)
		}
	})

	t.Run("WrongAppReturnsErrNoRollbackTarget", func(t *testing.T) {
		_, app1 := seedPgAccountAndApp(t, s, ctx)
		_, app2 := seedPgAccountAndApp(t, s, ctx)
		// Create deployment under app1, then ask for it via app2 — must
		// be rejected (defense-in-depth: caller can't rollback to a
		// deployment that belongs to a different app even if it knows
		// the deployment id).
		d := seedPgDeployment(t, s, ctx, app1)
		if err := s.MarkDeploymentSuperseded(ctx, d.ID); err != nil {
			t.Fatalf("MarkDeploymentSuperseded: %v", err)
		}
		_, err := s.GetDeploymentByIDScopedToSuperseded(ctx, app2.ID, d.ID)
		if !errors.Is(err, state.ErrNoRollbackTarget) {
			t.Fatalf("wrong app: got %v, want ErrNoRollbackTarget", err)
		}
	})

	t.Run("LiveTargetReturnsErrRollbackTargetAlreadyLive", func(t *testing.T) {
		_, app := seedPgAccountAndApp(t, s, ctx)
		d := seedPgDeployment(t, s, ctx, app)
		// Newly-created deployment is status='live' by default per
		// the deployment state machine; MarkDeploymentLive is the
		// explicit transition used by rollback, but the seed helper
		// already leaves it live.
		got, err := s.GetDeploymentByIDScopedToSuperseded(ctx, app.ID, d.ID)
		if !errors.Is(err, state.ErrRollbackTargetAlreadyLive) {
			t.Fatalf("live target: got err=%v, want ErrRollbackTargetAlreadyLive; got.Deployment=%+v", err, got)
		}
	})

	t.Run("PendingTargetReturnsErrRollbackTargetAlreadyLive", func(t *testing.T) {
		// Pending deployments (status='pending') are not yet live; they
		// must also be rejected — you cannot rollback to something that
		// hasn't been deployed yet.
		_, app := seedPgAccountAndApp(t, s, ctx)
		d := seedPgDeployment(t, s, ctx, app)
		if err := s.UpdateDeploymentStatus(ctx, d.ID, state.DeployPending, ""); err != nil {
			t.Fatalf("UpdateDeploymentStatus(pending): %v", err)
		}
		_, err := s.GetDeploymentByIDScopedToSuperseded(ctx, app.ID, d.ID)
		if !errors.Is(err, state.ErrRollbackTargetAlreadyLive) {
			t.Fatalf("pending target: got %v, want ErrRollbackTargetAlreadyLive", err)
		}
	})

	t.Run("SupersededTargetReturns", func(t *testing.T) {
		_, app := seedPgAccountAndApp(t, s, ctx)
		d := seedPgDeployment(t, s, ctx, app)
		if err := s.MarkDeploymentSuperseded(ctx, d.ID); err != nil {
			t.Fatalf("MarkDeploymentSuperseded: %v", err)
		}
		got, err := s.GetDeploymentByIDScopedToSuperseded(ctx, app.ID, d.ID)
		if err != nil {
			t.Fatalf("superseded target: unexpected err %v", err)
		}
		if got.ID != d.ID {
			t.Errorf("superseded target: got.ID=%q, want %q", got.ID, d.ID)
		}
		if got.Status != state.DeploySuperseded {
			t.Errorf("superseded target: got.Status=%q, want %q", got.Status, state.DeploySuperseded)
		}
	})

	t.Run("SkipsIntermediateSuperseded", func(t *testing.T) {
		// Regression test for the "specific rollback over an intermediate"
		// use case from the SAFE-RELEASES-G plan. With three superseded
		// deployments under one app, requesting the second one returns
		// exactly that row — LatestSupersededDeployment would return the
		// third (most-recent), GetDeploymentByIDScopedToSuperseded must
		// return the explicitly-requested id regardless of created_at order.
		_, app := seedPgAccountAndApp(t, s, ctx)
		d1 := seedPgDeployment(t, s, ctx, app)
		d2 := seedPgDeployment(t, s, ctx, app)
		d3 := seedPgDeployment(t, s, ctx, app)
		for _, id := range []string{d1.ID, d2.ID, d3.ID} {
			if err := s.MarkDeploymentSuperseded(ctx, id); err != nil {
				t.Fatalf("MarkDeploymentSuperseded(%s): %v", id, err)
			}
		}
		got, err := s.GetDeploymentByIDScopedToSuperseded(ctx, app.ID, d2.ID)
		if err != nil {
			t.Fatalf("middle superseded: unexpected err %v", err)
		}
		if got.ID != d2.ID {
			t.Errorf("middle superseded: got.ID=%q, want %q", got.ID, d2.ID)
		}
	})
}
