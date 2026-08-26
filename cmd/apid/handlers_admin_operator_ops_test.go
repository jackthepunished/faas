package main

import (
	"testing"
	"time"

	"github.com/onebox-faas/faas/pkg/api"
	"github.com/onebox-faas/faas/pkg/state"
)

func TestObsTenantActivity_UnknownTenant(t *testing.T) {
	e := newObsEnv(t, api.ScopesAdminOnly, "ops@faas.dev", "ops@faas.dev")
	rec := e.do(t, "GET", "/v1/admin/obs/tenants/00000000-0000-0000-0000-000000000000/activity", nil, nil)
	if rec.Code != 404 {
		t.Fatalf("tenant activity: got status %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}

func TestObsAppDetail_RejectsBadID(t *testing.T) {
	e := newObsEnv(t, api.ScopesAdminOnly, "ops@faas.dev", "ops@faas.dev")
	rec := e.do(t, "GET", "/v1/admin/obs/apps/not-a-uuid", nil, nil)
	if rec.Code != 400 {
		t.Fatalf("app detail: got status %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}

func TestObsNodeDetail_UnknownNode(t *testing.T) {
	e := newObsEnv(t, api.ScopesAdminOnly, "ops@faas.dev", "ops@faas.dev")
	rec := e.do(t, "GET", "/v1/admin/obs/nodes/missing-node/detail", nil, nil)
	if rec.Code != 404 {
		t.Fatalf("node detail: got status %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}

func TestObsNodeMutation_RequiresConfirmation(t *testing.T) {
	e := newObsEnv(t, api.ScopesAdminOnly, "ops@faas.dev", "ops@faas.dev")
	rec := e.do(t, "POST", "/v1/admin/ops/nodes/missing-node/drain", nil, nil)
	if rec.Code != 400 {
		t.Fatalf("node drain without confirmation: got status %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}

func TestProjectObsNodeApps_SeparatesLiveStates(t *testing.T) {
	now := time.Now().UTC()
	apps := []state.App{{ID: "app-1", AccountID: "acct-1", Slug: "orders", Status: state.AppActive}}
	instances := []state.Instance{
		{AppID: "app-1", State: "RUNNING", RAMMB: 128, LastRequestAt: now.Add(-time.Minute)},
		{AppID: "app-1", State: "COLD_BOOTING", RAMMB: 256, LastRequestAt: now},
		{AppID: "app-1", State: "PARKED", RAMMB: 512, LastRequestAt: now.Add(-time.Hour)},
	}
	rows := projectObsNodeApps(apps, instances)
	if len(rows) != 1 {
		t.Fatalf("node apps: got %d rows, want 1", len(rows))
	}
	if rows[0].InstancesLive != 2 || rows[0].InstancesRunning != 1 || rows[0].InstancesColdBooting != 1 {
		t.Fatalf("node app live counters: got %+v", rows[0])
	}
	if rows[0].RAMUsedMB != 400 {
		t.Fatalf("node app RAM: got %d, want 400", rows[0].RAMUsedMB)
	}
}
