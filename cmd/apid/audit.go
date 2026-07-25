// auditor is the IAM-4 (ADR-035) seam that apid handlers call to
// record a security event. It is intentionally thin: a best-effort
// wrapper around state.Store.AppendEvent that logs failures and
// increments the audit_write_failures counter. Mirrors the schedd
// pattern at pkg/sched/engine.go:1317-1329 — a failed audit write
// never rolls back the action that produced it.
//
// The events table is append-only (spec §5) and shared with schedd;
// IAM-4 is the auth-event front-end on the same surface.
package main

import (
	"context"
	"encoding/json"
	"log/slog"

	"github.com/onebox-faas/faas/pkg/state"
	"github.com/prometheus/client_golang/prometheus"
)

// auditActor is the value written to events.actor for every IAM-4
// audit row. Free-form text per spec §5; schedd uses "schedd" so the
// convention is <daemon-name>.
const auditActor = "apid"

// auditOps is the narrow counter surface auditor needs. wire.OpsMetrics
// satisfies it (it has both EventsWriteFailures and AuditWriteFailures).
// Defined as an interface so the helper can be unit-tested with a stub.
type auditOps interface {
	AuditWriteFailures() prometheus.Counter
}

// auditor is the IAM-4 audit seam. Constructed once per server in
// newServerWithDeps and held as s.audit. The single Emit method is
// what handlers call.
type auditor struct {
	store state.Store
	log   *slog.Logger
	ops   auditOps
}

// newAuditor builds the helper. nil ops is allowed — Emit will skip
// the counter increment so unit tests can run without an OpsMetrics.
func newAuditor(store state.Store, log *slog.Logger, ops auditOps) *auditor {
	return &auditor{store: store, log: log, ops: ops}
}

// Emit writes one events row. accountID is optional (nil allowed for
// system-level events, e.g. cron-fired). data may be nil; marshal into
// {} on the way down so the column is always valid JSON.
//
// Failure semantics: a json.Marshal failure on our own map[string]any
// is a programmer bug, not a runtime concern — log Error and return.
// An AppendEvent failure logs Warn and increments the
// audit_write_failures counter; the auth action has already returned
// 200 by the time this fires, so the audit row is observation, not
// source of truth. Never roll back the action.
func (a *auditor) Emit(ctx context.Context, kind string, accountID *string, data map[string]any) {
	if data == nil {
		data = map[string]any{}
	}
	payload, err := json.Marshal(data)
	if err != nil {
		a.log.Error("audit: marshal", "kind", kind, "err", err)
		return
	}
	var subject *string
	if accountID != nil && *accountID != "" {
		subject = accountID
	}
	if err := a.store.AppendEvent(ctx, auditActor, kind, subject, payload); err != nil {
		a.log.Warn("audit: append event",
			"kind", kind, "subject", subject, "err", err)
		if a.ops != nil {
			a.ops.AuditWriteFailures().Inc()
		}
	}
}
