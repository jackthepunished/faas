// pgstore_alert_metrics_test.go — PgStore tests for the alert
// evaluator's signal-feeding reads (issue #1233 / ADR-123):
//
//   - CountFailedDeploymentsSince   (deployment_failed metric)
//   - WasInvokedSuccessfullySince   (api_up metric)
//   - MTDSpendEurCents              (account_spend_eur metric)
//   - UpsertAccountSpendSnapshot    (meterd tick loop)
//   - MinCertExpiryForApp           (cert_expiry_seconds metric)
//   - RefreshCertExpiryStates       (meterd refresher goroutine)
//   - ListCertExpiryStateForWalker  (meterd gauge writer)
//
// These methods sit on the alert-evaluator hot path; the migration
// floor (check-state-coverage ≥ 70%) requires them to be exercised
// end-to-end against the schema. TestPg_CountFailedDeploymentsSince_*,
// TestPg_WasInvokedSuccessfullySince_*, etc., each insert a fresh
// fixture row tagged with the same UUIDs the methods filter on.
package state_test

import (
	"context"
	"testing"
	"time"

	"github.com/onebox-faas/faas/pkg/api"
	"github.com/onebox-faas/faas/pkg/state"
)

// seedTestAccount creates one account and one app on the test
// schema and returns their ids. Distinct from seedLiveDeploy in
// that we don't need a deployment — the alert-metric reads target
// invocations / spend / cert tables, not deployments.
func seedTestAccount(t *testing.T, s *state.PgStore, ctx context.Context, suffix string) (acctID, appID string) {
	t.Helper()
	acct, err := s.CreateAccount(ctx, "u-am-"+suffix+"@example.com", api.PlanPro)
	if err != nil {
		t.Fatalf("CreateAccount: %v", err)
	}
	app, err := s.CreateApp(ctx, state.App{
		AccountID: acct.ID, Slug: "am-app-" + suffix, Type: state.AppTypeApp,
		RAMMB: 256, MaxConcurrency: 5, IdleTimeoutS: 60,
	})
	if err != nil {
		t.Fatalf("CreateApp: %v", err)
	}
	return acct.ID, app.ID
}

func TestPg_CountFailedDeploymentsSince_EmptyWindow(t *testing.T) {
	s, _, ctx := pgStoreWithPool(t)
	acctID, appID := seedTestAccount(t, s, ctx, "fdep-empty")
	got, err := s.CountFailedDeploymentsSince(ctx, acctID, appID, time.Now().Add(-1*time.Hour))
	if err != nil {
		t.Fatalf("CountFailedDeploymentsSince: %v", err)
	}
	if got != 0 {
		t.Errorf("count = %d; want 0 (no deployments seeded)", got)
	}
}

func TestPg_CountFailedDeploymentsSince_OnlyFailedCount(t *testing.T) {
	s, pool, ctx := pgStoreWithPool(t)
	acctID, appID := seedTestAccount(t, s, ctx, "fdep-count")

	// One failed + one live + one pending in the last hour.
	// Only the failed one must count.
	if _, err := pool.Exec(ctx, `
		insert into deployments (id, app_id, image_digest, status, created_at)
		values (gen_random_uuid(), $1, 'img@sha256:0', 'failed',  now() - interval '5 minutes'),
		       (gen_random_uuid(), $1, 'img@sha256:0', 'live',    now() - interval '5 minutes'),
		       (gen_random_uuid(), $1, 'img@sha256:0', 'pending', now() - interval '5 minutes')`,
		appID); err != nil {
		t.Fatalf("insert deployments: %v", err)
	}
	got, err := s.CountFailedDeploymentsSince(ctx, acctID, appID, time.Now().Add(-1*time.Hour))
	if err != nil {
		t.Fatalf("CountFailedDeploymentsSince: %v", err)
	}
	if got != 1 {
		t.Errorf("count = %d; want 1 (only the failed deployment)", got)
	}
}

func TestPg_CountFailedDeploymentsSince_EmptyAppArgMeansAccountScope(t *testing.T) {
	s, pool, ctx := pgStoreWithPool(t)
	acctID, appID := seedTestAccount(t, s, ctx, "fdep-account")

	if _, err := pool.Exec(ctx, `
		insert into deployments (id, app_id, image_digest, status, created_at)
		values (gen_random_uuid(), $1, 'img@sha256:0', 'failed', now() - interval '5 minutes')`,
		appID); err != nil {
		t.Fatalf("insert deployment: %v", err)
	}
	// Pass empty appID — the store treats "" as "any app on this
	// account". Used by the alert evaluator when scoping an
	// account-wide alert.
	got, err := s.CountFailedDeploymentsSince(ctx, acctID, "", time.Now().Add(-1*time.Hour))
	if err != nil {
		t.Fatalf("CountFailedDeploymentsSince: %v", err)
	}
	if got != 1 {
		t.Errorf("count = %d; want 1 (account-scoped query)", got)
	}
}

func TestPg_WasInvokedSuccessfullySince_EmptyReturnsFalse(t *testing.T) {
	s, _, ctx := pgStoreWithPool(t)
	acctID, appID := seedTestAccount(t, s, ctx, "wiss-empty")
	got, err := s.WasInvokedSuccessfullySince(ctx, acctID, appID, time.Now().Add(-1*time.Hour))
	if err != nil {
		t.Fatalf("WasInvokedSuccessfullySince: %v", err)
	}
	if got {
		t.Errorf("got = true; want false (cold start)")
	}
}

func TestPg_WasInvokedSuccessfullySince_TrueOnSuccess(t *testing.T) {
	s, pool, ctx := pgStoreWithPool(t)
	acctID, appID := seedTestAccount(t, s, ctx, "wiss-true")

	if _, err := pool.Exec(ctx, `
		insert into invocations (id, account_id, app_id, source, state, created_at)
		values (gen_random_uuid(), $1, $2, 'async_invoke', 'completed', now() - interval '5 minutes')`,
		acctID, appID); err != nil {
		t.Fatalf("insert invocation: %v", err)
	}
	got, err := s.WasInvokedSuccessfullySince(ctx, acctID, appID, time.Now().Add(-1*time.Hour))
	if err != nil {
		t.Fatalf("WasInvokedSuccessfullySince: %v", err)
	}
	if !got {
		t.Errorf("got = false; want true (a succeeded invocation exists)")
	}
}

func TestPg_UpsertAccountSpendSnapshot_InsertAndUpsert(t *testing.T) {
	s, pool, ctx := pgStoreWithPool(t)
	acctID, _ := seedTestAccount(t, s, ctx, "spend")

	now := time.Now().UTC()
	if err := s.UpsertAccountSpendSnapshot(ctx, acctID, now, now.Add(time.Minute), 12.5, 12345, "meterd"); err != nil {
		t.Fatalf("UpsertAccountSpendSnapshot (insert): %v", err)
	}
	// Verify row landed.
	var eur int64
	if err := pool.QueryRow(ctx, `select eur_cents from account_spend_snapshot where account_id = $1`, acctID).Scan(&eur); err != nil {
		t.Fatalf("query after insert: %v", err)
	}
	if eur != 12345 {
		t.Errorf("eur_cents = %d; want 12345", eur)
	}

	// ON CONFLICT (account_id, source, period_end) DO UPDATE —
	// the same period_end with a fresh eur_cents must overwrite.
	if err := s.UpsertAccountSpendSnapshot(ctx, acctID, now, now.Add(time.Minute), 12.5, 99999, "meterd"); err != nil {
		t.Fatalf("UpsertAccountSpendSnapshot (upsert): %v", err)
	}
	if err := pool.QueryRow(ctx, `select eur_cents from account_spend_snapshot where account_id = $1`, acctID).Scan(&eur); err != nil {
		t.Fatalf("query after upsert: %v", err)
	}
	if eur != 99999 {
		t.Errorf("eur_cents = %d; want 99999 (upsert must overwrite)", eur)
	}
}

func TestPg_MTDSpendEurCents_SumsAcrossSources(t *testing.T) {
	s, _, ctx := pgStoreWithPool(t)
	acctID, _ := seedTestAccount(t, s, ctx, "mtd")

	now := time.Now().UTC()
	// Two sources, two periods, both inside the MTD window.
	if err := s.UpsertAccountSpendSnapshot(ctx, acctID, now, now.Add(time.Minute), 1.0, 100, "meterd"); err != nil {
		t.Fatalf("UpsertAccountSpendSnapshot (a): %v", err)
	}
	if err := s.UpsertAccountSpendSnapshot(ctx, acctID, now, now.Add(2*time.Minute), 2.0, 250, "manual"); err != nil {
		t.Fatalf("UpsertAccountSpendSnapshot (b): %v", err)
	}
	got, err := s.MTDSpendEurCents(ctx, acctID)
	if err != nil {
		t.Fatalf("MTDSpendEurCents: %v", err)
	}
	if got != 350 {
		t.Errorf("total = %d; want 350 (100 + 250)", got)
	}
}

func TestPg_MTDSpendEurCents_EmptyAccountReturnsZero(t *testing.T) {
	s, _, ctx := pgStoreWithPool(t)
	acctID, _ := seedTestAccount(t, s, ctx, "mtd-empty")
	got, err := s.MTDSpendEurCents(ctx, acctID)
	if err != nil {
		t.Fatalf("MTDSpendEurCents: %v", err)
	}
	if got != 0 {
		t.Errorf("total = %d; want 0 (no rows for fresh account)", got)
	}
}

func TestPg_MinCertExpiryForApp_NoSurfacesReturnsMinusOne(t *testing.T) {
	s, _, ctx := pgStoreWithPool(t)
	acctID, appID := seedTestAccount(t, s, ctx, "cert-empty")
	got, err := s.MinCertExpiryForApp(ctx, acctID, appID)
	if err != nil {
		t.Fatalf("MinCertExpiryForApp: %v", err)
	}
	if got != -1 {
		t.Errorf("got = %d; want -1 (no surfaces)", got)
	}
}

func TestPg_MinCertExpiryForApp_PicksMinAcrossRows(t *testing.T) {
	s, pool, ctx := pgStoreWithPool(t)
	acctID, appID := seedTestAccount(t, s, ctx, "cert-min")

	// Insert two surfaces with different last_observed_cert_not_after
	// values; the store must return the smaller remaining-seconds.
	if _, err := pool.Exec(ctx, `
		insert into meterd_tenant_surface_cert_expiry_state
			(tenant_surface_id, account_id, app_id, hostname,
			 last_observed_cert_not_after, last_walk_status, last_refreshed_at)
		values (gen_random_uuid(), $1, $2, 'a.example',
		        now() + interval '30 days', 'ok', now()),
		       (gen_random_uuid(), $1, $2, 'b.example',
		        now() + interval '7 days',  'ok', now())`,
		acctID, appID); err != nil {
		t.Fatalf("insert cert states: %v", err)
	}
	got, err := s.MinCertExpiryForApp(ctx, acctID, appID)
	if err != nil {
		t.Fatalf("MinCertExpiryForApp: %v", err)
	}
	// 7 days = 604800 seconds, 30 days = 2592000. The min must
	// land in the 7-day ballpark (allow ±5 min slack for clock
	// drift between insert and read).
	const sevenDays = int64(7 * 24 * 60 * 60)
	if got < sevenDays-300 || got > sevenDays+300 {
		t.Errorf("min = %d seconds; want ~%d (7 days, ±5min)", got, sevenDays)
	}
}

func TestPg_RefreshCertExpiryStates_UpsertsFromTenantSurfaces(t *testing.T) {
	s, pool, ctx := pgStoreWithPool(t)
	acctID, appID := seedTestAccount(t, s, ctx, "cert-refresh")

	// Insert one tenant_surface with cert_state='issued'. The
	// refresher must mirror it into meterd_tenant_surface_cert_expiry_state.
	var surfaceID string
	if err := pool.QueryRow(ctx, `
		insert into tenant_surfaces (id, account_id, app_id, hostname, cert_state, cert_not_after)
		values (gen_random_uuid(), $1, $2, 'r.example', 'issued', now() + interval '14 days')
		returning id`, acctID, appID).Scan(&surfaceID); err != nil {
		t.Fatalf("insert tenant_surface: %v", err)
	}

	n, err := s.RefreshCertExpiryStates(ctx)
	if err != nil {
		t.Fatalf("RefreshCertExpiryStates: %v", err)
	}
	if n != 1 {
		t.Errorf("rows upserted = %d; want 1", n)
	}
	var lastWalk string
	if err := pool.QueryRow(ctx, `select last_walk_status from meterd_tenant_surface_cert_expiry_state where tenant_surface_id = $1`, surfaceID).Scan(&lastWalk); err != nil {
		t.Fatalf("query mirrored row: %v", err)
	}
	if lastWalk != "ok" {
		t.Errorf("last_walk_status = %q; want \"ok\"", lastWalk)
	}
}

func TestPg_RefreshCertExpiryStates_CertUnissuedWhenNotAfterIsNull(t *testing.T) {
	s, pool, ctx := pgStoreWithPool(t)
	acctID, appID := seedTestAccount(t, s, ctx, "cert-unissued")

	// Defensive path: cert_state='issued' but cert_not_after is
	// NULL (CHECK in 00243 should prevent but the refresher must
	// not crash). Status must land as 'cert_unissued'.
	var surfaceID string
	if err := pool.QueryRow(ctx, `
		insert into tenant_surfaces (id, account_id, app_id, hostname, cert_state, cert_not_after)
		values (gen_random_uuid(), $1, $2, 'u.example', 'issued', null)
		returning id`, acctID, appID).Scan(&surfaceID); err != nil {
		t.Fatalf("insert tenant_surface: %v", err)
	}
	if _, err := s.RefreshCertExpiryStates(ctx); err != nil {
		t.Fatalf("RefreshCertExpiryStates: %v", err)
	}
	var lastWalk string
	if err := pool.QueryRow(ctx, `select last_walk_status from meterd_tenant_surface_cert_expiry_state where tenant_surface_id = $1`, surfaceID).Scan(&lastWalk); err != nil {
		t.Fatalf("query mirrored row: %v", err)
	}
	if lastWalk != "cert_unissued" {
		t.Errorf("last_walk_status = %q; want \"cert_unissued\"", lastWalk)
	}
}

func TestPg_ListCertExpiryStateForWalker_StaleCutoffFilters(t *testing.T) {
	s, pool, ctx := pgStoreWithPool(t)
	acctID, appID := seedTestAccount(t, s, ctx, "cert-walker")

	// Two rows: one fresh, one stale (last_refreshed_at = 1h ago).
	if _, err := pool.Exec(ctx, `
		insert into meterd_tenant_surface_cert_expiry_state
			(tenant_surface_id, account_id, app_id, hostname,
			 last_observed_cert_not_after, last_walk_status, last_refreshed_at)
		values (gen_random_uuid(), $1, $2, 'fresh.example',
		        now() + interval '14 days', 'ok', now()),
		       (gen_random_uuid(), $1, $2, 'stale.example',
		        now() + interval '14 days', 'ok', now() - interval '1 hour')`,
		acctID, appID); err != nil {
		t.Fatalf("insert cert states: %v", err)
	}

	got, err := s.ListCertExpiryStateForWalker(ctx, 5*time.Minute)
	if err != nil {
		t.Fatalf("ListCertExpiryStateForWalker: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("rows = %d; want 1 (stale row filtered)", len(got))
	}
	if got[0].Hostname != "fresh.example" {
		t.Errorf("hostname = %q; want \"fresh.example\"", got[0].Hostname)
	}
}
