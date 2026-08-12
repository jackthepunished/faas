// gatewayd audit emitter (issue #294, ADR-042).
//
// Mirrors cmd/apid/audit.go (best-effort AppendEvent) so the GitHub
// proxy can emit webhook.replay_rejected rows without dragging in
// the apid auditor. The append is best-effort per ADR-035: a
// failure logs Warn and the mutation is never rolled back.
//
// gatewayd does not own the events table; it shares the apid
// writer (the events table is a single append-only stream with no
// per-daemon partitioning). Subject for gatewayd rows is nil
// (the proxy does not have an account id at the edge) — the
// `data` payload carries the provider + delivery_id instead so a
// dashboard filter can scope by kind_prefix=webhook.*.
package main

import (
	"context"
	"encoding/json"
	"log/slog"

	"github.com/onebox-faas/faas/pkg/auditutil"
	"github.com/onebox-faas/faas/pkg/state"
)

// auditActor is the fixed actor the gatewayd audit emitter stamps
// on every event row. Mirrors the "apid" constant in
// cmd/apid/audit.go:25 — per ADR-035 each daemon has its own
// bounded actor namespace so a dashboard filter doesn't have to
// special-case.
const auditActor = "gatewayd"

// auditStore is the slice of state.Store the gatewayd audit
// emitter needs. Pinning the interface keeps the audit seam free
// of the full state.Store surface (so tests can inject a tiny
// fake that counts AppendEvent calls).
type auditStore interface {
	AppendEvent(ctx context.Context, actor, kind string, subject *string, data []byte) error
}

// gatewaydAuditor emits webhook.* (and any future gatewayd-scope)
// audit events. Best-effort per ADR-035.
type gatewaydAuditor struct {
	store auditStore
	log   *slog.Logger
}

// newGatewaydAuditor builds the auditor. log must be non-nil so
// best-effort failures have somewhere to land.
func newGatewaydAuditor(store auditStore, log *slog.Logger) *gatewaydAuditor {
	return &gatewaydAuditor{store: store, log: log}
}

// Emit writes an event row. Failures are logged at WARN and
// swallowed — the proxy must not roll back a webhook forward
// because the audit row failed to land. Matches the apid Emit
// semantics at cmd/apid/audit.go:79.
//
// ADR-091 D20 PR-B: this method does NOT stamp a `result` field.
// Call sites that want to record an outcome use EmitResult; legacy
// sites without a meaningful outcome (e.g. webhook.forwarded) stay
// on Emit. Mirrors pkg/audit.Auditor.Emit / EmitResult.
func (a *gatewaydAuditor) Emit(ctx context.Context, kind string, subject *string, data map[string]any) {
	a.emit(ctx, kind, subject, data, "")
}

// EmitResult is the result-bearing twin of [Emit]. result is the
// literal value written to data["result"] — "success" / "error"
// in the load-bearing case (edge_rule.* audit rows) or a finer
// form for sites that encode context. The stamp goes through
// [auditutil.WithResult] so a caller's explicit value on
// data["result"] always wins.
func (a *gatewaydAuditor) EmitResult(ctx context.Context, kind string, subject *string, data map[string]any, result string) {
	a.emit(ctx, kind, subject, data, result)
}

// emit is the shared body. result == "" is the legacy Emit path;
// result != "" is the EmitResult path. Single source of truth for
// marshal + store call so the two entry points stay in lock-step.
func (a *gatewaydAuditor) emit(ctx context.Context, kind string, subject *string, data map[string]any, result string) {
	if a == nil {
		return
	}
	data = auditutil.WithResult(data, result)
	payload, err := json.Marshal(data)
	if err != nil {
		if a.log != nil {
			a.log.Warn("gatewayd audit marshal failed", "kind", kind, "err", err)
		}
		return
	}
	if err := a.store.AppendEvent(ctx, auditActor, kind, subject, payload); err != nil {
		if a.log != nil {
			a.log.Warn("gatewayd audit emit failed", "kind", kind, "err", err)
		}
	}
}

// Compile-time check: *state.PgStore satisfies auditStore so the
// production wiring in cmd/gatewayd-internal/main.go can pass a pgStore
// directly without an adapter. The check fails to compile if
// state.Store.AppendEvent drifts.
var _ auditStore = (*state.PgStore)(nil)
