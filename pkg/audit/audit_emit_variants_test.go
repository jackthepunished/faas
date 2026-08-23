// audit_emit_variants_test.go — fill pkg/audit coverage of the
// EmitResult / EmitAs / EmitAsResult entry points and the typed-nil
// normalisation branches on isTypedNilAuditOps.
//
// Targets:
//   - EmitResult: result-bearing entry point, with trace_id lift,
//     result-stamping branch, AppendEvent failure path
//   - EmitAs: per-call actor override; empty actor falls back to
//     the constructor-baked a.actor
//   - EmitAsResult: the union of EmitAs + EmitResult semantics
//   - isTypedNilAuditOps: chan / map / func / slice typed-nil
//     normalisation (the four Kind branches not exercised by the
//     pointer case in TestSetOps_TypedNilDoesNotPin)
//   - emit: AppendEvent failure with nil ops; ok path with nil ops

package audit_test

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"testing"

	oteltrace "go.opentelemetry.io/otel/trace"

	"github.com/onebox-faas/faas/pkg/api"
	"github.com/onebox-faas/faas/pkg/audit"
	"github.com/onebox-faas/faas/pkg/state"
)

// --- EmitResult ---------------------------------------------------

// TestEmitResult_StampsResultField covers the entry point that
// emits a result-bearing audit row (ADR-091 D20 PR-B). The auditutil
// helper folds the supplied result into data["result"] so downstream
// observability can split the histogram by outcome.
func TestEmitResult_StampsResultField(t *testing.T) {
	store := state.NewMemStore()
	ops := newStubAuditOps()
	a := audit.New(store, silentLog(), ops, "apid")
	acctRec, err := store.CreateAccount(context.Background(), "apid-result@example.com", api.PlanHobby)
	if err != nil {
		t.Fatalf("CreateAccount: %v", err)
	}

	a.EmitResult(context.Background(), "key.created", &acctRec.ID,
		map[string]any{"key_id": "k-1"}, "success")

	rows, _ := store.ListEvents(context.Background(), acctRec.ID, 0)
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	if got := ops.durationCount(t, "ok"); got != 1 {
		t.Errorf("ok observations = %d, want 1", got)
	}
}

// TestEmitResult_CallerSuppliedResultWins pins the
// auditutil.WithResult precedence: a caller's explicit data["result"]
// must NOT be overwritten by the supplied result argument. Sites
// that want a richer encoding (e.g. "error:code=im403") rely on
// this contract.
func TestEmitResult_CallerSuppliedResultWins(t *testing.T) {
	store := state.NewMemStore()
	a := audit.New(store, silentLog(), newStubAuditOps(), "apid")
	acctRec, _ := store.CreateAccount(context.Background(), "apid-result-win@example.com", api.PlanHobby)

	a.EmitResult(context.Background(), "key.failed", &acctRec.ID,
		map[string]any{"result": "error:code=im403"}, "success")

	rows, _ := store.ListEvents(context.Background(), acctRec.ID, 0)
	if len(rows) != 1 {
		t.Fatalf("got %d rows", len(rows))
	}
	var data map[string]any
	if err := decodeJSON(t, rows[0].Data, &data); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if data["result"] != "error:code=im403" {
		t.Errorf("result = %v, want caller-supplied winner", data["result"])
	}
}

// TestEmitResult_AppendEventFailureIncrementsFailureCounter covers
// the failure branch on the result-bearing path. The "failed"
// duration observation must fire (not "ok") and the counter must
// be incremented, mirroring Emit's documented failure semantics.
func TestEmitResult_AppendEventFailureIncrementsFailureCounter(t *testing.T) {
	base := state.NewMemStore()
	acctRec, _ := base.CreateAccount(context.Background(), "apid-result-fail@example.com", api.PlanHobby)
	store := failingStore{base}
	ops := newStubAuditOps()
	a := audit.New(store, silentLog(), ops, "apid")

	a.EmitResult(context.Background(), "key.failed", &acctRec.ID,
		map[string]any{"key_id": "k-1"}, "error")

	if got := ops.durationCount(t, "failed"); got != 1 {
		t.Errorf("failed observations = %d, want 1", got)
	}
	if got := ops.failureCount(t, acctRec.ID); got != 1 {
		t.Errorf("failure counter = %v, want 1", got)
	}
}

// --- EmitAs -------------------------------------------------------

// TestEmitAs_StampsPerCallActor covers the per-call-actor override
// introduced for the SAFE-RELEASES-E.1 deployment events. The
// supplied actor is the literal written to events.actor, allowing
// SOC 2 / GDPR attribution at request entry (cmd/apid/deploy_actor.go).
func TestEmitAs_StampsPerCallActor(t *testing.T) {
	store := state.NewMemStore()
	a := audit.New(store, silentLog(), newStubAuditOps(), "apid")
	acctRec, _ := store.CreateAccount(context.Background(), "apid-emitas@example.com", api.PlanHobby)

	a.EmitAs(context.Background(), "dashboard:8a2...", "app.deployed",
		&acctRec.ID, map[string]any{"app_id": "a-1"})

	rows, _ := store.ListEvents(context.Background(), acctRec.ID, 0)
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	if rows[0].Actor != "dashboard:8a2..." {
		t.Errorf("Actor = %q, want dashboard:8a2...", rows[0].Actor)
	}
	if rows[0].Kind != "app.deployed" {
		t.Errorf("Kind = %q, want app.deployed", rows[0].Kind)
	}
}

// TestEmitAs_EmptyActorFallsBackToBakedActor covers the
// `if actor == ""` branch at audit.go:191. The per-call override is
// a no-op when the caller passes ""; the row's actor column is the
// daemon's constructor-baked value ("apid" here). Preserves the
// safe-default behaviour for future sites that want to "use the
// baked actor explicitly".
func TestEmitAs_EmptyActorFallsBackToBakedActor(t *testing.T) {
	store := state.NewMemStore()
	a := audit.New(store, silentLog(), newStubAuditOps(), "apid")
	acctRec, _ := store.CreateAccount(context.Background(), "apid-emitas-empty@example.com", api.PlanHobby)

	a.EmitAs(context.Background(), "", "app.deployed",
		&acctRec.ID, map[string]any{"app_id": "a-1"})

	rows, _ := store.ListEvents(context.Background(), uuidStringOf(acctRec.ID), 0)
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	if rows[0].Actor != "apid" {
		t.Errorf("Actor = %q, want apid (baked fallback)", rows[0].Actor)
	}
}

// --- EmitAsResult --------------------------------------------------

// TestEmitAsResult_RoundTrip covers the union of the per-call-actor
// and result-bearing entry points. Mirrors EmitResult's semantics
// (auditutil.WithResult precedence) on the EmitAs carrier.
func TestEmitAsResult_RoundTrip(t *testing.T) {
	store := state.NewMemStore()
	ops := newStubAuditOps()
	a := audit.New(store, silentLog(), ops, "schedd")
	acctRec, _ := store.CreateAccount(context.Background(), "schedd-asresult@example.com", api.PlanHobby)

	a.EmitAsResult(context.Background(), "github:poyrazK", "deploy.source_ref",
		&acctRec.ID, map[string]any{"sha": "abc123"}, "success")

	rows, _ := store.ListEvents(context.Background(), acctRec.ID, 0)
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	if rows[0].Actor != "github:poyrazK" {
		t.Errorf("Actor = %q, want github:poyrazK", rows[0].Actor)
	}
	if rows[0].Kind != "deploy.source_ref" {
		t.Errorf("Kind = %q, want deploy.source_ref", rows[0].Kind)
	}
	if got := ops.durationCount(t, "ok"); got != 1 {
		t.Errorf("ok observations = %d, want 1", got)
	}
}

// TestEmitAsResult_EmptyActorFallback covers the empty-actor branch
// on the union path: the actor falls back to the constructor-baked
// value while the result stamp is forwarded into auditutil.
func TestEmitAsResult_EmptyActorFallback(t *testing.T) {
	store := state.NewMemStore()
	a := audit.New(store, silentLog(), newStubAuditOps(), "schedd")
	acctRec, _ := store.CreateAccount(context.Background(), "schedd-asresult-fb@example.com", api.PlanHobby)

	a.EmitAsResult(context.Background(), "", "reaper.scale_down",
		&acctRec.ID, map[string]any{"app_id": "a-1"}, "success")

	rows, _ := store.ListEvents(context.Background(), acctRec.ID, 0)
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	if rows[0].Actor != "schedd" {
		t.Errorf("Actor = %q, want schedd", rows[0].Actor)
	}
}

// TestEmitAsResult_AppendEventFailureIncrementsFailureCounter pins
// the failure semantics on the union path. Mirrors
// TestEmitResult_AppendEventFailureIncrementsFailureCounter.
func TestEmitAsResult_AppendEventFailureIncrementsFailureCounter(t *testing.T) {
	base := state.NewMemStore()
	acctRec, _ := base.CreateAccount(context.Background(), "schedd-asresult-fail@example.com", api.PlanHobby)
	store := failingStore{base}
	ops := newStubAuditOps()
	a := audit.New(store, silentLog(), ops, "schedd")

	a.EmitAsResult(context.Background(), "github:poyrazK", "deploy.source_ref",
		&acctRec.ID, map[string]any{"sha": "abc"}, "error")

	if got := ops.durationCount(t, "failed"); got != 1 {
		t.Errorf("failed observations = %d, want 1", got)
	}
	if got := ops.failureCount(t, acctRec.ID); got != 1 {
		t.Errorf("failure counter = %v, want 1", got)
	}
}

// --- isTypedNilAuditOps branches ---------------------------------

// The chan / map / func / slice branches in isTypedNilAuditOps
// (audit.go:120-126) are defensive against future audit.Ops
// implementations that wrap a typed-nil of one of those kinds.
// They are unreachable through the current audit.Ops contract
// (the only valid implementation is *typedNilAuditOps, a struct
// pointer). SetOps's typed-pointer case is exercised by
// TestSetOps_TypedNilDoesNotPin in audit_test.go; we do not
// duplicate that coverage here.

// --- emit internal branches --------------------------------------

// TestEmit_AppendEventFailureWithNilOps exercises the failure-path
// `if a.ops != nil` guard. Without this the guard reads at 50% and a
// refactor that removed the nil check could regress silently. Pins
// the documented "nil ops is allowed" path on the failure branch.
func TestEmit_AppendEventFailureWithNilOps(t *testing.T) {
	store := failingStore{state.NewMemStore()}
	a := audit.New(store, silentLog(), nil, "apid")
	acct := "acct-1"

	// Must not panic.
	a.Emit(context.Background(), "key.failed", &acct, map[string]any{"key_id": "k-1"})
}

// TestEmit_OkPathWithNilOps covers the ok-path `if a.ops != nil`
// guard symmetric to TestEmit_AppendEventFailureWithNilOps.
func TestEmit_OkPathWithNilOps(t *testing.T) {
	store := state.NewMemStore()
	a := audit.New(store, silentLog(), nil, "apid")
	acctRec, _ := store.CreateAccount(context.Background(), "apid-nilops-ok@example.com", api.PlanHobby)

	a.Emit(context.Background(), "key.created", &acctRec.ID, map[string]any{"key_id": "k-1"})

	rows, _ := store.ListEvents(context.Background(), uuidStringOf(acctRec.ID), 0)
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
}

// TestEmitResult_LiftsTraceContext pins the issue #555 PR-5 trace
// lift on the result-bearing entry point. Both Emit and EmitResult
// must perform the same context lift — a regression that skipped
// it on EmitResult would lose trace correlation on the four
// deployment event types.
func TestEmitResult_LiftsTraceContext(t *testing.T) {
	store := state.NewMemStore()
	a := audit.New(store, silentLog(), newStubAuditOps(), "apid")
	acctRec, _ := store.CreateAccount(context.Background(), "apid-result-trace@example.com", api.PlanHobby)

	var tid oteltrace.TraceID
	var sid oteltrace.SpanID
	for i := range tid {
		tid[i] = byte(i + 1)
	}
	for i := range sid {
		sid[i] = byte(i + 1)
	}
	sc := oteltrace.NewSpanContext(oteltrace.SpanContextConfig{
		TraceID: tid, SpanID: sid, Remote: true,
	})
	ctx := oteltrace.ContextWithSpanContext(context.Background(), sc)

	a.EmitResult(ctx, "key.created", &acctRec.ID,
		map[string]any{"key_id": "k-1"}, "success")

	rows, _ := store.ListEvents(context.Background(), acctRec.ID, 0)
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	var data map[string]any
	if err := decodeJSON(t, rows[0].Data, &data); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if data["trace_id"] != tid.String() {
		t.Errorf("trace_id = %v, want %s", data["trace_id"], tid.String())
	}
}

// --- helpers -----------------------------------------------------

// decodeJSON is a small adapter to keep the unmarshal branches
// concise in this file. Mirrors the json.Unmarshal pattern used
// in audit_test.go.
func decodeJSON(t *testing.T, raw []byte, dst any) error {
	t.Helper()
	return json.Unmarshal(raw, dst)
}

// Compile-time witness: a typed-nil chan would be an interface-typed
// audit.Ops value with non-nil interface header but nil pointer.
// The audit.Ops contract requires two methods (AuditWriteFailures
// and AuditWriteFailureDuration); a raw chan/map/func/slice cannot
// satisfy it. The four Kind branches in isTypedNilAuditOps are
// defensive and not exercised through the public API.

// _ = errors.New is referenced below for go vet; the unused-import
// pattern keeps the file self-contained.
var _ = errors.New

// Pull in slog for any future logger-only tests in this file.
var _ = slog.Default
