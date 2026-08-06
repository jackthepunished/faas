package state_test

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/onebox-faas/faas/pkg/state"
)

// Third batch of PgStore coverage round-trips: invocations,
// sessions, secrets/env, metering/builder usage, webhook dedupe,
// and deployment logs. All run against a fresh migrated schema via
// pgStore(t).

func TestPg_CoverageInvocations(t *testing.T) {
	s, ctx, account, app, _ := pgCoverageFixture(t)
	// EnqueueInvocation + the list variants.
	inv, err := s.EnqueueInvocation(ctx, state.Invocation{AccountID: account.ID, AppID: app.ID, Source: state.InvocationAsyncInvoke, DueAt: time.Now().Add(-time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	if got, err := s.ListInvocationsForAccount(ctx, account.ID, 10, ""); err != nil || len(got) != 1 || got[0].ID != inv.ID {
		t.Fatalf("invocations for account = %+v, %v", got, err)
	}
	if got, err := s.ListInvocationsForApp(ctx, app.ID); err != nil || len(got) != 1 {
		t.Fatalf("invocations for app (no states) = %+v, %v", got, err)
	}
	if got, err := s.ListInvocationsForApp(ctx, app.ID, state.InvocationPending); err != nil || len(got) != 1 {
		t.Fatalf("invocations for app pending = %+v, %v", got, err)
	}
	if got, err := s.ListInvocationsForApp(ctx, app.ID, state.InvocationCompleted); err != nil || len(got) != 0 {
		t.Fatalf("invocations for app completed = %+v, %v", got, err)
	}
}

func TestPg_CoverageSessions(t *testing.T) {
	s, ctx, account, _, _ := pgCoverageFixture(t)
	// CreateSession + GetSession (hit/miss).
	sess, err := s.CreateSession(ctx, uuid.NewString(), account.ID, "203.0.113.5", "test-agent")
	if err != nil {
		t.Fatal(err)
	}
	if got, err := s.GetSession(ctx, sess.ID); err != nil || got.ID != sess.ID || got.AccountID != account.ID {
		t.Fatalf("get session = %+v, %v", got, err)
	}
	if _, err := s.GetSession(ctx, uuid.NewString()); !errors.Is(err, state.ErrNotFound) {
		t.Fatalf("get session missing = %v", err)
	}
	// ListSessions + TouchSessionLastSeen.
	if got, err := s.ListSessions(ctx, account.ID); err != nil || len(got) != 1 {
		t.Fatalf("list sessions = %+v, %v", got, err)
	}
	if err := s.TouchSessionLastSeen(ctx, sess.ID); err != nil {
		t.Fatal(err)
	}
	// RevokeSession (true on real write, false on no-op).
	if revoked, err := s.RevokeSession(ctx, sess.ID, account.ID); err != nil || !revoked {
		t.Fatalf("revoke = %v, %v", revoked, err)
	}
	if revoked, err := s.RevokeSession(ctx, sess.ID, account.ID); err != nil || revoked {
		t.Fatalf("revoke repeat = %v, %v", revoked, err)
	}
	// RevokeAllSessions with an except-id.
	sess2, err := s.CreateSession(ctx, uuid.NewString(), account.ID, "203.0.113.6", "agent-2")
	if err != nil {
		t.Fatal(err)
	}
	n, err := s.RevokeAllSessions(ctx, account.ID, sess2.ID)
	if err != nil || n != 0 {
		// Both were already revoked (sess) — sess2 is excluded, so 0.
		t.Fatalf("revoke all = %d, %v", n, err)
	}
	// A fresh session gets revoked by RevokeAllSessions.
	sess3, err := s.CreateSession(ctx, uuid.NewString(), account.ID, "203.0.113.7", "agent-3")
	if err != nil {
		t.Fatal(err)
	}
	n, err = s.RevokeAllSessions(ctx, account.ID, sess2.ID)
	if err != nil || n != 1 || sess3.ID == sess2.ID {
		t.Fatalf("revoke all fresh = %d, %v", n, err)
	}
}

func TestPg_CoverageSecretsAndEnv(t *testing.T) {
	s, ctx, account, app, _ := pgCoverageFixture(t)
	// UpsertAppSecret + ListAppSecrets + CountAppSecrets + DeleteAppSecret.
	if err := s.UpsertAppSecret(ctx, account.ID, app.ID, "DB_URL", []byte("cipher-a")); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertAppSecret(ctx, account.ID, app.ID, "API_KEY", []byte("cipher-b")); err != nil {
		t.Fatal(err)
	}
	if got, err := s.ListAppSecrets(ctx, account.ID, app.ID); err != nil || len(got) != 2 {
		t.Fatalf("list secrets = %+v, %v", got, err)
	}
	if got, err := s.ListAppSecretsForAccount(ctx, account.ID, 10, ""); err != nil || len(got) != 2 {
		t.Fatalf("secrets for account = %+v, %v", got, err)
	}
	if n, err := s.CountAppSecrets(ctx, account.ID, app.ID); err != nil || n != 2 {
		t.Fatalf("count secrets = %d, %v", n, err)
	}
	if err := s.DeleteAppSecret(ctx, account.ID, app.ID, "DB_URL"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteAppSecret(ctx, account.ID, app.ID, "DB_URL"); !errors.Is(err, state.ErrNotFound) {
		t.Fatalf("delete secret repeat = %v", err)
	}
	// UpsertAppEnv + ListAppEnv + CountAppEnv + DeleteAppEnv.
	if err := s.UpsertAppEnv(ctx, account.ID, app.ID, "PORT", "8080"); err != nil {
		t.Fatal(err)
	}
	if got, err := s.ListAppEnv(ctx, account.ID, app.ID); err != nil || len(got) != 1 || got[0].Value != "8080" {
		t.Fatalf("list env = %+v, %v", got, err)
	}
	if n, err := s.CountAppEnv(ctx, account.ID, app.ID); err != nil || n != 1 {
		t.Fatalf("count env = %d, %v", n, err)
	}
	if err := s.DeleteAppEnv(ctx, account.ID, app.ID, "PORT"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteAppEnv(ctx, account.ID, app.ID, "PORT"); !errors.Is(err, state.ErrNotFound) {
		t.Fatalf("delete env repeat = %v", err)
	}
}

func TestPg_CoverageMeteringAndWebhooks(t *testing.T) {
	s, ctx, account, app, deployment := pgCoverageFixture(t)
	// AppendBuilderUsage (idempotent on build_id). build_id is a uuid FK.
	buildID := uuid.NewString()
	if err := s.AppendBuilderUsage(ctx, account.ID, app.ID, buildID, time.Now(), "dockerfile", 120); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendBuilderUsage(ctx, account.ID, app.ID, buildID, time.Now(), "dockerfile", 999); err != nil {
		t.Fatal("duplicate builder usage should be a no-op")
	}
	// UsageDaily + AppendSnapshotStorage + StorageUsage + LatestSnapshotBytes.
	day := time.Now().UTC().Truncate(24 * time.Hour)
	if got, err := s.UsageDaily(ctx, account.ID, day); err != nil || len(got) != 0 {
		t.Fatalf("usage daily = %+v, %v", got, err)
	}
	if err := s.AppendSnapshotStorage(ctx, account.ID, app.ID, day, 100, 200); err != nil {
		t.Fatal(err)
	}
	if got, err := s.StorageUsage(ctx, account.ID, day); err != nil || len(got) != 1 {
		t.Fatalf("storage usage = %+v, %v", got, err)
	}
	if mb, disk, err := s.LatestSnapshotBytes(ctx, app.ID); err != nil || mb != 0 || disk != 0 {
		t.Fatalf("latest snapshot bytes = %d/%d, %v", mb, disk, err)
	}
	// HasStripePushHour + RecordStripePushHour.
	hour := time.Now().UTC().Truncate(time.Hour)
	if ok, err := s.HasStripePushHour(ctx, account.ID, hour); err != nil || ok {
		t.Fatalf("stripe hour before = %v, %v", ok, err)
	}
	if err := s.RecordStripePushHour(ctx, account.ID, hour); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.HasStripePushHour(ctx, account.ID, hour); err != nil || !ok {
		t.Fatalf("stripe hour after = %v, %v", ok, err)
	}
	// CheckWebhookReplay + RecordWebhookDelivery + SweepExpiredWebhookDeliveries.
	if replay, err := s.CheckWebhookReplay(ctx, "stripe", "del-1", time.Now().Add(-5*time.Minute)); err != nil || replay {
		t.Fatalf("replay before = %v, %v", replay, err)
	}
	if err := s.RecordWebhookDelivery(ctx, "stripe", "del-1", time.Now().Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if replay, err := s.CheckWebhookReplay(ctx, "stripe", "del-1", time.Now().Add(-5*time.Minute)); err != nil || !replay {
		t.Fatalf("replay after = %v, %v", replay, err)
	}
	if err := s.RecordWebhookDelivery(ctx, "github", "del-old", time.Now().Add(-10*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if n, err := s.SweepExpiredWebhookDeliveries(ctx, time.Now()); err != nil || n != 1 {
		t.Fatalf("sweep = %d, %v", n, err)
	}
	_ = deployment
}

func TestPg_CoverageDeploymentLogs(t *testing.T) {
	s, ctx, _, _, deployment := pgCoverageFixture(t)
	// AppendDeploymentLog + ListDeploymentLogs (paging + hasMore).
	seq1, err := s.AppendDeploymentLog(ctx, deployment.ID, "build", "line 1")
	if err != nil {
		t.Fatal(err)
	}
	seq2, err := s.AppendDeploymentLog(ctx, deployment.ID, "build", "line 2")
	if err != nil {
		t.Fatal(err)
	}
	rows, hasMore, err := s.ListDeploymentLogs(ctx, deployment.ID, 0, 10)
	if err != nil || len(rows) != 2 || hasMore {
		t.Fatalf("logs = %d rows/%v, %v", len(rows), hasMore, err)
	}
	if rows[0].Seq != seq2 || rows[1].Seq != seq1 {
		t.Fatalf("log order = %d, %d", rows[0].Seq, rows[1].Seq)
	}
	// beforeSeq paging.
	rows, hasMore, err = s.ListDeploymentLogs(ctx, deployment.ID, seq2, 10)
	if err != nil || len(rows) != 1 || rows[0].Seq != seq1 || hasMore {
		t.Fatalf("logs before = %d rows/%v, %v", len(rows), hasMore, err)
	}
	// limit clamp + hasMore on a full page.
	rows, hasMore, err = s.ListDeploymentLogs(ctx, deployment.ID, 0, 1)
	if err != nil || len(rows) != 1 || !hasMore {
		t.Fatalf("logs limit 1 = %d rows/%v, %v", len(rows), hasMore, err)
	}
}
