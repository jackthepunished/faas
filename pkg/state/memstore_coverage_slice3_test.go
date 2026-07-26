package state

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestMemStoreCoverageListsAndUsage(t *testing.T) {
	m, ctx, account, _, _ := memCoverageFixture(t)
	if accounts, err := m.ListAllAccounts(ctx); err != nil || len(accounts) < 1 {
		t.Fatalf("list all accounts = %+v, %v", accounts, err)
	}
	if apps, err := m.ListAllApps(ctx); err != nil || len(apps) < 1 {
		t.Fatalf("list all apps = %+v, %v", apps, err)
	}
	if nodes, err := m.ListAllComputeNodes(ctx); err != nil || len(nodes) < 1 {
		t.Fatalf("default node missing = %+v, %v", nodes, err)
	}
	if keys, err := m.ListAPIKeys(ctx, account.ID); err != nil || len(keys) != 0 {
		t.Fatalf("empty keys = %+v, %v", keys, err)
	}
	if got, err := m.UsageByAccount(ctx, account.ID, time.Now().Add(-time.Hour)); err != nil || len(got) != 0 {
		t.Fatalf("usage by account empty = %+v, %v", got, err)
	}
	if got, err := m.UsageByHour(ctx, account.ID, time.Now().Add(-time.Hour), time.Now()); err != nil || len(got) != 0 {
		t.Fatalf("usage by hour empty = %+v, %v", got, err)
	}
	if got, err := m.UsageByAccount(ctx, "missing", time.Now().Add(-time.Hour)); err != nil || len(got) != 0 {
		t.Fatalf("usage missing = %+v, %v", got, err)
	}
}

func TestMemStoreCoverageAccountDeletionAndGdpr(t *testing.T) {
	m, ctx, account, _, _ := memCoverageFixture(t)
	if err := m.MarkAccountDeletionPending(ctx, "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("mark deletion missing = %v", err)
	}
	if err := m.RestoreAccount(ctx, "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("restore missing = %v", err)
	}
	if err := m.DeleteAccount(ctx, "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("delete missing = %v", err)
	}
	if err := m.MarkAccountDeletionPending(ctx, account.ID); err != nil {
		t.Fatal(err)
	}
	if err := m.DeleteAccount(ctx, account.ID); err != nil {
		t.Fatal(err)
	}
	if err := m.RestoreAccount(ctx, account.ID); err == nil {
		t.Fatal("restore after delete should fail")
	}
	if err := m.RestoreAccount(ctx, account.ID); err == nil {
		t.Fatal("restore after delete should still fail")
	}
	req := GdprRequest{ID: uuid.NewString(), AccountID: account.ID, Action: GdprActionDelete, AccountEmail: account.Email, RequestedAt: time.Now()}
	if err := m.AppendGdprRequest(ctx, req); err != nil {
		t.Fatal(err)
	}
	if got, err := m.ListGdprRequestsForAccount(ctx, account.ID, 10); err != nil || len(got) != 1 {
		t.Fatalf("gdpr list = %+v, %v", got, err)
	}
	if got, _ := m.ListGdprRequestsForAccount(ctx, "missing", 10); len(got) != 0 {
		t.Fatalf("gdpr empty = %+v", got)
	}
	if err := m.CompleteGdprRequest(ctx, account.ID, string(req.Action)); err != nil {
		t.Fatal(err)
	}
	if err := m.CompleteGdprRequest(ctx, account.ID, string(req.Action)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("complete double = %v", err)
	}
	if err := m.CompleteGdprRequest(ctx, "missing", "delete"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("complete missing = %v", err)
	}
}

func TestMemStoreCoverageSecretsAndDunning(t *testing.T) {
	m, ctx, account, app, _ := memCoverageFixture(t)
	if err := m.UpsertAppSecret(ctx, account.ID, app.ID, "ENV", []byte("ciphertext")); err != nil {
		t.Fatal(err)
	}
	if err := m.UpsertAppSecret(ctx, account.ID, app.ID, "ENV", []byte("ciphertext-v2")); err != nil {
		t.Fatal(err)
	}
	if err := m.UpsertAppSecret(ctx, account.ID, app.ID, "OTHER", []byte("ct2")); err != nil {
		t.Fatal(err)
	}
	secrets, err := m.ListAppSecrets(ctx, account.ID, app.ID)
	if err != nil || len(secrets) != 2 {
		t.Fatalf("secrets list = %+v, %v", secrets, err)
	}
	var envSecret *AppSecret
	for i := range secrets {
		if secrets[i].Key == "ENV" {
			envSecret = &secrets[i]
		}
	}
	if envSecret == nil || string(envSecret.Ciphertext) != "ciphertext-v2" {
		t.Fatalf("env secret not updated: %+v", envSecret)
	}
	if _, err := m.ListAppSecrets(ctx, "wrong", app.ID); err != nil {
		t.Fatalf("cross-account list = %v", err)
	}
	if err := m.UpsertAppSecret(ctx, account.ID, "missing-app", "ENV", []byte("ct")); err != nil {
		t.Fatalf("upsert no validate = %v", err)
	}
	if err := m.DeleteAppSecret(ctx, account.ID, app.ID, "ENV"); err != nil {
		t.Fatal(err)
	}
	if err := m.DeleteAppSecret(ctx, account.ID, app.ID, "ENV"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("delete secret missing = %v", err)
	}
	if _, err := m.RenameApp(ctx, account.ID, "missing", "renamed"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("rename missing = %v", err)
	}
	renamed, err := m.RenameApp(ctx, account.ID, app.Slug, "renamed-"+uuid.NewString()[:6])
	if err != nil {
		t.Fatal(err)
	}
	if renamed.Slug == app.Slug {
		t.Fatalf("rename no-op: %+v", renamed)
	}
	if err := m.SetAppMinInstances(ctx, app.ID, 1); err != nil {
		t.Fatal(err)
	}
	if err := m.SetAppMinInstances(ctx, "missing", 0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("min instances missing = %v", err)
	}
	if err := m.UpdateAccountStatus(ctx, account.ID, AccountPastDue); err != nil {
		t.Fatal(err)
	}
	if err := m.UpdateAccountStatus(ctx, account.ID, AccountActive); err != nil {
		t.Fatal(err)
	}
	if err := m.MarkDunningStep(ctx, account.ID, AccountActive, AccountPastDue); err != nil {
		t.Fatal(err)
	}
	if err := m.MarkDunningStep(ctx, account.ID, AccountPastDue, AccountPastDue); err != nil {
		t.Fatalf("from==to backfill: %v", err)
	}
	if err := m.MarkDunningStep(ctx, "missing", AccountActive, AccountPastDue); !errors.Is(err, ErrNotFound) {
		t.Fatalf("dunning missing = %v", err)
	}
}

func TestMemStoreCoverageBuildClaims(t *testing.T) {
	m, ctx, account, _, _ := memCoverageFixture(t)
	if err := m.RecordRecentBuildClaim(ctx, "", "build"); err == nil {
		t.Fatal("empty account should fail")
	}
	if err := m.RecordRecentBuildClaim(ctx, account.ID, "build-1"); err != nil {
		t.Fatal(err)
	}
	if err := m.RecordRecentBuildClaim(ctx, account.ID, "build-2"); err != nil {
		t.Fatal(err)
	}
}
