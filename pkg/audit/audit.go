// Package audit is the IAM-4 / ADR-035 seam that records security and
// operational events into the append-only `events` table. Both apid
// (auth/key/secret/cron.created/cron.updated/cron.deleted/app.*/domain.*)
// and schedd (state_transition/reaper_scale_down/cron.fired) wire up one
// Auditor via [New] and call [Auditor.Emit] from their success branches.
//
// The wrapper is intentionally thin: a best-effort call into
// [state.Store.AppendEvent] that logs failures and increments the
// audit-write-failure counter. A failed audit write NEVER rolls back
// the action that produced it (ADR-035 §"Failure semantics").
//
// Failure semantics (issue #278 widened this surface — see ADR-035
// for the policy rationale):
//   - json.Marshal failure on our own map[string]any is a programmer
//     bug, not a runtime concern — log Error and return. We don't
//     reach AppendEvent, so no duration observation is recorded.
//   - AppendEvent failure logs Warn, observes the latency under
//     result="failed", and increments audit_write_failures labelled
//     by the resolved subject id (or "anonymous" if subject is nil/
//     empty). The action has already returned 200 / committed by the
//     time this fires, so the audit row is observation, not source
//     of truth. Never roll back the action.
//   - AppendEvent success observes the latency under result="ok"
//     so the failure-path latency distribution is comparable to
//     the healthy-path latency distribution (issue #278 acceptance).
package audit

import (
	"context"
	"encoding/json"
	"log/slog"
	"reflect"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	oteltrace "go.opentelemetry.io/otel/trace"

	"github.com/onebox-faas/faas/pkg/auditutil"
	"github.com/onebox-faas/faas/pkg/state"
)

// Ops is the narrow counter+histogram surface Auditor needs. The
// concrete [wire.OpsMetrics] satisfies it (both daemons wire one up).
// Defined as an interface so the helper can be unit-tested with a
// stub (audit_test.go).
//
// AccountID label cardinality is bounded by the bounded-admission
// helper upstream of the counter (pkg/wire); empty input collapses to
// "anonymous" and overflow collapses to "__other__".
type Ops interface {
	AuditWriteFailures(accountID string) prometheus.Counter
	AuditWriteFailureDuration(result string) prometheus.Observer
}

// Auditor is the IAM-4 audit seam. Constructed once per daemon and
// held on the server/engine. The single Emit method is what handlers
// call.
type Auditor struct {
	actor string
	store state.Store
	log   *slog.Logger
	ops   Ops
}

// New builds an Auditor. actor is the literal value written to the
// events.actor column for every row this Auditor emits — the spec §5
// convention is <daemon-name> ("apid", "schedd"). Passing actor here
// rather than as a package-level const enforces the convention at
// the call site, so a future daemon that forgets to pass an actor
// fails to compile rather than silently writing an empty string.
//
// nil ops is allowed — Emit will skip the counter increment and the
// latency observation so unit tests can run without an OpsMetrics.
//
// Typed-nil guard: routes the ops argument through SetOps so the
// same nil-normalisation logic fires regardless of whether the
// caller constructs via New or wires later via SetOps. Without this
// routing, a `var p *wire.OpsMetrics = nil; a := audit.New(..., p, ...)`
// would leave a.ops as a typed-nil interface and the next
// .AuditWriteFailureDuration(...).Observe(...) call would panic —
// the nil-receiver guard on the concrete Counter/Observer returns
// nil and .Observe on nil dereferences. Mirrors the same routing
// pkg/eventretention.New does NOT need (it accepts Ops via a
// Params struct field and a constructor helper).
func New(store state.Store, log *slog.Logger, ops Ops, actor string) *Auditor {
	a := &Auditor{actor: actor, store: store, log: log}
	a.SetOps(ops)
	return a
}

// SetOps replaces the Ops interface after construction. Used by the
// apid server's WithOpsMetrics flow: the server is built before
// OpsMetrics (which may need to be constructed after the registry
// wiring), so the auditor starts with nil ops and gets a real one
// later. Pass nil to disable the counter/histogram path.
//
// Typed-nil guard: storing a non-nil interface wrapping a
// typed-nil pointer (e.g. SetOps(srv.ops) when srv.ops is a nil
// *wire.OpsMetrics) would panic at the next metric call — the
// nil-receiver guard on the concrete Counter/Observer returns nil
// and .Inc/.Observe on nil panics. isTypedNil normalises both
// flavours to the same nil a.ops value so the per-call guard stays
// accurate. Mirrors pkg/eventretention.Cleanup.SetOps.
func (a *Auditor) SetOps(ops Ops) {
	if isTypedNilAuditOps(ops) {
		a.ops = nil
		return
	}
	a.ops = ops
}

// isTypedNilAuditOps is the audit-side twin of
// pkg/eventretention.isTypedNil. Duplicated rather than shared so
// pkg/audit stays import-clean (pkg/audit is widely imported —
// pulling in pkg/eventretention would reverse the dep direction).
func isTypedNilAuditOps(v any) bool {
	if v == nil {
		return true
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Pointer, reflect.Chan, reflect.Func, reflect.Map, reflect.Slice, reflect.Interface:
		return rv.IsNil()
	}
	return false
}

// Emit writes one events row. accountID is optional (nil allowed for
// system-level events, e.g. cron-fired by schedd). data may be nil;
// marshal into {} on the way down so the column is always valid JSON.
//
// Issue #555 PR-5: lift the active OTel span context from ctx and
// stamp trace_id / span_id onto the data JSON so the row joins the
// in-memory trace ring on the same key. The lift is best-effort: a
// missing span context (legacy single-box without OTel) leaves the
// data unchanged.
//
// ADR-091 D20 PR-B: emit sites that want to stamp the binary
// `result` field call [EmitResult] (or wrap their data with
// auditutil.WithResult before calling Emit). Emit itself does NOT
// stamp a default result — keeps the cmd/gatewayd-internal mirror
// in lock-step (it does NOT call this method) and avoids a second
// silent wire-format change. Legacy emit sites without a meaningful
// outcome stay on Emit unchanged.
func (a *Auditor) Emit(ctx context.Context, kind string, accountID *string, data map[string]any) {
	a.emit(ctx, a.actor, kind, accountID, data, "")
}

// EmitResult is the result-bearing twin of [Emit]. result is the
// literal value written to data["result"] — "success" / "error" in
// the load-bearing case, or a finer-grained form (e.g.
// "error:code=im403") for sites that want to encode context. The
// stamp goes through [auditutil.WithResult] so a caller's explicit
// value on data["result"] always wins over the supplied result.
func (a *Auditor) EmitResult(ctx context.Context, kind string, accountID *string, data map[string]any, result string) {
	a.emit(ctx, a.actor, kind, accountID, data, result)
}

// EmitAs is the per-call-actor twin of [Emit] (issue #606 /
// SAFE-RELEASES-E.1). Most emit sites don't know who the actor is
// at the package level — they stamp the daemon-level actor that
// the constructor baked in ("apid", "schedd", …) and call it a
// day. A handful of emit sites (currently: the four deployment
// event types — app.deployed, app.signed_image_accepted,
// deployment.traffic_percent_set_on_create, deploy.local_tarball,
// deploy.source_ref) DO know who the actor is — they resolved
// the actor at request entry (cmd/apid/deploy_actor.go) and want
// to stamp it onto the audit row's actor column for SOC 2 / GDPR
// attribution. EmitAs is the explicit override for that case.
//
// actor is the literal value written to events.actor — the
// canonical shape is "<via>:<id>" (e.g. "dashboard:8a2...",
// "github:poyrazK"). See cmd/apid/deploy_actor.resolvedActorString
// for the convention. Passing actor == "" falls through to the
// constructor-baked a.actor — preserves the safe-default
// behaviour for any future emit site that wants to "use the
// baked actor explicitly" rather than implicitly. Emit sites
// that want to stamp a sentinel "unknown" should use the
// literal "<via>:unknown" form rather than relying on this
// fallback.
//
// Why a separate method rather than widening Emit: keeping the
// constructor-baked behaviour for the ~85 unchanged emit sites
// is the comment at lines 65-70 ("forget to pass an actor
// fails to compile rather than silently writing an empty
// string"). EmitAs is the explicit, opt-in escape hatch — its
// existence documents that this site has decided to attribute
// the row rather than leave it as the daemon's blanket name.
func (a *Auditor) EmitAs(ctx context.Context, actor, kind string, accountID *string, data map[string]any) {
	if actor == "" {
		actor = a.actor
	}
	a.emit(ctx, actor, kind, accountID, data, "")
}

// EmitAsResult is the result-bearing twin of [EmitAs]. Same
// per-call-actor semantics; result is forwarded to the shared
// emit body just like EmitResult's. Mirroring the Emit /
// EmitResult split keeps the trace-lift + marshal + metric-
// observation code paths in lock-step across the four entry
// points (Emit / EmitResult / EmitAs / EmitAsResult).
func (a *Auditor) EmitAsResult(ctx context.Context, actor, kind string, accountID *string, data map[string]any, result string) {
	if actor == "" {
		actor = a.actor
	}
	a.emit(ctx, actor, kind, accountID, data, result)
}

// emit is the shared body. result == "" is the legacy Emit path;
// result != "" is the EmitResult path. Single source of truth for
// the trace lift, marshal, store call, and metric observation —
// keeping the entry points in lock-step. actor is the literal
// value to stamp onto events.actor; callers pre-resolve it (Emit /
// EmitResult pass a.actor; EmitAs / EmitAsResult pass the
// per-call override).
func (a *Auditor) emit(ctx context.Context, actor, kind string, accountID *string, data map[string]any, result string) {
	data = auditutil.WithResult(data, result)
	if data == nil {
		data = map[string]any{}
	}
	if sc := oteltrace.SpanContextFromContext(ctx); sc.IsValid() {
		// Don't overwrite a customer-supplied trace_id (e.g. cron-fired
		// events that synthesise a trace_id for the row). The merge
		// falls back to the active span context only when the field
		// is absent.
		if _, ok := data["trace_id"]; !ok {
			data["trace_id"] = sc.TraceID().String()
		}
		if _, ok := data["span_id"]; !ok {
			data["span_id"] = sc.SpanID().String()
		}
	}
	payload, err := json.Marshal(data)
	if err != nil {
		a.log.Error("audit: marshal", "kind", kind, "err", err)
		return
	}
	// Normalize the subject into a string we can label the metric
	// with. nil and empty collapse to the empty string; the bounded-
	// admission helper upstream of the counter maps that to
	// "anonymous". This keeps the labelled-counter helper on a single
	// non-nil string argument so Ops stays a clean two-method
	// interface.
	subjectStr := ""
	if accountID != nil {
		subjectStr = *accountID
	}
	var subject *string
	if subjectStr != "" {
		subject = accountID
	}
	start := time.Now()
	err = a.store.AppendEvent(ctx, actor, kind, subject, payload)
	dur := time.Since(start)
	if a.ops != nil {
		if err != nil {
			a.ops.AuditWriteFailureDuration("failed").Observe(dur.Seconds())
		} else {
			a.ops.AuditWriteFailureDuration("ok").Observe(dur.Seconds())
		}
	}
	if err != nil {
		a.log.Warn("audit: append event",
			"actor", actor, "kind", kind, "subject", subject, "err", err)
		if a.ops != nil {
			a.ops.AuditWriteFailures(subjectStr).Inc()
		}
	}
}
