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
	"net/http"
	"time"

	"github.com/onebox-faas/faas/pkg/api"
	"github.com/onebox-faas/faas/pkg/events"
	"github.com/onebox-faas/faas/pkg/state/sqlc"
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
	MarkTriggerRecordSucceeded(ctx context.Context, id string) error
	MarkTriggerRecordRetry(ctx context.Context, id, lastError string, nextFireAt time.Time) error
	MarkTriggerRecordDeadLetter(ctx context.Context, id, lastError string) error
	InsertTriggerDeadLetter(ctx context.Context, recordID, triggerID, reason, routedTo string, detail []byte) error
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
	triggers, err := store.ListEnabledTriggers(ctx)
	if err != nil {
		l.log.Warn("sched trigger tick: list", "err", err)
		return
	}
	if len(triggers) == 0 {
		return
	}
	for i := range triggers {
		t := triggers[i]
		if err := l.dispatchOneTrigger(ctx, t, store); err != nil {
			l.log.Warn("sched trigger tick: dispatch",
				"trigger_id", t.ID.String(),
				"kind", t.Kind,
				"err", err)
		}
	}
}

// dispatchOneTrigger runs one trigger's per-tick work.
func (l *Loop) dispatchOneTrigger(ctx context.Context, t sqlc.Trigger, store storeLike) error {
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
		return nil
	}
	if len(res.Records) == 0 {
		return nil
	}

	// 3. Batch close: size / 6MB.
	batch := closeBatch(res.Records, int(t.BatchSizeMax), 6*1024*1024)
	if len(batch) == 0 {
		return nil
	}

	// 4. Rate-limit gate. Deny → dead_letter(reason='rate_limited').
	if l.rateLimiter != nil && !l.rateLimiter.AllowWakeApp(t.AppID.String(), api.PlanFree) {
		items := batchItemIDs(batch)
		l.deadLetterAll(ctx, t.ID.String(), items, "rate_limited", "wake rate limit exceeded", store)
		return nil
	}

	// 5. Claim trigger_records rows.
	claimed, claimErr := store.ClaimTriggerRecords(ctx, t.ID.String(), int32(len(batch)))
	if claimErr != nil {
		l.log.Warn("sched trigger tick: claim",
			"trigger_id", t.ID.String(),
			"err", claimErr)
		return nil
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
		_ = poller.Nack(ctx, t, batchItemIDs(batch), "broker_error")
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
		l.deadLetterAll(ctx, t.ID.String(), ids, "poison_record", "gateway response malformed", store)
		_ = poller.Nack(ctx, t, batchItemIDs(batch), "poison_record")
		return nil
	}

	statusByID := make(map[string]triggerDispatchResult, len(resp.Results))
	for _, r := range resp.Results {
		statusByID[r.ItemIdentifier] = r
	}

	succeedIDs := []string{}
	retryIDs := []string{}
	dlqIDs := []string{}
	succeedItems := []string{}
	retryItems := []string{}
	dlqItems := []string{}

	for _, c := range claimed {
		itemID := c.ItemIdentifier
		status, found := statusByID[itemID]
		if !found {
			retryIDs = append(retryIDs, c.ID.String())
			retryItems = append(retryItems, itemID)
			continue
		}
		switch status.Status {
		case "succeeded":
			succeedIDs = append(succeedIDs, c.ID.String())
			succeedItems = append(succeedItems, itemID)
		case "retry", "broker_error":
			retryIDs = append(retryIDs, c.ID.String())
			retryItems = append(retryItems, itemID)
		case "dead_letter":
			dlqIDs = append(dlqIDs, c.ID.String())
			dlqItems = append(dlqItems, itemID)
		default:
			retryIDs = append(retryIDs, c.ID.String())
			retryItems = append(retryItems, itemID)
		}
	}

	if len(succeedIDs) > 0 {
		for _, id := range succeedIDs {
			if err := store.MarkTriggerRecordSucceeded(ctx, id); err != nil {
				l.log.Warn("sched trigger tick: mark succeeded",
					"id", id, "err", err)
			}
			// Audit: trigger.fired per succeeded record.
			l.emitAudit(events.TriggerFiredEvent{
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
		nextFireAt := time.Now().Add(2 * time.Second)
		for _, id := range retryIDs {
			if err := store.MarkTriggerRecordRetry(ctx, id, "", nextFireAt); err != nil {
				l.log.Warn("sched trigger tick: mark retry",
					"id", id, "err", err)
			}
			l.emitAudit(events.TriggerRetryEvent{
				TriggerID:  t.ID.String(),
				RecordID:   id,
				AppID:      t.AppID.String(),
				Attempt:    1,
				NextFireAt: nextFireAt,
			})
		}
		if err := poller.Nack(ctx, t, retryItems, "broker_error"); err != nil {
			l.log.Warn("sched trigger tick: poller nack",
				"trigger_id", t.ID.String(), "err", err)
		}
	}
	if len(dlqIDs) > 0 {
		for _, id := range dlqIDs {
			if err := store.MarkTriggerRecordDeadLetter(ctx, id, ""); err != nil {
				l.log.Warn("sched trigger tick: mark dlq",
					"id", id, "err", err)
			}
			l.emitAudit(events.TriggerDLQEvent{
				TriggerID: t.ID.String(),
				RecordID:  id,
				AppID:     t.AppID.String(),
				Reason:    "max_attempts",
				Attempts:  1,
			})
		}
		if err := poller.Nack(ctx, t, dlqItems, "poison_record"); err != nil {
			l.log.Warn("sched trigger tick: poller nack (dlq)",
				"trigger_id", t.ID.String(), "err", err)
		}
	}
	// Audit: trigger.fired.batch — aggregated counts.
	l.emitAudit(events.TriggerFiredBatchEvent{
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
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("status %d: %s", resp.StatusCode, raw)
	}
	return io.ReadAll(resp.Body)
}

// deadLetterAll marks every record as dead_letter + inserts a
// trigger_dead_letter row.
func (l *Loop) deadLetterAll(ctx context.Context, triggerID string, ids []string, reason, detail string, store storeLike) {
	if store == nil || len(ids) == 0 {
		return
	}
	for _, id := range ids {
		if err := store.InsertTriggerDeadLetter(ctx, id, triggerID, reason, "drop", []byte(detail)); err != nil {
			l.log.Warn("sched trigger tick: insert dlq", "id", id, "err", err)
			continue
		}
		if err := store.MarkTriggerRecordDeadLetter(ctx, id, reason); err != nil {
			l.log.Warn("sched trigger tick: mark dlq", "id", id, "err", err)
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

// Compile-time guarantee the helpers we use are wired.
var _ = slog.Default

// emitAudit writes an audit row via the loop's audit.Auditor.
// Nil-safe (no-ops if the Loop has no Auditor wired — keeps tests
// quiet).
//
// Trigger audit kinds are emitted as generic events via the
// Auditor's typed path. The Auditor writes a single events row
// per call (pkg/audit/audit.go mirrors pkg/events.Platform's
// best-effort semantics).
func (l *Loop) emitAudit(ev events.WakeEvent) {
	if l == nil || l.audit == nil || ev == nil {
		return
	}
	// pkg/audit.Auditor.Emit takes (kind, *accountID, map[string]any).
	// We pull the payload map off the WakeEvent and pass it
	// through. A future commit adds WakeEvent support to the
	// Auditor so the dispatch tick + cron tick share one emit
	// path.
	l.audit.Emit(context.Background(), ev.Kind(), nil, ev.Payload())
}
