// pgstore_live_deployments_test.go — PR-B (issue #556) pinned tests
// for the plural LiveDeployments query.
//
// Two pinned contracts:
//
//   1. TestPg_LiveDeployments_ReturnsOnlyLiveRows — the plural
//      query returns only status='live' rows for the app. A
//      superseded row is filtered out. Empty/nil for an app
//      with no live rows (callers treat that as "no live
//      deployment, 503").
//
//   2. TestPg_LiveDeployments_PicksUpIndex — EXPLAIN confirms
//      the partial index deployments_live_traffic_idx is used
//      via Index Only Scan. Without INCLUDE the planner would
//      fall back to a heap fetch and the cost would scale with
//      row width — defeating the access path the picker relies
//      on at notify rate.

package state_test

import (
	"strings"
	"testing"

	"github.com/onebox-faas/faas/pkg/api"
	"github.com/onebox-faas/faas/pkg/state"
)

// TestPg_LiveDeployments_ReturnsOnlyLiveRows pins the sqlc plural
// query: every row returned has status='live'; superseded / failed
// / parked rows are filtered. An app with no live rows returns
// (nil, nil) per the Store contract.
func TestPg_LiveDeployments_ReturnsOnlyLiveRows(t *testing.T) {
	s, ctx := pgStore(t)
	acct, err := s.CreateAccount(ctx, "live-deps@example.com", api.PlanPro)
	if err != nil {
		t.Fatalf("CreateAccount: %v", err)
	}
	app, err := s.CreateApp(ctx, state.App{
		AccountID: acct.ID, Slug: "live-deps-app", Type: state.AppTypeApp,
		RAMMB: 512, MaxConcurrency: 5, IdleTimeoutS: 60,
	})
	if err != nil {
		t.Fatalf("CreateApp: %v", err)
	}

	// Empty case: no live deployments → (nil, nil).
	deps, err := s.LiveDeployments(ctx, app.ID)
	if err != nil {
		t.Fatalf("LiveDeployments (empty): %v", err)
	}
	if len(deps) != 0 {
		t.Errorf("LiveDeployments (empty) = %+v, want 0 rows", deps)
	}

	// Create + mark live: now plural returns 1 row.
	depA, err := s.CreateDeployment(ctx, state.Deployment{
		AppID: app.ID, Kind: state.DeploymentKindImage, ImageDigest: "sha256:aaa", Status: state.DeployPending,
	})
	if err != nil {
		t.Fatalf("CreateDeployment A: %v", err)
	}
	if err := s.MarkDeploymentLive(ctx, depA.ID); err != nil {
		t.Fatalf("MarkDeploymentLive A: %v", err)
	}

	deps, err = s.LiveDeployments(ctx, app.ID)
	if err != nil {
		t.Fatalf("LiveDeployments (1 live): %v", err)
	}
	if len(deps) != 1 {
		t.Fatalf("LiveDeployments (1 live) = %d rows, want 1", len(deps))
	}
	if deps[0].ID != depA.ID {
		t.Errorf("LiveDeployments[0].ID = %q, want dep-A (%q)", deps[0].ID, depA.ID)
	}
	if deps[0].TrafficPercent != 100 {
		t.Errorf("LiveDeployments[0].TrafficPercent = %d, want 100 (single live row)", deps[0].TrafficPercent)
	}

	// Mark A superseded: plural returns 0 rows.
	if err := s.MarkDeploymentSuperseded(ctx, depA.ID); err != nil {
		t.Fatalf("MarkDeploymentSuperseded A: %v", err)
	}
	deps, err = s.LiveDeployments(ctx, app.ID)
	if err != nil {
		t.Fatalf("LiveDeployments (post-supersede): %v", err)
	}
	if len(deps) != 0 {
		t.Errorf("LiveDeployments (post-supersede) = %+v, want 0 rows (superseded rows filtered)", deps)
	}
}

// TestPg_LiveDeployments_PicksUpIndex pins the access path: EXPLAIN
// must show Index Only Scan on deployments_live_traffic_idx. A
// regression that drops the INCLUDE columns would force a heap
// fetch — defeating the per-notify read the picker relies on.
func TestPg_LiveDeployments_PicksUpIndex(t *testing.T) {
	_, pool, ctx := pgStoreWithPool(t)
	rows, err := pool.Query(ctx, `EXPLAIN SELECT id, traffic_percent FROM deployments WHERE app_id = $1 AND status = 'live'`, "00000000-0000-0000-0000-000000000000")
	if err != nil {
		t.Fatalf("EXPLAIN: %v", err)
	}
	defer rows.Close()
	var plan strings.Builder
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatalf("scan: %v", err)
		}
		plan.WriteString(line)
		plan.WriteString("\n")
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}
	got := plan.String()
	// The Index Only Scan must reference deployments_live_traffic_idx
	// — not deployments_pkey or any other index. A heap fetch
	// (Seq Scan / Index Scan without "Only") is a regression.
	if !strings.Contains(got, "deployments_live_traffic_idx") {
		t.Errorf("EXPLAIN does not use deployments_live_traffic_idx:\n%s", got)
	}
	if !strings.Contains(got, "Index Only Scan") {
		t.Errorf("EXPLAIN is not Index Only Scan (INCLUDE columns missing?):\n%s", got)
	}
}
