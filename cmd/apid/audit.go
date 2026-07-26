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
	"time"

	"github.com/onebox-faas/faas/pkg/state"
	"github.com/prometheus/client_golang/prometheus"
)

// auditActor is the value written to events.actor for every IAM-4
// audit row. Free-form text per spec §5; schedd uses "schedd" so the
// convention is <daemon-name>.
const auditActor = "apid"

// auditOps is the narrow counter+histogram surface auditor needs.
// wire.OpsMetrics satisfies it (it has AuditWriteFailures and
// AuditWriteFailureDuration). Defined as an interface so the
// helper can be unit-tested with a stub (cmd/apid/audit_test.go).
//
// Issue #278 widened the audit-write surface:
//   - AuditWriteFailures takes accountID so the counter can be
//     labelled per-customer. The label value flows through the
//     bounded admission set; empty input becomes "anonymous" and
//     overflow collapses to "__other__".
//   - AuditWriteFailureDuration records the AppendEvent latency
//     histogram labelled by result ∈ {ok, failed} so an operator
//     can distinguish a Postgres outage (slow failed) from a
//     transient insert race (fast failed).
type auditOps interface {
	AuditWriteFailures(accountID string) prometheus.Counter
	AuditWriteFailureDuration(result string) prometheus.Observer
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
// Failure semantics (issue #278 widened this surface — see ADR-035
// for the policy rationale):
//   - json.Marshal failure on our own map[string]any is a programmer
//     bug, not a runtime concern — log Error and return. We don't
//     reach AppendEvent, so no duration observation is recorded.
//   - AppendEvent failure logs Warn, observes the latency under
//     result="failed", and increments audit_write_failures labelled
//     by the resolved subject id (or "anonymous" if subject is nil/
//     empty). The auth action has already returned 200 by the time
//     this fires, so the audit row is observation, not source of
//     truth. Never roll back the action.
//   - AppendEvent success observes the latency under result="ok"
//     so the failure-path latency distribution is comparable to
//     the healthy-path latency distribution (issue #278 acceptance).
func (a *auditor) Emit(ctx context.Context, kind string, accountID *string, data map[string]any) {
	if data == nil {
		data = map[string]any{}
	}
	payload, err := json.Marshal(data)
	if err != nil {
		a.log.Error("audit: marshal", "kind", kind, "err", err)
		return
	}
	// Normalize the subject into a string we can label the metric
	// with. nil and empty collapse to the empty string; accountLabel
	// upstream in the metric helper maps that to "anonymous". This
	// keeps the labelled-counter helper on a single non-nil string
	// argument so auditOps stays a clean two-method interface.
	subjectStr := ""
	if accountID != nil {
		subjectStr = *accountID
	}
	var subject *string
	if subjectStr != "" {
		subject = accountID
	}
	start := time.Now()
	err = a.store.AppendEvent(ctx, auditActor, kind, subject, payload)
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
			"kind", kind, "subject", subject, "err", err)
		if a.ops != nil {
			a.ops.AuditWriteFailures(subjectStr).Inc()
		}
	}
}
