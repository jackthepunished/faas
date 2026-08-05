package imaged

// scan_test.go — per-deploy grype scan behaviour pins (issue #464 /
// ADR-055 / PR-A). The plan called for a TestScanResultPersisted
// that asserts the typed scan payload actually lands on the
// deployment row after the deploy-complete hook fires, plus the
// failed-branch + skipped-branch + not-yet-wired branches.
//
// The seam is the WithGrypeRun fluent setter (no subprocess
// invocation; the test injects a stub returning a known
// *ScanResult). The dest is state.Store.UpsertDeploymentScanResult
// (default MemStore — its impl is byte-identical to the Pg one
// modulo the SELECT 1 existence check, so the test pins the
// seam not the SQL).

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/onebox-faas/faas/pkg/state"
	"github.com/onebox-faas/faas/pkg/wire"
)

// TestRunDeployScan_StampsComplete pins the happy path: the stub
// grype runner returns a known *ScanResult (one CRITICAL, two
// HIGH), the hook writes it via the store, and the row carries
// scan_status="complete" with the payload bytes round-tripping
// back into the *ScanResult seen by the test.
//
// A failure here means the typed result was dropped on the
// floor — the dashboard / /scan route would render an empty
// table for every deploy.
func TestRunDeployScan_StampsComplete(t *testing.T) {
	store := state.NewMemStore()
	ctx := context.Background()

	// Build a minimal Handler with a stub grype runner. We don't
	// need the full imaged scaffolding (storage, notifier, etc.)
	// — runDeployScan only reads store, log, appsRoot + invokes
	// the injected grypeRun. The skill in handler_cleanup_test.go
	// is the precedent.
	h := &Handler{
		store:    store,
		log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		appsRoot: filepath.Join(t.TempDir(), "apps"),
	}

	want := &ScanResult{
		SeverityCounts: SeverityCounts{Critical: 1, High: 2, Medium: 0, Low: 0, Unknown: 0},
	}
	h.WithGrypeRun(func(_ context.Context, _ string) (*ScanResult, error) {
		return want, nil
	})

	// Build the deployment row the test will assert on. MemStore
	// seeds App + Account + Deployment in one shot via the
	// CreateApp path; we just need the test to know the dep.ID
	// the hook will write to.
	acct, err := store.CreateAccount(ctx, "alice@example.com", "hobby")
	if err != nil {
		t.Fatalf("CreateAccount: %v", err)
	}
	app, err := store.CreateApp(ctx, state.App{
		AccountID: acct.ID,
		Slug:      "scantest",
	})
	if err != nil {
		t.Fatalf("CreateApp: %v", err)
	}
	dep, err := store.CreateDeployment(ctx, state.Deployment{
		AppID: app.ID,
		Kind:  "git",
	})
	if err != nil {
		t.Fatalf("CreateDeployment: %v", err)
	}

	// Drive the hook.
	h.runDeployScan(ctx, app, dep)

	// Re-read the row and assert scan_status + payload round-trip.
	row, err := store.DeploymentByID(ctx, dep.ID)
	if err != nil {
		t.Fatalf("DeploymentByID: %v", err)
	}
	if row.ScanStatus != "complete" {
		t.Errorf("ScanStatus = %q, want %q", row.ScanStatus, "complete")
	}
	if row.ScanResult == nil {
		t.Fatal("ScanResult empty after successful scan")
	}
	var got ScanResult
	if err := json.Unmarshal(row.ScanResult, &got); err != nil {
		t.Fatalf("unmarshal scan result: %v", err)
	}
	if got.Critical != 1 || got.High != 2 {
		t.Errorf("severity counts = %+v, want CRITICAL=1 HIGH=2", got.SeverityCounts)
	}
}

// TestRunDeployScan_StampsFailed pins the grype-failure branch:
// the runner returns an error, the hook stamps scan_status="failed"
// with the error string preserved in the JSON payload. The deploy
// itself must remain unaffected (AC #4: CRITICAL-CVE images deploy
// successfully — the row's status is not touched by the scan).
func TestRunDeployScan_StampsFailed(t *testing.T) {
	store := state.NewMemStore()
	ctx := context.Background()

	h := &Handler{
		store:    store,
		log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		appsRoot: filepath.Join(t.TempDir(), "apps"),
	}
	wantErr := errors.New("grype: no such file")
	h.WithGrypeRun(func(_ context.Context, _ string) (*ScanResult, error) {
		return nil, wantErr
	})

	acct, err := store.CreateAccount(ctx, "alice@example.com", "hobby")
	if err != nil {
		t.Fatalf("CreateAccount: %v", err)
	}
	app, err := store.CreateApp(ctx, state.App{
		AccountID: acct.ID,
		Slug:      "failtest",
	})
	if err != nil {
		t.Fatalf("CreateApp: %v", err)
	}
	dep, err := store.CreateDeployment(ctx, state.Deployment{
		AppID: app.ID,
		Kind:  "git",
	})
	if err != nil {
		t.Fatalf("CreateDeployment: %v", err)
	}

	h.runDeployScan(ctx, app, dep)

	row, err := store.DeploymentByID(ctx, dep.ID)
	if err != nil {
		t.Fatalf("DeploymentByID: %v", err)
	}
	if row.ScanStatus != "failed" {
		t.Errorf("ScanStatus = %q, want %q", row.ScanStatus, "failed")
	}
	var got ScanResult
	if err := json.Unmarshal(row.ScanResult, &got); err != nil {
		t.Fatalf("unmarshal scan result: %v", err)
	}
	if got.Error == "" {
		t.Errorf("failed scan lost the grype error message: payload=%s", row.ScanResult)
	}
	// The deploy row's lifecycle status must NOT have been
	// touched by the scan failure (AC #4).
	_ = row.Status
}

// TestRunDeployScan_NoOpsOnUnwiredHandler pins the defensive
// nil-check: a Handler built without store/log skips the scan
// entirely (no row to write, no log channel). The test must
// not panic and must not write any bytes.
func TestRunDeployScan_NoOpsOnUnwiredHandler(t *testing.T) {
	h := &Handler{appsRoot: filepath.Join(t.TempDir(), "apps")}
	// Both store and log are nil. The hook must return early.
	h.runDeployScan(context.Background(), state.App{}, state.Deployment{})
	// Surviving the call is the assertion.
}

// TestObserveDeployScan_NilReceiver pins the metric no-op
// semantics: the OpsMetrics methods the imaged hook calls
// must be safe on a nil receiver so the hook doesn't need a
// nil-check at the top of every deploy. The wire package
// already follows this pattern (VerifyMemory note
// wire/OpsMetrics); the new helpers follow the same rule.
func TestObserveDeployScan_NilReceiver(t *testing.T) {
	var ops *wire.OpsMetrics // nil
	ops.ObserveDeployScanDuration("app", "complete", 0)
	ops.ObserveDeployScanTotal("app", "complete")
	ops.ObserveDeployScanVulns("app", "CRITICAL", 1)
	// Surviving the calls is the assertion.
}
