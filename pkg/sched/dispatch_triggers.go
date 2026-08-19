// dispatch_triggers.go — trigger dispatch tick
// (issue #757 / ADR-0NN, commit #14 of feat-triggers-mega).
//
// runTriggerTick is the sibling of runCronTick (loop.go:1808):
// 1-second cadence, walks every enabled trigger, polls its broker
// adapter, claims the per-record FSM rows via ClaimTriggerRecords
// (FOR UPDATE SKIP LOCKED), batches the records (size + 6MB cap),
// posts the batch envelope to the gateway, parses per-record
// status, and transitions trigger_records rows through the FSM.
//
// FSM per record:
//
//	pending ── claim ─▶ claimed ── succeeded
//	                              ── retry ─▶ retry (next_fire_at=future)
//	                                            └▶ attempts >= max ─▶ dead_letter
//	                              ── poison_record ─▶ dead_letter
//
// Concurrency: the Loop's mutex protects the cron tick. We use
// the same mutex here so two ticks don't race on the same
// trigger's pollers (the broker library is goroutine-safe but the
// per-trigger in-flight map is not).
//
// Rate-limit gate: AllowWakeApp + AllowWakeAccount, AND semantics
// per pkg/sched/rate_limit.go:113-150. Deny path lifts to
// trigger.dlq audit + dead_letter row per record (NOT a transient
// 429 like the wake path — triggers retry on next dispatch tick).

package sched

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"strings"
	"time"

	"github.com/onebox-faas/faas/pkg/api"
	"github.com/onebox-faas/faas/pkg/events"
	"github.com/onebox-faas/faas/pkg/state/sqlc"
)

// trigger DLQ reason constants (match the CHECK on
// trigger_dead_letter.reason in migrations/00296_triggers.sql).
// CI lint rule goconst would otherwise flag the per-reason
// comparisons in classifyDLQReason() + the path-through call
// sites as duplicates.
const (
	triggerReasonPoisonRecord    = "poison_record"
	triggerReasonMaxAttempts     = "max_attempts"
	triggerReasonBrokerError     = "broker_error"
	triggerReasonRateLimited     = "rate_limited"
	triggerReasonPayloadTooLarge = "payload_too_large"
)

// triggerDispatchRecord is one broker-delivered record the
// dispatch tick packages for the gateway batch envelope.
type triggerDispatchRecord struct {
	ItemIdentifier string            `json:"item_identifier"`
	PayloadB64     string            `json:"payload_b64"`
	Headers        map[string]string `json:"headers"`
	Metadata       map[string]any    `json:"metadata"`
}

// triggerDispatchRequest is the JSON body posted to
// /v1/invocations:dispatch_batch on the gateway (commit #13).
type triggerDispatchRequest struct {
	InvocationID string                  `json:"invocation_id"`
	AppID        string                  `json:"app_id"`
	Source       string                  `json:"source"`
	TriggerID    string                  `json:"trigger_id"`
	Records      []triggerDispatchRecord `json:"records"`
}

// triggerDispatchResponse mirrors pkg/gateway/synth.go's batch
// response shape.
type triggerDispatchResponse struct {
	Results []triggerDispatchResult `json:"results"`
}

// triggerDispatchResult mirrors one batch record's outcome.
type triggerDispatchResult struct {
	ItemIdentifier string `json:"item_identifier"`
	Status         string `json:"status"`
	Error          string `json:"error,omitempty"`
	// Code (audit #8) is the structured counterpart to Error —
	// a stable machine-readable string the dispatch tick
	// switches on instead of substring-matching Error. Mirrors
	// pkg/gateway/synth.go::batchDispatchResult.Code.
	Code string `json:"code,omitempty"`
}

// loopStoreAccessor returns the store the Loop uses. The store
// lives on Loop.engine.store; for commit #14 we go through a
// tiny helper so callers in this file don't reach into engine
// internals.
func (l *Loop) loopStore() storeLike {
	if l.engine != nil {
		return l.engine.store
	}
	return nil
}

// storeLike is the trimmed interface this file needs from the
// store. Defined inline to keep dispatch_triggers.go free of a
// hard dependency on pkg/state.
type storeLike interface {
	ListEnabledTriggers(ctx context.Context) ([]sqlc.Trigger, error)
	ClaimTriggerRecords(ctx context.Context, triggerID string, limit int32) ([]sqlc.TriggerRecord, error)
	// InsertTriggerRecord bridges broker-delivered records into
	// the trigger_records FSM queue (review finding #1, PR #910).
	InsertTriggerRecord(ctx context.Context, triggerID, itemIdentifier string, payload, headers, metadata []byte) (string, error)
	MarkTriggerRecordSucceeded(ctx context.Context, id string) error
	MarkTriggerRecordRetry(ctx context.Context, id, lastError string, nextFireAt time.Time) error
	MarkTriggerRecordDeadLetter(ctx context.Context, id, lastError string) error
	InsertTriggerDeadLetter(ctx context.Context, recordID, triggerID, reason, routedTo string, detail []byte) error
	// TriggerRecordIDByItemIdentifier (audit round 2 finding #1)
	// bridges the broker-handle namespace (kafka offset, NATS
	// seq, SQS receipt handle, Redis entry-id, queue invocation_id)
	// to the trigger_records.id UUID the dead_letter FK expects.
	TriggerRecordIDByItemIdentifier(ctx context.Context, triggerID, itemIdentifier string) (string, error)
}

// triggerWakeup is the channel-side wakeup signal the schedd's
// pg_notify subscriber delivers on every NotifyTriggerReady +
// NotifyTriggerChanged payload. The Loop's run() method selects
// on it alongside the 1s ticker.
//
// WakeupTriggers sends a single token; runTriggerTick reads at
// most one trigger per ticker arm so a burst of broker messages
// doesn't cause a stampede.

// WakeupTriggers nudges the dispatch tick. Idempotent; safe to
// call from any goroutine.
func (l *Loop) WakeupTriggers() {
	if l == nil {
		return
	}
	l.triggerWakeupOnce.Do(func() {
		l.triggerWakeup = make(chan struct{}, 1)
	})
	select {
	case l.triggerWakeup <- struct{}{}:
	default:
		// Channel full — a wake is already pending.
	}
}

// runTriggerTick is the entry point. One tick handles ALL
// enabled triggers.
func (l *Loop) runTriggerTick(ctx context.Context) {
	if l == nil || l.engine == nil {
		return
	}
	store := l.loopStore()
	if store == nil {
		return
	}
	// Without an HTTP transport for the batch dispatch path
	// (gatewayd-internal was not wired at boot — production
	// schedd on a single-box deploy, or the schedd-only e2e
	// harness like meterd_dunning_e2e_test / meterd_quota_e2e_test
	// that don't run gatewayd-internal), skip the tick entirely.
	// Otherwise we walk every trigger, poll records from each
	// broker, claim the trigger_records rows, fail on postBatch
	// (typed error "gateway http client not configured"), and
	// bounce the records to retry — every 1s. That tight retry
	// storm will also mask the real failure (the dial error from
	// boot), so the warn emitted at boot by cmd/schedd/main.go is
	// the single source of operator visibility. PR #910 audit #5
	// required boot-fatal for the dial; the boot warn + tick
	// short-circuit preserves that signal without crashing the
	// daemon when the trigger primitive is unconfigured.
	if l.gatewayHTTPClient == nil {
		return
	}
	triggers, err := store.ListEnabledTriggers(ctx)
	if err != nil {
		l.log.Warn("sched trigger tick: list", "err", err)
		return
	}
	if len(triggers) == 0 {
		return
	}
	// Cache per-app+plan lookup so the per-record rate-limit gate
	// (review finding #4: was hardcoded to api.PlanFree) can
	// re-use the AccountPlan across the loop. The whole batch
	// for one trigger is one wake plan; the cache halves the
	// per-tick Postgres load when many triggers share an app.
	planCache := map[string]api.Plan{}
	resolvePlan := func(appID string) api.Plan {
		if p, ok := planCache[appID]; ok {
			return p
		}
		// Look up app → account to read the actual plan.
		app, appErr := l.engine.Store().AppByID(ctx, appID)
		if appErr != nil {
			return api.PlanFree
		}
		acct, acctErr := l.engine.Store().AccountByID(ctx, app.AccountID)
		if acctErr != nil {
			return api.PlanFree
		}
		if acct.Plan == "" {
			return api.PlanFree
		}
		planCache[appID] = acct.Plan
		return acct.Plan
	}
	for i := range triggers {
		t := triggers[i]
		if err := l.dispatchOneTrigger(ctx, t, store, resolvePlan); err != nil {
			l.log.Warn("sched trigger tick: dispatch",
				"trigger_id", t.ID.String(),
				"kind", t.Kind,
				"err", err)
		}
	}
}

// dispatchOneTrigger runs one trigger's per-tick work. The planFor
// resolver is consulted at the rate-limit gate so Hobby/Pro/Scale
// apps get their per-plan WakeBurstPerApp + TriggerRecordsPerSecondPerApp
// caps rather than collapsing to Free.
func (l *Loop) dispatchOneTrigger(ctx context.Context, t sqlc.Trigger, store storeLike, planFor func(string) api.Plan) error {
	// 1. Poller lookup. Cached on the Loop.
	if l.triggerPollers == nil {
		l.triggerPollers = map[string]triggerSource{}
	}
	poller, ok := l.triggerPollers[t.ID.String()]
	if !ok {
		src, ok := newPollerForTrigger(t)
		if !ok {
			l.log.Debug("sched trigger tick: no poller for kind",
				"trigger_id", t.ID.String(),
				"kind", t.Kind)
			return nil
		}
		poller = src
		l.triggerPollers[t.ID.String()] = poller
	}
	if poller == nil {
		return nil
	}

	// 2. Poll.
	res := poller.Poll(ctx, t)
	if res.Error != nil {
		l.log.Warn("sched trigger tick: poll",
			"trigger_id", t.ID.String(),
			"kind", t.Kind,
			"err", res.Error)
		return fmt.Errorf("poll trigger %s: %w", t.ID, res.Error)
	}
	if len(res.Records) == 0 {
		return nil
	}

	// 3. Batch close: size / 6MB.
	batch := closeBatch(res.Records, int(t.BatchSizeMax), int(t.PayloadMaxBytes))
	if len(batch) == 0 {
		return nil
	}

	// 4. Rate-limit gate. Deny → dead_letter(reason='rate_limited').
	// review finding #4: the plan argument was hardcoded to api.PlanFree,
	// which collapsed Hobby/Pro/Scale customers to the Free bucket's
	// 1-wake-per-minute ceiling. Resolve the actual account plan via
	// the per-tick plan cache (constructed in runTriggerTick).
	appPlan := api.PlanFree
	if planFor != nil {
		appPlan = planFor(t.AppID.String())
	}
	if l.rateLimiter != nil && !l.rateLimiter.AllowWakeApp(t.AppID.String(), appPlan) {
		items := batchItemIDs(batch)
		l.deadLetterAll(ctx, t.ID.String(), items, triggerReasonRateLimited, "wake rate limit exceeded", store)
		return nil
	}
	// Review finding #8: also consult the per-account bucket so a
	// runaway broker fan-out across many apps under one account
	// is rejected even when each app stays within its per-app
	// cap. The lookup walks app → account inline because the
	// per-tick plan cache above only stores AccountPlan, not the
	// account_id; an extra round-trip per trigger (one per tick)
	// is acceptable because the deny path is the hot path we
	// want to keep fast.
	if l.rateLimiter != nil {
		app, appErr := l.engine.Store().AppByID(ctx, t.AppID.String())
		if appErr == nil {
			if !l.rateLimiter.AllowWakeAccount(app.AccountID, appPlan) {
				items := batchItemIDs(batch)
				l.deadLetterAll(ctx, t.ID.String(), items, triggerReasonRateLimited, "wake rate limit exceeded", store)
				return nil
			}
		}
	}

	// 5. Persist polled records into trigger_records (review finding
	// #1, PR #910: without this insert, ClaimTriggerRecords returns 0
	// rows and the entire dispatch tick is structurally dead — every
	// record never reaches the gateway and the function never fires).
	//
	// Each Poll() returned SourceRecord becomes one trigger_records
	// row BEFORE we attempt to claim + dispatch. ON CONFLICT
	// (trigger_id, item_identifier) DO NOTHING (set in
	// queries.sql:1283-1310) means a re-poll after a partial commit
	// + Ack timeout reuses the existing row id rather than doubling
	// the queue depth.
	//
	// Rollback semantics: if the insert fails for a record, the
	// dispatch tick continues without claiming it (the row didn't
	// land, so SKIP LOCKED can't see it) and the broker message
	// stays in poller.inFlight. On the next poll cycle the broker
	// library re-delivers; the next tick tries the insert again.
	// This is the "Ack only after the row exists" guarantee the
	// audit pins: dispatch_triggers.go never calls poller.Ack on a
	// record whose trigger_records row is missing.
	for _, rec := range batch {
		payload := rec.Payload
		if payload == nil {
			payload = []byte("{}")
		}
		headers := marshalJSON(rec.Headers)
		metadata := marshalJSON(rec.Metadata)
		if _, err := store.InsertTriggerRecord(ctx, t.ID.String(), rec.ItemIdentifier, payload, headers, metadata); err != nil {
			l.log.Warn("sched trigger tick: insert record",
				"trigger_id", t.ID.String(),
				"item_identifier", rec.ItemIdentifier,
				"err", err)
		}
	}

	// 6. Claim trigger_records rows.
	claimed, claimErr := store.ClaimTriggerRecords(ctx, t.ID.String(), int32(len(batch)))
	if claimErr != nil {
		l.log.Warn("sched trigger tick: claim",
			"trigger_id", t.ID.String(),
			"err", claimErr)
		return fmt.Errorf("dispatchOneTrigger claim: %w", claimErr)
	}
	if len(claimed) == 0 {
		return nil
	}

	// 6. Post the batch envelope to the gateway.
	envelope := buildDispatchEnvelope(t, batch)
	respBody, postErr := l.postBatch(ctx, envelope)
	if postErr != nil {
		l.log.Warn("sched trigger tick: gateway post",
			"trigger_id", t.ID.String(),
			"err", postErr)
		l.markRetryAll(ctx, claimed, postErr.Error(), store)
		// Audit finding #6: SKIP LOCKED may return fewer rows
		// than len(batch). Nack only the records we actually
		// claimed — the rest stay in poller.inFlight and re-poll
		// on the next tick. Without this guard the broker side
		// saw Ack/Nack on records that had no trigger_records row
		// to retry, and the poller's in-flight bookkeeping
		// dropped those entries on the floor.
		_ = poller.Nack(ctx, t, claimedItemIDs(claimed), triggerReasonBrokerError)
		return nil
	}

	var resp triggerDispatchResponse
	if err := json.Unmarshal(respBody, &resp); err != nil {
		l.log.Warn("sched trigger tick: response parse",
			"trigger_id", t.ID.String(),
			"err", err)
		ids := make([]string, 0, len(claimed))
		for _, c := range claimed {
			ids = append(ids, c.ID.String())
		}
		l.deadLetterAll(ctx, t.ID.String(), ids, triggerReasonPoisonRecord, "gateway response malformed", store)
		// Audit finding #6 (paired with the post-error path
		// above): same partial-claim guard — only Nack the
		// item_identifiers that have a corresponding
		// trigger_records row. The other batch entries stay
		// in poller.inFlight until the next poll cycle.
		_ = poller.Nack(ctx, t, claimedItemIDs(claimed), triggerReasonPoisonRecord)
		return nil
	}

	statusByID := make(map[string]triggerDispatchResult, len(resp.Results))
	for _, r := range resp.Results {
		statusByID[r.ItemIdentifier] = r
	}

	succeedIDs := []string{}
	retryIDs := []string{}
	dlqIDs := []string{}
	dlqReasons := []string{}   // parallel to dlqIDs; review finding #8
	dlqErrors := []string{}    // parallel to dlqIDs; review finding #8
	dlqAttempts := []int32{}   // parallel to dlqIDs; review finding #10
	retryAttempts := []int32{} // parallel to retryIDs; review finding #10
	succeedItems := []string{}
	retryItems := []string{}
	dlqItems := []string{}

	for _, c := range claimed {
		itemID := c.ItemIdentifier
		status, found := statusByID[itemID]
		if !found {
			retryIDs = append(retryIDs, c.ID.String())
			retryItems = append(retryItems, itemID)
			// Review finding #10: report the post-increment
			// Attempts value.
			retryAttempts = append(retryAttempts, c.Attempts+1)
			continue
		}
		switch status.Status {
		case "succeeded":
			succeedIDs = append(succeedIDs, c.ID.String())
			succeedItems = append(succeedItems, itemID)
		case "retry", "broker_error":
			retryIDs = append(retryIDs, c.ID.String())
			retryItems = append(retryItems, itemID)
			retryAttempts = append(retryAttempts, c.Attempts+1)
		case "dead_letter":
			dlqIDs = append(dlqIDs, c.ID.String())
			dlqItems = append(dlqItems, itemID)
			dlqAttempts = append(dlqAttempts, c.Attempts+1)
			// Review finding #8: the old code dropped
			// status.Error on the floor and stampped the
			// audit Reason='max_attempts' regardless of
			// cause. Map the gateway-supplied status.Error
			// onto one of the trigger_dead_letter.reason
			// CHECK values; fall through to 'poison_record'
			// when the gateway didn't classify.
			reason := classifyDLQReason(status.Code, status.Error)
			dlqReasons = append(dlqReasons, reason)
			dlqErrors = append(dlqErrors, status.Error)
		default:
			retryIDs = append(retryIDs, c.ID.String())
			retryItems = append(retryItems, itemID)
			retryAttempts = append(retryAttempts, c.Attempts+1)
		}
	}

	if len(succeedIDs) > 0 {
		for _, id := range succeedIDs {
			if err := store.MarkTriggerRecordSucceeded(ctx, id); err != nil {
				l.log.Warn("sched trigger tick: mark succeeded",
					"id", id, "err", err)
			}
			// Audit: trigger.fired per succeeded record.
			l.emitAudit(ctx, events.TriggerFiredEvent{
				TriggerID: t.ID.String(),
				RecordID:  id,
				AppID:     t.AppID.String(),
				FiredAt:   time.Now(),
			})
		}
		if err := poller.Ack(ctx, t, succeedItems); err != nil {
			l.log.Warn("sched trigger tick: poller ack",
				"trigger_id", t.ID.String(), "err", err)
		}
	}
	if len(retryIDs) > 0 {
		for i, id := range retryIDs {
			attempts := retryAttempts[i]
			// Review finding #9: exponential backoff + ±20%
			// jitter replaces the prior hardcoded 2s.
			backoff := computeRetryBackoff(attempts)
			nextFireAt := time.Now().Add(backoff)
			if err := store.MarkTriggerRecordRetry(ctx, id, "", nextFireAt); err != nil {
				l.log.Warn("sched trigger tick: mark retry",
					"id", id, "err", err)
			}
			l.emitAudit(ctx, events.TriggerRetryEvent{
				TriggerID:  t.ID.String(),
				RecordID:   id,
				AppID:      t.AppID.String(),
				Attempt:    int(attempts),
				NextFireAt: nextFireAt,
			})
		}
		if err := poller.Nack(ctx, t, retryItems, triggerReasonBrokerError); err != nil {
			l.log.Warn("sched trigger tick: poller nack",
				"trigger_id", t.ID.String(), "err", err)
		}
	}
	if len(dlqIDs) > 0 {
		for i, id := range dlqIDs {
			reason := dlqReasons[i]
			lastErr := dlqErrors[i]
			attempts := dlqAttempts[i]
			if err := store.MarkTriggerRecordDeadLetter(ctx, id, lastErr); err != nil {
				l.log.Warn("sched trigger tick: mark dlq",
					"id", id, "err", err)
			}
			l.emitAudit(ctx, events.TriggerDLQEvent{
				TriggerID: t.ID.String(),
				RecordID:  id,
				AppID:     t.AppID.String(),
				Reason:    reason,
				Attempts:  int(attempts),
				LastError: lastErr,
			})
		}
		if err := poller.Nack(ctx, t, dlqItems, triggerReasonPoisonRecord); err != nil {
			l.log.Warn("sched trigger tick: poller nack (dlq)",
				"trigger_id", t.ID.String(), "err", err)
		}
	}
	// Audit: trigger.fired.batch — aggregated counts.
	l.emitAudit(ctx, events.TriggerFiredBatchEvent{
		TriggerID:      t.ID.String(),
		BatchSize:      len(batch),
		AttemptTotal:   len(batch),
		SucceededTotal: len(succeedIDs),
		FailedTotal:    len(retryIDs) + len(dlqIDs),
	})

	l.log.Debug("sched trigger tick: batch complete",
		"trigger_id", t.ID.String(),
		"kind", t.Kind,
		"records", len(batch),
		"succeeded", len(succeedIDs),
		"retry", len(retryIDs),
		"dead_letter", len(dlqIDs))
	return nil
}

// closeBatch truncates the polled slice to honour the per-trigger
// batch_size_max + 6MB payload cap.
//
// Per-record semantics at the gateway (audit round 2 finding #4,
// PR #910): the gateway's handleInvocationDispatchBatch loops
// Invoke() once per record — the handler is single-threaded,
// the schedd's HTTP client is held for the entire batch
// duration, and each record pays its own wake gate + admission
// cost. The doc-comment at pkg/gateway/synth.go:420 pins this
// explicitly because the original wording ("one function
// invocation serves N records", Lambda ESM) was misleading.
// A future PR can batch-encode and short-circuit Invoke() if a
// target VM is already RUNNING; that change is out of scope
// here.
func closeBatch(records []SourceRecord, sizeMax, byteCap int) []SourceRecord {
	if sizeMax <= 0 {
		sizeMax = len(records)
	}
	out := make([]SourceRecord, 0, len(records))
	total := 0
	for _, r := range records {
		if len(out) >= sizeMax {
			break
		}
		if total+len(r.Payload) > byteCap {
			break
		}
		total += len(r.Payload)
		out = append(out, r)
	}
	return out
}

// buildDispatchEnvelope packages the records into the JSON shape
// the gateway expects.
func buildDispatchEnvelope(t sqlc.Trigger, batch []SourceRecord) triggerDispatchRequest {
	recs := make([]triggerDispatchRecord, 0, len(batch))
	for _, r := range batch {
		recs = append(recs, triggerDispatchRecord{
			ItemIdentifier: r.ItemIdentifier,
			PayloadB64:     base64.StdEncoding.EncodeToString(r.Payload),
			Headers:        r.Headers,
			Metadata:       r.Metadata,
		})
	}
	return triggerDispatchRequest{
		InvocationID: "trigger-" + t.ID.String(),
		AppID:        t.AppID.String(),
		Source:       "esm",
		TriggerID:    t.ID.String(),
		Records:      recs,
	}
}

// postBatch hits the gateway's batch endpoint via the existing
// GatewaySynth HTTP transport.
func (l *Loop) postBatch(ctx context.Context, env triggerDispatchRequest) ([]byte, error) {
	body, err := json.Marshal(env)
	if err != nil {
		return nil, fmt.Errorf("marshal envelope: %w", err)
	}
	if l.gatewayHTTPClient == nil {
		return nil, fmt.Errorf("sched: gateway http client not configured")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		l.gatewayBaseURL+"/v1/invocations:dispatch_batch",
		&byteReadCloser{b: body})
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.ContentLength = int64(len(body))
	resp, err := l.gatewayHTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("post: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("status %d: %s", resp.StatusCode, raw)
	}
	return io.ReadAll(resp.Body)
}

// deadLetterAll marks every record as dead_letter + inserts a
// trigger_dead_letter row.
//
// The `ids` slice is the list of item_identifiers the broker
// delivered — broker handles (kafka offset, NATS seq, SQS receipt
// handle, Redis entry-id, queue invocation_id), NOT trigger_records
// row UUIDs. The trigger_dead_letter.record_id column is a UUID FK
// into trigger_records.id, so we resolve each item_identifier to
// its row UUID via TriggerRecordIDByItemIdentifier before the
// InsertTriggerDeadLetter call.
//
// Audit round 2 finding #1 (PR #910): without this lookup every
// rate-limit denial tripped SQLSTATE 23503, the dead_letter row
// was silently dropped, MarkTriggerRecordDeadLetter updated 0
// rows, the record stayed in poller.inFlight forever, and the
// broker offset never advanced.
//
// If the row hasn't been inserted yet (rate-limit fires before
// the InsertTriggerRecord loop on the next dispatch step), the
// lookup returns an empty UUID — we skip the DLQ insert + skip
// the MarkTriggerRecordDeadLetter call and leave the record in
// poller.inFlight for the next tick to retry. Rate-limited records
// MUST come back, not be silently lost.
//
// The poisoned-response path at dispatch_triggers.go:354 calls
// this helper with row UUIDs already (it walks the `claimed`
// result-set and reads c.ID.String()). That works because the
// UUID lookup self-resolves: the row is by construction present,
// so the lookup returns the same UUID we passed in.
func (l *Loop) deadLetterAll(ctx context.Context, triggerID string, ids []string, reason, detail string, store storeLike) {
	if store == nil || len(ids) == 0 {
		return
	}
	for _, id := range ids {
		uuid, lookupErr := store.TriggerRecordIDByItemIdentifier(ctx, triggerID, id)
		if lookupErr != nil {
			l.log.Warn("sched trigger tick: dlq record_id lookup",
				"trigger_id", triggerID,
				"item_identifier", id,
				"err", lookupErr)
			continue
		}
		// Empty UUID = row not yet in trigger_records (rate-limit
		// fired before InsertTriggerRecord). Skip — the broker
		// offset stays where it is and the next tick retries.
		if uuid == "" {
			l.log.Warn("sched trigger tick: dlq row missing (rate-limit before insert; record will be retried)",
				"trigger_id", triggerID,
				"item_identifier", id,
				"reason", reason)
			continue
		}
		if err := store.InsertTriggerDeadLetter(ctx, uuid, triggerID, reason, "drop", []byte(detail)); err != nil {
			l.log.Warn("sched trigger tick: insert dlq", "id", uuid, "err", err)
			continue
		}
		if err := store.MarkTriggerRecordDeadLetter(ctx, uuid, reason); err != nil {
			l.log.Warn("sched trigger tick: mark dlq", "id", uuid, "err", err)
		}
	}
}

// markRetryAll marks every record as state='retry'.
func (l *Loop) markRetryAll(ctx context.Context, claimed []sqlc.TriggerRecord, errMsg string, store storeLike) {
	if store == nil || len(claimed) == 0 {
		return
	}
	nextFireAt := time.Now().Add(2 * time.Second)
	for _, c := range claimed {
		if err := store.MarkTriggerRecordRetry(ctx, c.ID.String(), errMsg, nextFireAt); err != nil {
			l.log.Warn("sched trigger tick: mark retry", "id", c.ID.String(), "err", err)
		}
	}
}

// batchItemIDs walks the batch and returns the item identifiers
// for Ack/Nack on the poller.
func batchItemIDs(batch []SourceRecord) []string {
	if len(batch) == 0 {
		return nil
	}
	out := make([]string, 0, len(batch))
	for _, r := range batch {
		out = append(out, r.ItemIdentifier)
	}
	return out
}

// claimedItemIDs returns the item_identifiers from the ClaimTriggerRecords
// result set. Distinct from batchItemIDs because SKIP LOCKED may return
// fewer rows than the polled batch — Ack/Nack must operate only on
// records that have a corresponding trigger_records row (audit #6).
// Records that didn't get claimed stay in poller.inFlight and re-poll
// naturally on the next tick.
func claimedItemIDs(claimed []sqlc.TriggerRecord) []string {
	if len(claimed) == 0 {
		return nil
	}
	out := make([]string, 0, len(claimed))
	for _, c := range claimed {
		out = append(out, c.ItemIdentifier)
	}
	return out
}

// byteReadCloser is a tiny io.ReadCloser for a []byte.
type byteReadCloser struct {
	b   []byte
	pos int
}

func (r *byteReadCloser) Read(p []byte) (int, error) {
	if r.pos >= len(r.b) {
		return 0, io.EOF
	}
	n := copy(p, r.b[r.pos:])
	r.pos += n
	return n, nil
}

func (r *byteReadCloser) Close() error { return nil }

// marshalJSON turns a generic map into a JSONB-ready byte slice.
// Returns "{}" when the input is nil so the ON CONFLICT DO NOTHING
// branch on InsertTriggerRecord stays deterministic across the
// broker adapters (kafka / nats / redis_streams all produce
// slightly different header / metadata shapes — every nil map
// must serialise to the same empty-object payload).
func marshalJSON(v any) []byte {
	if v == nil {
		return []byte("{}")
	}
	out, err := json.Marshal(v)
	if err != nil {
		return []byte("{}")
	}
	return out
}

// computeRetryBackoff returns the retry delay for an attempt at
// the post-increment `attempts` count. Review finding #9: replaces
// the hardcoded 2s previously used on every retry. Shape: 1s
// base, doubling each attempt up to a 5-minute ceiling, with
// ±20% jitter so a burst of synchronous broker failures doesn't
// re-fire on the same tick.
//
//	attempts=1 → ~1s  (range 0.8s..1.2s)
//	attempts=2 → ~2s  (range 1.6s..2.4s)
//	attempts=3 → ~4s  (range 3.2s..4.8s)
//	attempts=4 → ~8s
//	attempts=N → min(2^(N-1), 300)s
func computeRetryBackoff(attempts int32) time.Duration {
	if attempts < 1 {
		attempts = 1
	}
	exp := attempts - 1
	if exp > 9 {
		exp = 9
	}
	base := time.Second << exp
	if base > 5*time.Minute {
		base = 5 * time.Minute
	}
	//nolint:gosec // G404: jitter on retry backoff has no security value — the
	// worst a malicious caller can do is nudge our delay by ±20%, which
	// the dispatch tick absorbs. math/rand/v2 is correct here.
	jitter := time.Duration(float64(base) * (0.8 + 0.01*float64(rand.Uint64()%41)))
	return jitter
}

// Compile-time guarantee the helpers we use are wired.
var _ = slog.Default

// classifyDLQReason maps the gateway's per-record outcome onto
// one of trigger_dead_letter.reason's CHECK values
// (migrations/00296_triggers.sql::trigger_dead_letter.reason_check).
//
// Audit finding #8: the prior code hardcoded 'max_attempts' for
// every Status='dead_letter' record AND substring-matched the
// Error field — both brittle. Substring matching broke the moment
// the gateway added a new error path with overlapping words
// (e.g. "timeout" matching "broker timeout" but also "request
// timeout from client").
//
// The Code field (added in audit #8) is the structured counterpart
// the gateway emits — a stable machine-readable string the dispatch
// tick switches on. Real causes the gateway distinguishes today
// (pkg/gateway/synth.go::handleInvocationDispatchBatch):
//
//	"payload_b64_invalid"   → poison_record
//	"response_malformed"    → poison_record
//	"invoke_error"          → broker_error
//	"function_failed"       → max_attempts (the function decided
//	                                   to give up — it called
//	                                   back into the report with
//	                                   the same id which means it
//	                                   has exhausted its own
//	                                   retries)
//	"function_state_<X>"    → max_attempts (function returned
//	                                   non-succeeded state)
//	"" (empty, legacy gw)   → fall through to substring match
//	                            on Error for back-compat
//
// The substring fallback only fires for older gateway versions
// that pre-date the Code field. New deployments always go through
// the structured switch.
func classifyDLQReason(code, gatewayErr string) string {
	if code != "" {
		switch code {
		case "function_failed":
			return triggerReasonMaxAttempts
		case "function_state_timeout",
			"function_state_failed",
			"function_state_killed",
			"function_state_lost":
			return triggerReasonMaxAttempts
		case "payload_b64_invalid",
			"response_malformed":
			return triggerReasonPoisonRecord
		case "invoke_error":
			return triggerReasonBrokerError
		default:
			return triggerReasonPoisonRecord
		}
	}
	// Substring fallback for older gateways without a Code field.
	if gatewayErr == "" {
		return triggerReasonPoisonRecord
	}
	switch {
	case strings.Contains(gatewayErr, "batchItemFailures"):
		return triggerReasonMaxAttempts
	case strings.Contains(gatewayErr, "timeout"):
		return triggerReasonMaxAttempts
	case strings.Contains(gatewayErr, "malformed"),
		strings.Contains(gatewayErr, "broker_error"),
		strings.Contains(gatewayErr, "payload_b64"):
		return triggerReasonPoisonRecord
	default:
		return triggerReasonPoisonRecord
	}
}

// emitAudit writes an audit row via the loop's audit.Auditor.
// Nil-safe (no-ops if the Loop has no Auditor wired — keeps tests
// quiet).
//
// Trigger audit kinds are emitted as generic events via the
// Auditor's typed path. The Auditor writes a single events row
// per call (pkg/audit/audit.go mirrors pkg/events.Platform's
// best-effort semantics).
//
// ctx is threaded through so the call honours the dispatch tick's
// lifecycle (CI lint: contextcheck). pkg/audit.Auditor.Emit
// already accepts context.
func (l *Loop) emitAudit(ctx context.Context, ev events.WakeEvent) {
	if l == nil || l.audit == nil || ev == nil {
		return
	}
	l.audit.Emit(ctx, ev.Kind(), nil, ev.Payload())
}
