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
}

// Kind returns the audit kind for TriggerDLQEvent.
func (TriggerDLQEvent) Kind() string { return TriggerDLQ }

// At returns the current time for TriggerDLQEvent.
func (TriggerDLQEvent) At() time.Time { return time.Now() }

// Subject returns nil for TriggerDLQEvent.
func (TriggerDLQEvent) Subject() *string { return nil }

// Payload returns the typed struct for TriggerDLQEvent.
func (e TriggerDLQEvent) Payload() map[string]any { return eventPayload(e) }

// Compile-time guarantees each new event implements WakeEvent so
// Platform.Emit accepts it.
var (
	_ WakeEvent = TriggerFiredEvent{}
	_ WakeEvent = TriggerFiredBatchEvent{}
	_ WakeEvent = TriggerRetryEvent{}
	_ WakeEvent = TriggerDLQEvent{}
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
