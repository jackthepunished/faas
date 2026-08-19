// trigger.go — trigger audit vocabulary
// (issue #757 / ADR-0NN, commit #15 of feat-triggers-mega).
//
// Four new audit kinds extend the spec §5.1 taxonomy with a
// `trigger.` prefix so the §12 panel selector
// (`kind_prefix=trigger.`) captures the full trigger lifecycle:
//
//	trigger.fired         per-record: broker delivered + dispatched
//	trigger.fired.batch   per-batch: aggregated outcome counts
//	trigger.retry         per-record: state → retry, next_fire_at
//	trigger.dlq           per-record: state → dead_letter
//
// Pattern: each event is a typed struct that satisfies the
// WakeEvent interface (Kind() string). Platform.Emit writes a
// single row per call. The dashboard surfaces them via
// GET /v1/apps/{slug}/triggers/{tid}/audit (commit #18).
//
// Naming follows the wake.* convention — kind strings are
// lowercase, dot-separated, with the resource name first. The
// 4 kinds cover the FSM transitions of commit #14:

package events

import (
	"encoding/json"
	"time"
)

// Trigger-timeline vocabulary (issue #757 / ADR-0NN). Constants
// are the canonical kind strings written to events.kind.
const (
	// TriggerFired — one record successfully dispatched through
	// the gateway batch envelope. Payload: {trigger_id,
	// record_id, app_id, item_id, fired_at}.
	TriggerFired = "trigger.fired"

	// TriggerFiredBatch — aggregated outcome for one batch
	// envelope. Emitted once per dispatch tick per trigger with
	// the counts (records, succeeded, retry, dead_letter).
	// Payload: {trigger_id, batch_size, attempt_total,
	// succeeded_total, failed_total}.
	TriggerFiredBatch = "trigger.fired.batch"

	// TriggerRetry — record transitioned to state='retry'.
	// Payload: {trigger_id, record_id, app_id, attempt,
	// next_fire_at, last_error}.
	TriggerRetry = "trigger.retry"

	// TriggerDLQ — record transitioned to state='dead_letter'
	// (poison_record / max_attempts / rate_limited / broker_error
	// per the dlq CHECK constraint). Payload: {trigger_id,
	// record_id, app_id, reason, attempts}.
	TriggerDLQ = "trigger.dlq"

	// TriggerFilterError — FilterCriteria evaluation rejected a
	// record's path (ADR-118 / commit 6 of the issue #757
	// mega-PR). Operator-debug audit only; the record is
	// still Ack'd at the broker (a re-poll would loop on the
	// same parse error). Payload: {trigger_id, app_id, error}.
	TriggerFilterError = "trigger.filter.error"
)

// TriggerFiredEvent is the typed payload for TriggerFired.
type TriggerFiredEvent struct {
	TriggerID string    `json:"trigger_id"`
	RecordID  string    `json:"record_id"`
	AppID     string    `json:"app_id"`
	ItemID    string    `json:"item_id"`
	FiredAt   time.Time `json:"fired_at"`
}

// Kind returns the audit kind for TriggerFiredEvent.
func (TriggerFiredEvent) Kind() string { return TriggerFired }

// At returns the event timestamp for TriggerFiredEvent.
func (e TriggerFiredEvent) At() time.Time {
	if e.FiredAt.IsZero() {
		return time.Now()
	}
	return e.FiredAt
}

// Subject returns nil for TriggerFiredEvent — the trigger_id is
// in the payload; the events row is unattributed at the account
// level (the row is found via trigger_id, not account_id).
func (TriggerFiredEvent) Subject() *string { return nil }

// Payload returns the typed struct for TriggerFiredEvent.
func (e TriggerFiredEvent) Payload() map[string]any { return eventPayload(e) }

// TriggerFiredBatchEvent is the typed payload for TriggerFiredBatch.
type TriggerFiredBatchEvent struct {
	TriggerID      string `json:"trigger_id"`
	BatchSize      int    `json:"batch_size"`
	AttemptTotal   int    `json:"attempt_total"`
	SucceededTotal int    `json:"succeeded_total"`
	FailedTotal    int    `json:"failed_total"`
}

// Kind returns the audit kind for TriggerFiredBatchEvent.
func (TriggerFiredBatchEvent) Kind() string { return TriggerFiredBatch }

// At returns the current time for TriggerFiredBatchEvent.
func (TriggerFiredBatchEvent) At() time.Time { return time.Now() }

// Subject returns nil for TriggerFiredBatchEvent.
func (TriggerFiredBatchEvent) Subject() *string { return nil }

// Payload returns the typed struct for TriggerFiredBatchEvent.
func (e TriggerFiredBatchEvent) Payload() map[string]any { return eventPayload(e) }

// TriggerRetryEvent is the typed payload for TriggerRetry.
type TriggerRetryEvent struct {
	TriggerID  string    `json:"trigger_id"`
	RecordID   string    `json:"record_id"`
	AppID      string    `json:"app_id"`
	Attempt    int       `json:"attempt"`
	NextFireAt time.Time `json:"next_fire_at"`
	LastError  string    `json:"last_error,omitempty"`
}

// Kind returns the audit kind for TriggerRetryEvent.
func (TriggerRetryEvent) Kind() string { return TriggerRetry }

// At returns the next_fire_at timestamp for TriggerRetryEvent so
// the audit timeline reflects the broker-side retry window.
func (e TriggerRetryEvent) At() time.Time {
	if e.NextFireAt.IsZero() {
		return time.Now()
	}
	return e.NextFireAt
}

// Subject returns nil for TriggerRetryEvent.
func (TriggerRetryEvent) Subject() *string { return nil }

// Payload returns the typed struct for TriggerRetryEvent.
func (e TriggerRetryEvent) Payload() map[string]any { return eventPayload(e) }

// TriggerDLQEvent is the typed payload for TriggerDLQ.
type TriggerDLQEvent struct {
	TriggerID string `json:"trigger_id"`
	RecordID  string `json:"record_id"`
	AppID     string `json:"app_id"`
	Reason    string `json:"reason"`
	Attempts  int    `json:"attempts"`
	// LastError is the function-supplied error string the
	// gateway captured in the dispatch response. Review
	// finding #8: the prior code dropped status.Error on the
	// floor and hardcoded Reason='max_attempts' regardless of
	// cause. Carrying LastError here lets operators reading the
	// audit timeline distinguish "function timed out" from "we
	// exhausted retries". Optional; omitted when the gateway
	// response had no Error string.
	LastError string `json:"last_error,omitempty"`
}

// Kind returns the audit kind for TriggerDLQEvent.
func (TriggerDLQEvent) Kind() string { return TriggerDLQ }

// At returns the current time for TriggerDLQEvent.
func (TriggerDLQEvent) At() time.Time { return time.Now() }

// Subject returns nil for TriggerDLQEvent.
func (TriggerDLQEvent) Subject() *string { return nil }

// Payload returns the typed struct for TriggerDLQEvent.
func (e TriggerDLQEvent) Payload() map[string]any { return eventPayload(e) }

// TriggerFilterErrorEvent is the typed payload for
// TriggerFilterError. Emitted from
// pkg/sched/dispatch_triggers.go::dispatchOneTrigger when the
// per-record FilterCriteria evaluation rejected a record's
// path (the filter was authored in an unsupported form like
// "$.items[?(...)]"; the file-header doc on filter.go:38-55
// pins the minimal grammar). One event per tick per trigger
// summarises the count; individual record-level errors are
// Ack'd silently to the broker (no per-record audit row —
// would balloon the events table for a single bad filter).
type TriggerFilterErrorEvent struct {
	TriggerID string `json:"trigger_id"`
	AppID     string `json:"app_id"`
	Error     string `json:"error"`
}

// Kind returns the audit kind for TriggerFilterErrorEvent.
func (TriggerFilterErrorEvent) Kind() string { return TriggerFilterError }

// At returns the current time for TriggerFilterErrorEvent.
func (TriggerFilterErrorEvent) At() time.Time { return time.Now() }

// Subject returns nil for TriggerFilterErrorEvent.
func (TriggerFilterErrorEvent) Subject() *string { return nil }

// Payload returns the typed struct for TriggerFilterErrorEvent.
func (e TriggerFilterErrorEvent) Payload() map[string]any { return eventPayload(e) }

// Compile-time guarantees each new event implements WakeEvent so
// Platform.Emit accepts it.
var (
	_ WakeEvent = TriggerFiredEvent{}
	_ WakeEvent = TriggerFiredBatchEvent{}
	_ WakeEvent = TriggerRetryEvent{}
	_ WakeEvent = TriggerDLQEvent{}
	_ WakeEvent = TriggerFilterErrorEvent{}
)

// ESM vocabulary (ADR-118 / issue #757 closure, commit 4 of 11).
//
// The four esm.* kinds below are the OPERATOR-FACING ALIASES of
// the four trigger.* kinds above. They are dual-emitted from the
// same call sites (commit 6 wires the dual-emit into
// pkg/sched/dispatch_triggers.go and the apid createTrigger /
// deleteTrigger handlers) so a Grafana panel selector
// (`kind_prefix=esm.`) and a customer dashboard selector
// (`kind_prefix=trigger.`) both see the lifecycle.
//
// Per ADR-118 §"Audit vocabulary bridging":
//   - trigger.* is canonical (the wire shape + spec §5.1).
//   - esm.* is the operator alias (issue #757's original
//     wording; preserves the "ESM" vocabulary operators were
//     used to during the PR #910 planning phase).
//   - Both kinds land in the events table; consumers that want
//     to deduplicate can join on (trigger_id, record_id, At).
//
// Adding a new esm.* kind requires (a) a constant here, (b) a
// matching constant in the trigger.* set above (the pair
// should be 1:1 — never an esm.* without a trigger.* mirror),
// and (c) a unit test asserting the Kind() string matches
// exactly.
const (
	// ESMSourceCreated — mirrors TriggerFired (creation-side
	// event). Emitted on apid's createTrigger handler after the
	// `triggers` row commits, BEFORE the poller goroutine takes
	// over. The Pair field carries the canonical
	// trigger.source.created constant so a JOIN can collapse
	// the two rows to one timeline entry.
	ESMSourceCreated = "esm.source.created"

	// ESMSourceDeleted — emitted on apid's deleteTrigger
	// handler after the `triggers` row commits. The trigger.*
	// counterpart ("trigger.source.deleted") does not exist —
	// the source-deleted vocabulary is ESM-specific (PR #910
	// used trigger.disabled for the soft-delete path; the
	// hard-delete path lands here). Pinning the asymmetry in
	// ADR-118 §"Asymmetric kind mapping".
	ESMSourceDeleted = "esm.source.deleted"

	// ESMPollFailed — mirrors TriggerRetry (failure-side
	// event). Emitted from pkg/sched/dispatch_triggers.go on
	// the per-tick error branch (network failure, broker
	// timeout, group rebalance). Distinct from TriggerDLQ —
	// this is "the poll loop could not run"; DLQ is "a
	// record was dead-lettered".
	ESMPollFailed = "esm.poll.failed"

	// ESMDrainDLQ — mirrors TriggerDLQ. Emitted when a record
	// transitions to state='dead_letter' (poison_record,
	// max_attempts, rate_limited, broker_error, plan_quota,
	// payload_too_large, customer_disabled per the
	// trigger_dead_letter CHECK). The dual-emit lets a
	// dashboard selector `kind_prefix=esm.` filter
	// ESM-specific DLQs without losing cross-source aggregates.
	ESMDrainDLQ = "esm.drain.dlq"

	// ESMFilterError — mirrors TriggerFilterError. Emitted from
	// pkg/sched/dispatch_triggers.go when the per-record
	// FilterCriteria evaluation rejects a record's path (the
	// filter was authored in an unsupported form like
	// "$.items[?(...)]"). The audit row is operator-debug only
	// — the record is still Ack'd at the broker (a re-poll
	// would loop on the same parse error). No trigger.*
	// counterpart to "esm.source.created" or "esm.source.deleted"
	// exists for the FILTER-error case; the dual-emit pattern
	// applies to TriggerFilterError → ESMFilterError ONLY.
	ESMFilterError = "esm.filter.error"
)

// ESMSourceCreatedEvent is the typed payload for ESMSourceCreated.
// Emitted by the apid createTrigger handler (commit 6 wiring).
// Pair=TriggerFired-ish "source.created" is the canonical
// counterpart (no trigger.source.created kind exists today —
// the creation-side is ESM-only per the asymmetric mapping
// above; the trigger.fired lifecycle begins on the first
// broker delivery, NOT on creation).
//
// Naming note: SourceKind (not Kind) avoids shadowing the
// WakeEvent.Kind() method, and EmitAt (not At) matches the
// wake.go convention. The JSON tag stays `kind` / `at` so
// the wire shape is unchanged for SDK consumers.
type ESMSourceCreatedEvent struct {
	TriggerID  string    `json:"trigger_id"`
	AccountID  string    `json:"account_id"`
	AppID      string    `json:"app_id"`
	SourceKind string    `json:"kind"` // TriggerKind string (kafka/nats/...)
	EmitAt     time.Time `json:"at"`
}

// Kind returns the audit kind for ESMSourceCreatedEvent.
func (ESMSourceCreatedEvent) Kind() string { return ESMSourceCreated }

// At returns the event timestamp for ESMSourceCreatedEvent.
func (e ESMSourceCreatedEvent) At() time.Time {
	if e.EmitAt.IsZero() {
		return time.Now()
	}
	return e.EmitAt
}

// Subject returns nil — the trigger_id is in the payload.
func (ESMSourceCreatedEvent) Subject() *string { return nil }

// Payload returns the typed struct for ESMSourceCreatedEvent.
func (e ESMSourceCreatedEvent) Payload() map[string]any { return eventPayload(e) }

// ESMSourceDeletedEvent is the typed payload for ESMSourceDeleted.
type ESMSourceDeletedEvent struct {
	TriggerID  string    `json:"trigger_id"`
	AccountID  string    `json:"account_id"`
	AppID      string    `json:"app_id"`
	SourceKind string    `json:"kind"`
	EmitAt     time.Time `json:"at"`
}

func (ESMSourceDeletedEvent) Kind() string { return ESMSourceDeleted }
func (e ESMSourceDeletedEvent) At() time.Time {
	if e.EmitAt.IsZero() {
		return time.Now()
	}
	return e.EmitAt
}
func (ESMSourceDeletedEvent) Subject() *string          { return nil }
func (e ESMSourceDeletedEvent) Payload() map[string]any { return eventPayload(e) }

// ESMPollFailedEvent is the typed payload for ESMPollFailed.
type ESMPollFailedEvent struct {
	TriggerID  string    `json:"trigger_id"`
	AppID      string    `json:"app_id"`
	SourceKind string    `json:"kind"`
	Error      string    `json:"error"`
	EmitAt     time.Time `json:"at"`
}

func (ESMPollFailedEvent) Kind() string { return ESMPollFailed }
func (e ESMPollFailedEvent) At() time.Time {
	if e.EmitAt.IsZero() {
		return time.Now()
	}
	return e.EmitAt
}
func (ESMPollFailedEvent) Subject() *string          { return nil }
func (e ESMPollFailedEvent) Payload() map[string]any { return eventPayload(e) }

// ESMDrainDLQEvent is the typed payload for ESMDrainDLQ.
type ESMDrainDLQEvent struct {
	TriggerID string    `json:"trigger_id"`
	RecordID  string    `json:"record_id"`
	AppID     string    `json:"app_id"`
	Reason    string    `json:"reason"` // poison_record | max_attempts | rate_limited | ...
	EmitAt    time.Time `json:"at"`
}

func (ESMDrainDLQEvent) Kind() string { return ESMDrainDLQ }
func (e ESMDrainDLQEvent) At() time.Time {
	if e.EmitAt.IsZero() {
		return time.Now()
	}
	return e.EmitAt
}
func (ESMDrainDLQEvent) Subject() *string          { return nil }
func (e ESMDrainDLQEvent) Payload() map[string]any { return eventPayload(e) }

// ESMFilterErrorEvent is the typed payload for ESMFilterError
// (dual-emit counterpart to TriggerFilterErrorEvent). The
// field names mirror the SourceKind/EmitAt convention used by
// the other ESM events; the JSON tags stay `kind` / `at` for
// wire compatibility with existing consumers.
type ESMFilterErrorEvent struct {
	TriggerID  string    `json:"trigger_id"`
	AppID      string    `json:"app_id"`
	SourceKind string    `json:"kind"`
	Error      string    `json:"error"`
	EmitAt     time.Time `json:"at"`
}

func (ESMFilterErrorEvent) Kind() string { return ESMFilterError }
func (e ESMFilterErrorEvent) At() time.Time {
	if e.EmitAt.IsZero() {
		return time.Now()
	}
	return e.EmitAt
}
func (ESMFilterErrorEvent) Subject() *string          { return nil }
func (e ESMFilterErrorEvent) Payload() map[string]any { return eventPayload(e) }

// ESM (compile-time) — every ESM event satisfies WakeEvent.
var (
	_ WakeEvent = ESMSourceCreatedEvent{}
	_ WakeEvent = ESMSourceDeletedEvent{}
	_ WakeEvent = ESMPollFailedEvent{}
	_ WakeEvent = ESMDrainDLQEvent{}
	_ WakeEvent = ESMFilterErrorEvent{}
)

// eventPayload marshals the typed struct into map[string]any so
// the WakeEvent.Payload contract is satisfied without each event
// re-implementing the marshal step.
func eventPayload(v any) map[string]any {
	out := map[string]any{}
	body, err := json.Marshal(v)
	if err != nil {
		return out
	}
	_ = json.Unmarshal(body, &out)
	return out
}
