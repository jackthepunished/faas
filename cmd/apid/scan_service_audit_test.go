package main

// scan_service_audit_test.go — unit tests for the ADR-124 ship-
// blocker #2 audit emission (emitWorkloadSkippedRow +
// KindWorkloadSkipped). The audit row is the durable SOC 2
// CC7.2 paper trail the operator needs to answer "who deployed
// v3 and what did they skip?" — slog alone isn't auditable.
// These tests pin:
//
//  1. kind = project.workload.skipped
//  2. actor pass-through (resolvedActorString format)
//  3. payload shape: {project_id, app_id, workload_name, reason,
//     commit_sha}
//  4. nil-auditor safety (matches cmd/apid/audit.go:316-317)
//  5. per-row independence (loop over partition.Skipped fires
//     one row per entry, not coalesced)

import (
	"context"
	"sync"
	"testing"

	"github.com/onebox-faas/faas/pkg/reconcile"
)

// skippedStubAudit satisfies auditEmitterAs. Records every EmitAs
// call in order so tests can assert per-row emission, kind, and
// payload. Concurrency-safe (the production EmitAs is invoked
// from a single goroutine per request today, but a future batch
// path might emit concurrently — the stub mirrors that possibility
// without committing to it).
type skippedStubAudit struct {
	mu    sync.Mutex
	calls []skippedAuditCall
}

type skippedAuditCall struct {
	Actor     string
	Kind      string
	AccountID *string
	Data      map[string]any
}

func (s *skippedStubAudit) EmitAs(_ context.Context, actor, kind string, accountID *string, data map[string]any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Defensive copy of data so the test's later mutations don't
	// bleed into the recorded call.
	cp := make(map[string]any, len(data))
	for k, v := range data {
		cp[k] = v
	}
	s.calls = append(s.calls, skippedAuditCall{
		Actor:     actor,
		Kind:      kind,
		AccountID: accountID,
		Data:      cp,
	})
}

// TestEmitWorkloadSkippedRow_KindAndPayload pins the audit row's
// kind string + payload keys + values. A drift in any of these
// breaks the dashboard's audit-events filter ("show me all
// project.workload.skipped rows for project X") and SOC 2 CC7.2
// reporting (the audit-events table group-by joins on kind +
// project_id + workload_name).
func TestEmitWorkloadSkippedRow_KindAndPayload(t *testing.T) {
	stub := &skippedStubAudit{}
	emitWorkloadSkippedRow(
		context.Background(),
		stub,
		"dashboard:8a2f3a2e-1111-2222-3333-444455556666",
		"acct-uuid-1111",
		"proj-uuid-aaaa",
		"app-uuid-bbbb",
		"checkout-api",
		"abcdef0123456789",
	)

	if got := len(stub.calls); got != 1 {
		t.Fatalf("calls: got %d, want 1", got)
	}
	call := stub.calls[0]

	if call.Kind != reconcile.KindWorkloadSkipped {
		t.Errorf("kind: got %q, want %q", call.Kind, reconcile.KindWorkloadSkipped)
	}
	if call.Kind != "project.workload.skipped" {
		t.Errorf("kind literal drift: got %q, want %q (any drift breaks the audit-events group-by)", call.Kind, "project.workload.skipped")
	}
	if call.Actor != "dashboard:8a2f3a2e-1111-2222-3333-444455556666" {
		t.Errorf("actor pass-through: got %q, want %q", call.Actor, "dashboard:8a2f3a2e-1111-2222-3333-444455556666")
	}
	if call.AccountID == nil || *call.AccountID != "acct-uuid-1111" {
		t.Errorf("accountID: got %v, want pointer to acct-uuid-1111", call.AccountID)
	}

	// Payload shape pin — the dashboard joins on these keys.
	wantKeys := map[string]any{
		"project_id":    "proj-uuid-aaaa",
		"app_id":        "app-uuid-bbbb",
		"workload_name": "checkout-api",
		"reason":        "unchanged via exclude",
		"commit_sha":    "abcdef0123456789",
	}
	for k, want := range wantKeys {
		got, ok := call.Data[k]
		if !ok {
			t.Errorf("payload missing key %q", k)
			continue
		}
		if got != want {
			t.Errorf("payload[%q]: got %v, want %v", k, got, want)
		}
	}

	// Negative: no unexpected keys leak (mirrors PR #984's
	// annotation-merge "omit when zero" rule — the closed-set
	// shape is part of the contract).
	for k := range call.Data {
		if _, expected := wantKeys[k]; !expected {
			t.Errorf("unexpected payload key %q (drift in audit shape breaks dashboard joins)", k)
		}
	}
}

// TestEmitWorkloadSkippedRow_NilAuditorIsSafe pins the
// nil-receiver safety contract (matches cmd/apid/audit.go:316-317
// on *auditor.EmitAs). A nil auditor must not panic — the emit
// path runs on every preview that includes a --exclude entry, and
// tests that build a server without an audit handle must still
// pass.
func TestEmitWorkloadSkippedRow_NilAuditorIsSafe(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("nil auditor panicked: %v", r)
		}
	}()
	emitWorkloadSkippedRow(
		context.Background(),
		nil,
		"dashboard:any",
		"acct",
		"proj",
		"app",
		"workload",
		"sha",
	)
}

// TestEmitWorkloadSkippedRow_PerRowIndependence pins the
// per-row-emission invariant: looping over N partition.Skipped
// entries emits N audit rows, not one coalesced row. A refactor
// that coalesces (e.g. "one row with skipped_workloads[]") would
// break the dashboard's per-row event timeline + the SOC 2
// "show me every skip decision" filter.
func TestEmitWorkloadSkippedRow_PerRowIndependence(t *testing.T) {
	stub := &skippedStubAudit{}
	cases := []struct {
		workloadName string
		appID        string
		commitSHA    string
	}{
		{"checkout-api", "app-a", "sha-a"},
		{"checkout-web", "app-b", "sha-b"},
		{"inventory", "app-c", "sha-c"},
	}
	for _, tc := range cases {
		emitWorkloadSkippedRow(
			context.Background(),
			stub,
			"dashboard:user-x",
			"acct-1",
			"proj-1",
			tc.appID,
			tc.workloadName,
			tc.commitSHA,
		)
	}

	if got := len(stub.calls); got != len(cases) {
		t.Fatalf("calls: got %d, want %d (per-row independence broken)", got, len(cases))
	}
	for i, call := range stub.calls {
		if call.Data["workload_name"] != cases[i].workloadName {
			t.Errorf("call %d workload_name: got %v, want %v", i, call.Data["workload_name"], cases[i].workloadName)
		}
		if call.Data["app_id"] != cases[i].appID {
			t.Errorf("call %d app_id: got %v, want %v", i, call.Data["app_id"], cases[i].appID)
		}
		if call.Data["commit_sha"] != cases[i].commitSHA {
			t.Errorf("call %d commit_sha: got %v, want %v", i, call.Data["commit_sha"], cases[i].commitSHA)
		}
	}
}

// TestEmitWorkloadSkippedRow_KindConstIsStable pins the
// pkg/reconcile.KindWorkloadSkipped constant — a stray string at
// a future call site would silently fork the kind namespace and
// the dashboard's group-by would miss the row. The constant is
// the source of truth.
func TestEmitWorkloadSkippedRow_KindConstIsStable(t *testing.T) {
	if reconcile.KindWorkloadSkipped == "" {
		t.Fatal("KindWorkloadSkipped must be non-empty (typed const prevents fork)")
	}
	// Closed-set membership in the project.workload.<verb> shape.
	wantPrefix := "project.workload."
	if got := reconcile.KindWorkloadSkipped; len(got) <= len(wantPrefix) || got[:len(wantPrefix)] != wantPrefix {
		t.Errorf("KindWorkloadSkipped %q does not start with %q (closed-set shape broken)", got, wantPrefix)
	}
}
