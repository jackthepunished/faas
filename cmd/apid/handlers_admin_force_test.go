// handlers_admin_force_test.go — pins the contract of the P2a
// (force-park) and P2b (force-cold-boot) admin recovery handlers
// added in Commit 5b of the operator-side observability mega-PR.
//
// The table-driven cases below cover the four load-bearing edges:
//
//  1. confirm-required tripwire — without ?confirm=true the
//     handler returns 400 validation_failed (no state mutation,
//     no schedd RPC, no audit row).
//  2. nil-scheddClient — the handler returns 503 schedd_unavailable
//     so the operator's CLI / dashboard surfaces a clear retry
//     signal rather than a 500.
//  3. state gate (force-park only) — instance state ∉
//     {RUNNING, WAKING, COLD_BOOTING} returns 409
//     instance_not_parkable WITHOUT calling schedd.
//  4. happy path — the handler forwards the instance id / slug
//     to the fake forceRecoverer and returns 200 OK with the
//     expected JSON shape.
//
// The fake forceRecoverer (defined below) captures the call
// args so each test can assert the forward path without standing
// up a gRPC server. This is the same pattern used by
// handlers_admin_billing_test.go's fakeBillingProvider.
//
// Tests for the gregalectl CLI wrapper (commands_instances.go)
// live in cmd/gregalectl/commands_instances_test.go.

package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/onebox-faas/faas/pkg/api"
	"github.com/onebox-faas/faas/pkg/state"
	"github.com/onebox-faas/faas/pkg/wire"
)

// fakeForceRecoverer is the in-test substitute for the
// forceRecoverer interface. Records every call so each test
// can assert (a) that the handler forwarded the expected id
// and (b) that the handler did NOT call schedd when the
// pre-conditions were not met. The error fields let tests
// simulate schedd-side failures (ErrNotFound, transient gRPC
// errors).
type fakeForceRecoverer struct {
	parkCalls          []parkCall
	forceColdBootCalls []forceColdBootCall
	parkErr            error
	coldBootErr        error
	coldBootSnapIDs    []string
}

type parkCall struct {
	InstanceID string
	Reason     string
}

type forceColdBootCall struct {
	DeploymentID string
}

func (f *fakeForceRecoverer) ParkInstance(_ context.Context, instanceID, reason string) error {
	f.parkCalls = append(f.parkCalls, parkCall{InstanceID: instanceID, Reason: reason})
	return f.parkErr
}

func (f *fakeForceRecoverer) ForceColdBootNextWake(_ context.Context, deploymentID string) ([]string, error) {
	f.forceColdBootCalls = append(f.forceColdBootCalls, forceColdBootCall{DeploymentID: deploymentID})
	return f.coldBootSnapIDs, f.coldBootErr
}

// newForceHarness wires a server with a MemStore + a
// fakeForceRecoverer. The admin allowlist is set to the
// caller's email so adminAllowlist (compute_nodes.go:74-86)
// passes — without it the request would 403 before reaching
// the handler. The bearer carries admin scope.
func newForceHarness(t *testing.T, fake *fakeForceRecoverer) (*server, *state.MemStore, string) {
	t.Helper()
	store := state.NewMemStore()
	acct, err := store.CreateAccount(context.Background(), "ops@example.com", api.PlanPro)
	if err != nil {
		t.Fatal(err)
	}
	pt, hash, err := api.GenerateAPIKey()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateAPIKey(context.Background(), acct.ID, hash, "force-test", api.ScopesAdminOnly); err != nil {
		t.Fatal(err)
	}
	ops := wire.NewOpsMetrics("apid_force_test")
	srv := newServer(store, slog.New(slog.NewTextHandler(io.Discard, nil)), "gregale.dev", noopNotifier{}).
		WithOpsMetrics(context.Background(), ops).
		WithAdminAllowlist("ops@example.com")
	if fake != nil {
		srv.WithScheddClient(fake)
	}
	return srv, store, pt
}

// seedRunningInstance inserts an app + deployment + instance row
// triple into the MemStore and returns the (instance id, app id)
// tuple. The instance's state defaults to "RUNNING"; callers can
// override via stateStr. The MemStore's CreateInstance takes
// positional args (mirrors the pgstore's INSERT), so this helper
// stays shape-compatible.
func seedRunningInstance(t *testing.T, store *state.MemStore, stateStr string) (string, string) {
	t.Helper()
	tenant, err := store.CreateAccount(context.Background(), "tenant@example.com", api.PlanHobby)
	if err != nil {
		t.Fatal(err)
	}
	app, err := store.CreateApp(context.Background(), state.App{
		AccountID: tenant.ID,
		Slug:      "tenant-app",
		RAMMB:     128,
		Runtime:   "node22",
		Type:      state.AppTypeFunction,
	})
	if err != nil {
		t.Fatal(err)
	}
	dep, err := store.CreateDeployment(context.Background(), state.Deployment{
		AppID:       app.ID,
		ImageDigest: "deadbeef",
	})
	if err != nil {
		t.Fatal(err)
	}
	ins, err := store.CreateInstance(context.Background(), app.ID, dep.ID, stateStr, 128, "", "")
	if err != nil {
		t.Fatal(err)
	}
	return ins.ID, app.ID
}

// TestPostForcePark_TableDriven pins the four edges of the
// force-park handler.
func TestPostForcePark_TableDriven(t *testing.T) {
	t.Run("missing_confirm_returns_400", func(t *testing.T) {
		fake := &fakeForceRecoverer{}
		srv, store, key := newForceHarness(t, fake)
		insID, _ := seedRunningInstance(t, store, "RUNNING")

		req := httptest.NewRequest(http.MethodPost,
			"/v1/admin/instances/"+insID+"/force-park", nil)
		req.Header.Set("Authorization", "Bearer "+key)
		rec := httptest.NewRecorder()
		srv.handler().ServeHTTP(rec, req)

		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
		}
		if len(fake.parkCalls) != 0 {
			t.Errorf("park should not have been called; got %d calls", len(fake.parkCalls))
		}
	})

	t.Run("nil_scheddClient_returns_503", func(t *testing.T) {
		srv, store, key := newForceHarness(t, nil)
		insID, _ := seedRunningInstance(t, store, "RUNNING")

		req := httptest.NewRequest(http.MethodPost,
			"/v1/admin/instances/"+insID+"/force-park?confirm=true", nil)
		req.Header.Set("Authorization", "Bearer "+key)
		rec := httptest.NewRecorder()
		srv.handler().ServeHTTP(rec, req)

		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("status = %d, want 503; body=%s", rec.Code, rec.Body.String())
		}
		var prob api.Problem
		if err := json.Unmarshal(rec.Body.Bytes(), &prob); err != nil {
			t.Fatalf("decode problem: %v body=%s", err, rec.Body.String())
		}
		if prob.Code != "schedd_unavailable" {
			t.Errorf("code = %q, want schedd_unavailable", prob.Code)
		}
	})

	t.Run("parked_state_returns_409", func(t *testing.T) {
		fake := &fakeForceRecoverer{}
		srv, store, key := newForceHarness(t, fake)
		insID, _ := seedRunningInstance(t, store, "PARKED")

		req := httptest.NewRequest(http.MethodPost,
			"/v1/admin/instances/"+insID+"/force-park?confirm=true&reason=already_parked", nil)
		req.Header.Set("Authorization", "Bearer "+key)
		rec := httptest.NewRecorder()
		srv.handler().ServeHTTP(rec, req)

		if rec.Code != http.StatusConflict {
			t.Fatalf("status = %d, want 409; body=%s", rec.Code, rec.Body.String())
		}
		var prob api.Problem
		if err := json.Unmarshal(rec.Body.Bytes(), &prob); err != nil {
			t.Fatalf("decode problem: %v body=%s", err, rec.Body.String())
		}
		if prob.Code != "instance_not_parkable" {
			t.Errorf("code = %q, want instance_not_parkable", prob.Code)
		}
		if len(fake.parkCalls) != 0 {
			t.Errorf("park should not have been called on 409; got %d calls", len(fake.parkCalls))
		}
	})

	t.Run("happy_path_calls_park", func(t *testing.T) {
		fake := &fakeForceRecoverer{}
		srv, store, key := newForceHarness(t, fake)
		insID, _ := seedRunningInstance(t, store, "RUNNING")

		req := httptest.NewRequest(http.MethodPost,
			"/v1/admin/instances/"+insID+"/force-park?confirm=true&reason=incident_42", nil)
		req.Header.Set("Authorization", "Bearer "+key)
		rec := httptest.NewRecorder()
		srv.handler().ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
		}
		if len(fake.parkCalls) != 1 {
			t.Fatalf("park calls = %d, want 1", len(fake.parkCalls))
		}
		if fake.parkCalls[0].InstanceID != insID {
			t.Errorf("park call instance_id = %q, want %q", fake.parkCalls[0].InstanceID, insID)
		}
		if fake.parkCalls[0].Reason != "incident_42" {
			t.Errorf("park call reason = %q, want incident_42", fake.parkCalls[0].Reason)
		}
		var body map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode body: %v body=%s", err, rec.Body.String())
		}
		if body["ok"] != true {
			t.Errorf("body.ok = %v, want true", body["ok"])
		}
		if body["previous_state"] != "RUNNING" {
			t.Errorf("body.previous_state = %v, want RUNNING", body["previous_state"])
		}
	})

	t.Run("schedd_notfound_returns_404", func(t *testing.T) {
		fake := &fakeForceRecoverer{parkErr: state.ErrNotFound}
		srv, store, key := newForceHarness(t, fake)
		insID, _ := seedRunningInstance(t, store, "RUNNING")

		req := httptest.NewRequest(http.MethodPost,
			"/v1/admin/instances/"+insID+"/force-park?confirm=true", nil)
		req.Header.Set("Authorization", "Bearer "+key)
		rec := httptest.NewRecorder()
		srv.handler().ServeHTTP(rec, req)

		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404; body=%s", rec.Code, rec.Body.String())
		}
	})

	t.Run("invalid_reason_returns_400", func(t *testing.T) {
		fake := &fakeForceRecoverer{}
		srv, store, key := newForceHarness(t, fake)
		insID, _ := seedRunningInstance(t, store, "RUNNING")

		// Space + punctuation are not in [a-z0-9_]; handler must 400.
		req := httptest.NewRequest(http.MethodPost,
			"/v1/admin/instances/"+insID+"/force-park?confirm=true&reason=has%20space", nil)
		req.Header.Set("Authorization", "Bearer "+key)
		rec := httptest.NewRecorder()
		srv.handler().ServeHTTP(rec, req)

		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
		}
		if len(fake.parkCalls) != 0 {
			t.Errorf("park should not have been called on bad reason; got %d calls", len(fake.parkCalls))
		}
	})
}

// TestPostForceColdBoot_TableDriven pins the three edges of the
// force-cold-boot handler.
func TestPostForceColdBoot_TableDriven(t *testing.T) {
	t.Run("missing_confirm_returns_400", func(t *testing.T) {
		fake := &fakeForceRecoverer{}
		srv, store, key := newForceHarness(t, fake)
		_, _ = seedRunningInstance(t, store, "RUNNING")

		req := httptest.NewRequest(http.MethodPost,
			"/v1/admin/apps/tenant-app/force-cold-boot", nil)
		req.Header.Set("Authorization", "Bearer "+key)
		rec := httptest.NewRecorder()
		srv.handler().ServeHTTP(rec, req)

		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
		}
		if len(fake.forceColdBootCalls) != 0 {
			t.Errorf("force-cold-boot should not have been called; got %d calls", len(fake.forceColdBootCalls))
		}
	})

	t.Run("unknown_slug_returns_404", func(t *testing.T) {
		fake := &fakeForceRecoverer{}
		srv, _, key := newForceHarness(t, fake)

		req := httptest.NewRequest(http.MethodPost,
			"/v1/admin/apps/does-not-exist/force-cold-boot?confirm=true", nil)
		req.Header.Set("Authorization", "Bearer "+key)
		rec := httptest.NewRecorder()
		srv.handler().ServeHTTP(rec, req)

		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404; body=%s", rec.Code, rec.Body.String())
		}
		if len(fake.forceColdBootCalls) != 0 {
			t.Errorf("force-cold-boot should not have been called on 404; got %d calls", len(fake.forceColdBootCalls))
		}
	})

	t.Run("happy_path_returns_snap_ids", func(t *testing.T) {
		fake := &fakeForceRecoverer{coldBootSnapIDs: []string{"snap-warm-1", "snap-init-2"}}
		srv, store, key := newForceHarness(t, fake)
		_, _ = seedRunningInstance(t, store, "RUNNING")

		req := httptest.NewRequest(http.MethodPost,
			"/v1/admin/apps/tenant-app/force-cold-boot?confirm=true&reason=incident_42", nil)
		req.Header.Set("Authorization", "Bearer "+key)
		rec := httptest.NewRecorder()
		srv.handler().ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
		}
		if len(fake.forceColdBootCalls) != 1 {
			t.Fatalf("force-cold-boot calls = %d, want 1", len(fake.forceColdBootCalls))
		}
		var body map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode body: %v body=%s", err, rec.Body.String())
		}
		if body["ok"] != true {
			t.Errorf("body.ok = %v, want true", body["ok"])
		}
		snaps, _ := body["snap_ids_marked_stale"].([]any)
		if len(snaps) != 2 {
			t.Errorf("snap_ids_marked_stale = %v, want 2 entries", snaps)
		}
	})

	t.Run("empty_snap_list_is_200", func(t *testing.T) {
		// Engine returns ([]string{}, nil) when the deployment
		// has no snapshots. Handler must still 200 (durable
		// record of operator check, even when no-op).
		fake := &fakeForceRecoverer{coldBootSnapIDs: []string{}}
		srv, store, key := newForceHarness(t, fake)
		_, _ = seedRunningInstance(t, store, "RUNNING")

		req := httptest.NewRequest(http.MethodPost,
			"/v1/admin/apps/tenant-app/force-cold-boot?confirm=true", nil)
		req.Header.Set("Authorization", "Bearer "+key)
		rec := httptest.NewRecorder()
		srv.handler().ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
		}
	})

	t.Run("schedd_transient_error_returns_503", func(t *testing.T) {
		fake := &fakeForceRecoverer{coldBootErr: errors.New("connection refused")}
		srv, store, key := newForceHarness(t, fake)
		_, _ = seedRunningInstance(t, store, "RUNNING")

		req := httptest.NewRequest(http.MethodPost,
			"/v1/admin/apps/tenant-app/force-cold-boot?confirm=true", nil)
		req.Header.Set("Authorization", "Bearer "+key)
		rec := httptest.NewRecorder()
		srv.handler().ServeHTTP(rec, req)

		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("status = %d, want 503; body=%s", rec.Code, rec.Body.String())
		}
		var prob api.Problem
		if err := json.Unmarshal(rec.Body.Bytes(), &prob); err != nil {
			t.Fatalf("decode problem: %v body=%s", err, rec.Body.String())
		}
		if prob.Code != "schedd_unavailable" {
			t.Errorf("code = %q, want schedd_unavailable", prob.Code)
		}
	})
}
