// pkg/reconcile/audit_queue_controls_test.go — ADR-124 deployment
// queue-control audit emit payload pins.
//
// The four EmitDeployment* free functions
// (EmitDeploymentCancelled / EmitDeploymentReordered /
// EmitDeploymentCleared / EmitClearObsoleteDeployments) carry the
// SOC 2 CC7.2 operator-intent trail for the queue-control HTTP
// surface. These tests pin the per-kind payload shape so a future
// reviewer who "tidies up" the data map cannot silently change a
// reader's join key.
//
// The free-function shape mirrors EmitBuildEnqueued
// (pkg/reconcile/audit.go:233-254) — a single helper, typed
// parameters, no method receiver — so the test fakes also follow
// the precedent at pkg/reconcile/fakes_test.go (fakeStore +
// fakeAuditor + snapshotEvents + extractKinds).
//
// Convention: the actor is "reconcile" (per newFakeAuditor at
// pkg/reconcile/fakes_test.go:189). In production the apid path
// uses "apid" (cmd/apid/audit.go:43) — the actor is stamped by
// audit.New(..., actor) and these tests don't exercise that wire.
package reconcile

import (
	"context"
	"encoding/json"
	"sort"
	"testing"

	"github.com/onebox-faas/faas/pkg/audit"
)

// TestEmit_QueueControls_PayloadShape is the table-driven
// payload-shape pin. Each sub-case is named after the
// handler-emit site it backs (cmd/apid/handlers_queue_controls.go).
func TestEmit_QueueControls_PayloadShape(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name             string
		emit             func(ctx context.Context, a *audit.Auditor)
		wantKind         string
		wantDataContains []string
		wantFieldEquals  map[string]any
	}{
		{
			name: "Cancelled carries deployment_id + app_slug + prior_status + reason",
			emit: func(ctx context.Context, a *audit.Auditor) {
				EmitDeploymentCancelled(ctx, a, "acct-A", "dep-1", "api", "pending", "user")
			},
			wantKind: "deployment.cancelled",
			wantDataContains: []string{
				"deployment_id", "app_slug", "prior_status", "reason",
			},
			wantFieldEquals: map[string]any{
				"deployment_id": "dep-1",
				"app_slug":      "api",
				"prior_status":  "pending",
				"reason":        "user",
			},
		},
		{
			name: "Reordered carries deployment_id + app_slug + old_priority + new_priority",
			emit: func(ctx context.Context, a *audit.Auditor) {
				EmitDeploymentReordered(ctx, a, "acct-A", "dep-2", "api", 100, 0)
			},
			wantKind: "deployment.reordered",
			wantDataContains: []string{
				"deployment_id", "app_slug", "old_priority", "new_priority",
			},
			wantFieldEquals: map[string]any{
				"deployment_id": "dep-2",
				"app_slug":      "api",
				"old_priority":  float64(100), // json.Unmarshal to map[string]any gives float64 for numbers
				"new_priority":  float64(0),
			},
		},
		{
			name: "Cleared carries deployment_id + app_slug + prior_status",
			emit: func(ctx context.Context, a *audit.Auditor) {
				EmitDeploymentCleared(ctx, a, "acct-A", "dep-3", "api", "superseded")
			},
			wantKind: "deployment.cleared",
			wantDataContains: []string{
				"deployment_id", "app_slug", "prior_status",
			},
			wantFieldEquals: map[string]any{
				"deployment_id": "dep-3",
				"app_slug":      "api",
				"prior_status":  "superseded",
			},
		},
		{
			name: "ClearObsolete carries app_id + cleared_count + older_than",
			emit: func(ctx context.Context, a *audit.Auditor) {
				EmitClearObsoleteDeployments(ctx, a, "acct-A", "app-1", 7, "168h")
			},
			wantKind: "deployment.clear_obsolete",
			wantDataContains: []string{
				"app_id", "cleared_count", "older_than",
			},
			wantFieldEquals: map[string]any{
				"app_id":        "app-1",
				"cleared_count": float64(7),
				"older_than":    "168h",
			},
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			store := newFakeStore()
			a := newFakeAuditor(store)
			ctx := context.Background()

			tt.emit(ctx, a)

			events := store.snapshotEvents()
			if got, want := len(events), 1; got != want {
				t.Fatalf("event count = %d, want %d (events=%+v)", got, want, events)
			}
			ev := events[0]
			if ev.Kind != tt.wantKind {
				t.Errorf("Kind = %q, want %q", ev.Kind, tt.wantKind)
			}
			if ev.Actor != "reconcile" {
				t.Errorf("Actor = %q, want %q (stamped by audit.New)", ev.Actor, "reconcile")
			}
			if ev.Subject == nil || *ev.Subject != "acct-A" {
				t.Errorf("Subject = %v, want &acct-A", ev.Subject)
			}

			var data map[string]any
			if err := json.Unmarshal(ev.Data, &data); err != nil {
				t.Fatalf("data jsonb unmarshal: %v (raw=%s)", err, ev.Data)
			}
			for _, k := range tt.wantDataContains {
				if _, ok := data[k]; !ok {
					t.Errorf("data missing key %q (data=%v)", k, data)
				}
			}
			for k, want := range tt.wantFieldEquals {
				got, ok := data[k]
				if !ok {
					t.Errorf("data missing field %q (want=%v)", k, want)
					continue
				}
				if got != want {
					t.Errorf("data[%q] = %v (%T), want %v (%T)", k, got, got, want, want)
				}
			}
		})
	}
}

// TestEmit_QueueControls_AllKindsCovered pins that the 4 audit
// kinds land on the closed emit set AND that no extra event slips
// through (one emit per call). Without this guard a future
// "logging" change that double-emits would go unnoticed.
func TestEmit_QueueControls_AllKindsCovered(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		emit     func(ctx context.Context, a *audit.Auditor)
		wantKind string
	}{
		{"cancelled", func(ctx context.Context, a *audit.Auditor) {
			EmitDeploymentCancelled(ctx, a, "acct", "dep", "slug", "pending", "user")
		}, "deployment.cancelled"},
		{"reordered", func(ctx context.Context, a *audit.Auditor) {
			EmitDeploymentReordered(ctx, a, "acct", "dep", "slug", 100, 50)
		}, "deployment.reordered"},
		{"cleared", func(ctx context.Context, a *audit.Auditor) {
			EmitDeploymentCleared(ctx, a, "acct", "dep", "slug", "superseded")
		}, "deployment.cleared"},
		{"clear_obsolete", func(ctx context.Context, a *audit.Auditor) {
			EmitClearObsoleteDeployments(ctx, a, "acct", "app", 3, "24h")
		}, "deployment.clear_obsolete"},
	}

	allKinds := make([]string, 0, len(tests))
	for _, tt := range tests {
		store := newFakeStore()
		a := newFakeAuditor(store)
		ctx := context.Background()

		tt.emit(ctx, a)

		kinds := extractKinds(store.snapshotEvents())
		if got, want := len(kinds), 1; got != want {
			t.Fatalf("%s: event count = %d, want 1 (kinds=%v)", tt.name, got, kinds)
		}
		if kinds[0] != tt.wantKind {
			t.Errorf("%s: Kind = %q, want %q", tt.name, kinds[0], tt.wantKind)
		}
		allKinds = append(allKinds, kinds[0])
	}

	// Pin the closed set: 4 kinds, no duplicates, all present.
	sort.Strings(allKinds)
	want := []string{"deployment.cancelled", "deployment.clear_obsolete", "deployment.cleared", "deployment.reordered"}
	if !equalSlices(allKinds, want) {
		t.Errorf("closed kind set = %v, want %v", allKinds, want)
	}
}
