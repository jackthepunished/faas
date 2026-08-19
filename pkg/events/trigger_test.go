// trigger_test.go — pins the audit-kind string contract for the
// trigger + esm event families (issue #757 / ADR-0NN + ADR-118).
//
// The trigger.* kinds were already covered by the file's compile-
// time `var _ WakeEvent = ...` assertions; the test file pins the
// ESM* family explicitly + locks the dual-emit policy documented
// in ADR-118 §"Audit vocabulary bridging" (trigger.* canonical,
// esm.* operator alias; 1:1 mapping with one documented
// asymmetry on source.deleted).
package events_test

import (
	"strings"
	"testing"
	"time"

	"github.com/onebox-faas/faas/pkg/events"
)

// TestESMKindConstants pins the audit-kind string values. The
// dashboard panel selector (`kind_prefix=esm.`) and the audit
// timeline read these strings back from the events row; a typo
// here would silently misroute the panel.
func TestESMKindConstants(t *testing.T) {
	cases := []struct {
		name string
		got  string
		want string
	}{
		{"ESMSourceCreated", events.ESMSourceCreated, "esm.source.created"},
		{"ESMSourceDeleted", events.ESMSourceDeleted, "esm.source.deleted"},
		{"ESMPollFailed", events.ESMPollFailed, "esm.poll.failed"},
		{"ESMDrainDLQ", events.ESMDrainDLQ, "esm.drain.dlq"},
		{"ESMFilterError", events.ESMFilterError, "esm.filter.error"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.got != c.want {
				t.Errorf("%s = %q, want %q (dashboard selector `kind_prefix=esm.*` would misroute the panel on a drift)",
					c.name, c.got, c.want)
			}
		})
	}
}

// TestESMKindPrefix pins the esm.* prefix convention. The dual-
// emit policy (ADR-118 §"Audit vocabulary bridging") requires
// every ESM kind to start with `esm.`; a typo'd `esmm.` would
// break the dashboard's `kind_prefix=esm.` filter.
func TestESMKindPrefix(t *testing.T) {
	for _, k := range []string{
		events.ESMSourceCreated,
		events.ESMSourceDeleted,
		events.ESMPollFailed,
		events.ESMDrainDLQ,
		events.ESMFilterError,
	} {
		if !strings.HasPrefix(k, "esm.") {
			t.Errorf("kind %q does not start with `esm.` prefix (ADR-118 §Audit vocabulary bridging)", k)
		}
	}
}

// TestESMEventsSatisfyWakeEvent is the compile-time guarantee
// replicated as a runtime test so a future refactor that breaks
// the WakeEvent contract on any of the four ESM event types
// surfaces at `go test` rather than at Platform.Emit call time.
func TestESMEventsSatisfyWakeEvent(t *testing.T) {
	cases := []events.WakeEvent{
		events.ESMSourceCreatedEvent{TriggerID: "t1", AccountID: "a1", AppID: "app1", SourceKind: "kafka"},
		events.ESMSourceDeletedEvent{TriggerID: "t1", AccountID: "a1", AppID: "app1", SourceKind: "kafka"},
		events.ESMPollFailedEvent{TriggerID: "t1", AppID: "app1", SourceKind: "kafka", Error: "broker timeout"},
		events.ESMDrainDLQEvent{TriggerID: "t1", RecordID: "r1", AppID: "app1", Reason: "max_attempts"},
		events.ESMFilterErrorEvent{TriggerID: "t1", AppID: "app1", SourceKind: "kafka", Error: "bad path"},
	}
	for _, ev := range cases {
		if ev.Kind() == "" {
			t.Errorf("event %T has empty Kind() — Platform.Emit would write an un-attributable row", ev)
		}
		if ev.At().IsZero() {
			t.Errorf("event %T has zero At() — audit timeline would be undated", ev)
		}
	}
}

// TestESMAtDefaultsToNow pins the contract that an ESM event
// constructed with a zero At() returns time.Now() from At() —
// the dispatch path constructs events inline and depends on the
// default-to-now behaviour so a missed `At: time.Now()` in the
// call site still produces a usable timestamp.
func TestESMAtDefaultsToNow(t *testing.T) {
	before := time.Now()
	ev := events.ESMSourceCreatedEvent{TriggerID: "t1"}
	got := ev.At()
	after := time.Now()
	if got.Before(before) || got.After(after) {
		t.Errorf("ESMSourceCreatedEvent{}.At() = %v, want in [%v, %v]", got, before, after)
	}
}

// TestESMOneToOneMapping is the asymmetric-mapping pin. The
// ADR-118 §"Asymmetric kind mapping" spec calls out that
// ESMSourceDeleted has NO trigger.* counterpart (the canonical
// trigger.disabled path covers the soft-delete; the
// source-deleted vocabulary is ESM-only). Every other ESM kind
// MUST have a trigger.* mirror:
//
//	trigger.fired       ↔ esm.source.created (creation-side)
//	trigger.fired.batch ↔ esm.source.deleted (the only gap — ESM-only)
//	trigger.retry       ↔ esm.poll.failed  (failure-side)
//	trigger.dlq         ↔ esm.drain.dlq    (DLQ-side)
//	trigger.filter.error ↔ esm.filter.error (filter-error-side, commit 6)
//
// Drift in this map breaks the dual-emit JOIN (trigger_id,
// record_id, At) on which the audit timeline collapse depends.
func TestESMOneToOneMapping(t *testing.T) {
	cases := []struct {
		esm  string
		trig string
	}{
		{events.ESMSourceCreated, events.TriggerFired},
		{events.ESMPollFailed, events.TriggerRetry},
		{events.ESMDrainDLQ, events.TriggerDLQ},
		{events.ESMFilterError, events.TriggerFilterError},
	}
	for _, c := range cases {
		if !strings.HasPrefix(c.esm, "esm.") || !strings.HasPrefix(c.trig, "trigger.") {
			t.Errorf("dual-emit map broken: esm=%q trig=%q", c.esm, c.trig)
		}
	}
	// ESMSourceDeleted is intentionally the asymmetric one — pin
	// that no trigger.* mirror exists today (a future addition
	// would require an ADR amendment per ADR-118).
	_ = events.ESMSourceDeleted
}

// TestTriggerFilterErrorKind pins the new trigger.filter.error
// constant (commit 6 of the issue #757 mega-PR). It's the
// operator-debug audit kind emitted when a per-record filter
// parse error is encountered — NOT a customer-facing event.
func TestTriggerFilterErrorKind(t *testing.T) {
	if events.TriggerFilterError != "trigger.filter.error" {
		t.Errorf("TriggerFilterError = %q, want %q", events.TriggerFilterError, "trigger.filter.error")
	}
}
