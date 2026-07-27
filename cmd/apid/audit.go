// Thin in-package wrapper around pkg/audit.Auditor that bakes in the
// "apid" actor constant and the auditOps interface.
//
// The pkg/audit package is the canonical best-effort AppendEvent
// wrapper (ADR-035); apid and schedd both use it. This file exists so
// the existing `s.audit.Emit(...)` callsites in handlers don't churn
// across the cross-daemon lift — the shape stays exactly the same,
// only the underlying implementation moved to pkg/audit.
//
// Lift rationale: pkg/audit/audit.go holds the single best-effort
// wrapper (ADR-035). Lifting lets schedd reuse it for cron.fired
// without duplicating the failure contract. The apid wrapper keeps
// the auditActor const baked in so the 10+ emit sites added in
// PR #349 stay unchanged.
package main

import (
	"context"
	"log/slog"

	"github.com/onebox-faas/faas/pkg/audit"
	"github.com/onebox-faas/faas/pkg/state"
)

// auditActor is the value written to events.actor for every IAM-4
// audit row produced by apid. Free-form text per spec §5; schedd uses
// "schedd" so the convention is <daemon-name>. Baked into the
// wrapper here so the existing `s.audit.Emit(...)` callsites stay
// pre-PR shaped.
const auditActor = "apid"

// auditor wraps *pkg/audit.Auditor so the in-package callsites keep
// their pre-PR shape (s.audit.Emit(ctx, kind, &acct.ID, data)) while
// the underlying implementation is the shared pkg/audit seam.
type auditor struct {
	inner *audit.Auditor
}

func newAuditor(store state.Store, log *slog.Logger, ops audit.Ops) *auditor {
	return &auditor{inner: audit.New(store, log, ops, auditActor)}
}

// setOps forwards to the inner Auditor so WithOpsMetrics can re-bind
// the counter after construction. Mirrors the pre-PR s.audit.ops =
// ops line at server.go:187.
func (a *auditor) setOps(ops audit.Ops) {
	a.inner.SetOps(ops)
}

// Emit delegates to pkg/audit.Auditor.Emit. Same signature, same
// best-effort semantics — see pkg/audit/audit.go for the failure
// contract (ADR-035).
func (a *auditor) Emit(ctx context.Context, kind string, accountID *string, data map[string]any) {
	a.inner.Emit(ctx, kind, accountID, data)
}