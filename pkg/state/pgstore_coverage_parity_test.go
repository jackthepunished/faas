package state_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/onebox-faas/faas/pkg/api"
	"github.com/onebox-faas/faas/pkg/state"
)

func pgCoverageFixture(t *testing.T) (*state.PgStore, context.Context, state.Account, state.App, state.Deployment) {
	t.Helper()
	s, ctx := pgStore(t)
	now := time.Date(2026, 7, 26, 10, 0, 0, 0, time.UTC)
	account, err := s.CreateAccount(ctx, "pg-coverage-"+uuid.NewString()+"@example.com", api.PlanPro)
	if err != nil {
		t.Fatal(err)
	}
	app, err := s.CreateApp(ctx, state.App{AccountID: account.ID, Slug: "pg-coverage-" + uuid.NewString(), RAMMB: 512, MaxConcurrency: 5, IdleTimeoutS: 60})
	if err != nil {
		t.Fatal(err)
	}
	deployment, err := s.CreateDeployment(ctx, state.Deployment{AppID: app.ID, ImageDigest: "sha256:" + uuid.NewString(), Status: state.DeployPending, CreatedAt: now})
	if err != nil {
		t.Fatal(err)
	}
	return s, ctx, account, app, deployment
}

func TestPg_CoverageInvocationLifecycle(t *testing.T) {
	s, ctx, account, app, _ := pgCoverageFixture(t)
	if _, err := s.InvocationByID(ctx, "missing"); !errors.Is(err, state.ErrNotFound) {
		t.Fatalf("missing invocation = %v", err)
	}
	enqueue := func(due time.Time) (state.Invocation, error) {
		return s.EnqueueInvocation(ctx, state.Invocation{
			AccountID: account.ID, AppID: app.ID, Source: state.InvocationAsyncInvoke, DueAt: due,
		})
	}
	due, err := enqueue(time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	future, err := enqueue(time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if got, err := s.ListDueInvocations(ctx, time.Now(), 10); err != nil || len(got) != 1 || got[0].ID != due.ID {
		t.Fatalf("due list returned %d rows, want 1", len(got))
	}
	if _, err := s.ClaimInvocation(ctx, due.ID, "instance", 30); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimInvocation(ctx, due.ID, "instance", 30); !errors.Is(err, state.ErrNotFound) {
		t.Fatalf("double claim = %v", err)
	}
	if err := s.CompleteInvocation(ctx, due.ID, json.RawMessage(`{"ok":true}`)); err != nil {
		t.Fatal(err)
	}
	if err := s.FailInvocation(ctx, due.ID, "noop", 0); !errors.Is(err, state.ErrNotFound) {
		t.Fatalf("fail after completion = %v", err)
	}
	fail, err := enqueue(time.Now().Add(-time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimInvocation(ctx, fail.ID, "instance", 30); err != nil {
		t.Fatal(err)
	}
	if err := s.FailInvocation(ctx, fail.ID, "boom", time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := s.FailInvocation(ctx, fail.ID, "perm", 0); err != nil {
		t.Fatal(err)
	}
	if err := s.CancelInvocation(ctx, future.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CountInstanceInvocationsInMinute(ctx, "missing", time.Now()); err != nil {
		t.Fatal(err)
	}
}

func TestPg_CoveragePasswordOAuthIdempotency(t *testing.T) {
	s, ctx, account, _, _ := pgCoverageFixture(t)
	if err := s.SetAccountPassword(ctx, account.ID, "phc-test"); err != nil {
		t.Fatal(err)
	}
	if got, err := s.AccountPasswordByAccountID(ctx, account.ID); err != nil || got != "phc-test" {
		t.Fatalf("password read = %q, %v", got, err)
	}
	if err := s.SetAccountPassword(ctx, account.ID, "phc-test-v2"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetAccountPassword(ctx, "", "phc"); !errors.Is(err, state.ErrInvalidArgument) {
		t.Fatalf("empty account password = %v", err)
	}
	if _, err := s.AccountPasswordByAccountID(ctx, "missing"); !errors.Is(err, state.ErrNotFound) {
		t.Fatalf("password missing = %v", err)
	}
	if err := s.DeleteAccountPassword(ctx, account.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertOAuthLink(ctx, account.ID, "google", "subj", account.Email, true); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertOAuthLink(ctx, "other", "google", "subj", "other@example.com", true); !errors.Is(err, state.ErrConflict) {
		t.Fatalf("oauth takeover = %v", err)
	}
}

func TestPg_CoverageLoginAndCliTokens(t *testing.T) {
	s, ctx, account, _, _ := pgCoverageFixture(t)
	expired := []byte("expired-token")
	if err := s.IssueLoginToken(ctx, expired, account.ID, time.Now().Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ConsumeLoginToken(ctx, expired); !errors.Is(err, state.ErrNotFound) {
		t.Fatalf("expired login token = %v", err)
	}
	hash := []byte("pg-login-coverage")
	if err := s.IssueLoginToken(ctx, hash, account.ID, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ConsumeLoginToken(ctx, hash); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ConsumeLoginToken(ctx, hash); !errors.Is(err, state.ErrNotFound) {
		t.Fatalf("replay login token = %v", err)
	}
	if _, _, err := s.PeekCliAuthCode(ctx, []byte("missing-code")); err == nil {
		t.Fatal("peek missing code should error")
	}
	cliHash := []byte("pg-cli-coverage")
	if err := s.IssueCliAuthCode(ctx, cliHash, time.Now().Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.PeekCliAuthCode(ctx, cliHash); err == nil {
		t.Fatal("peek expired cli should error")
	}
	if err := s.IssueCliAuthCode(ctx, cliHash, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := s.ClaimCliAuthCode(ctx, cliHash, account.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.ConsumeCliAuthCode(ctx, cliHash); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.ConsumeCliAuthCode(ctx, cliHash); !errors.Is(err, state.ErrNotFound) {
		t.Fatalf("cli replay = %v", err)
	}
}

func TestPg_CoverageSnapshotsAndDomain(t *testing.T) {
	s, ctx, _, _, deployment := pgCoverageFixture(t)
	if _, err := s.CreateSnapshot(ctx, state.Snapshot{DeploymentID: deployment.ID}); err == nil {
		t.Fatal("snapshot without storage key should fail")
	}
	snap, err := s.CreateSnapshot(ctx, state.Snapshot{DeploymentID: deployment.ID, FCVersion: "v1", StorageKey: "snap/pg-1"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateSnapshot(ctx, state.Snapshot{DeploymentID: deployment.ID, FCVersion: "v1", StorageKey: "snap/pg-1"}); !errors.Is(err, state.ErrConflict) {
		t.Fatalf("duplicate snapshot = %v", err)
	}
	if err := s.MarkSnapshotStale(ctx, snap.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.LatestSnapshot(ctx, deployment.ID); !errors.Is(err, state.ErrNotFound) {
		t.Fatalf("latest stale = %v", err)
	}
	if _, err := s.CreateCustomDomain(ctx, "missing.example", uuid.NewString(), "tok"); err == nil {
		t.Fatal("domain with unknown app should fail")
	}
}

func TestPg_CoverageInstanceStatePaths(t *testing.T) {
	s, ctx, _, app, deployment := pgCoverageFixture(t)
	defaultNode := resolveDefaultLocal(t, ctx, s)
	instance, err := s.CreateInstance(ctx, app.ID, deployment.ID, string(state.StateRunning), 512, defaultNode, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateInstanceStateToTerminal(ctx, instance.ID, string(state.StateStopped), time.Now().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ListInstancesInTerminalStatesOlderThan(ctx, []state.State{state.StateStopped}, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ListInstancesByStatesOlderThan(ctx, []state.State{state.StateStopped}, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateInstanceState(ctx, "missing", string(state.StateRunning)); err == nil {
		t.Fatal("missing instance update should error")
	}
	if _, err := s.LiveDeployment(ctx, deployment.ID); err != nil {
		t.Fatal(err)
	}
}
