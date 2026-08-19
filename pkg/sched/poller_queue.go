// poller_queue.go — in-platform queue / delayed-task poller
// (issue #757 / ADR-0NN, commit #8 of feat-triggers-mega).
//
// The "queue" trigger kind is the unification of two pre-existing
// surfaces:
//
//   - per-app FIFO queue (source='queue' in invocations)
//   - delayed tasks    (source='delayed_task' in invocations)
//
// Both fan through the unified `invocations` table (drain.go).
// The trigger kind=queue poller bridges that pre-existing queue
// into the new trigger envelope: it reads rows where
// (app_id, source) matches the trigger's (kind='queue', source='queue'
// or 'delayed_task') and exposes them as SourceRecord slices.
//
// Why a poller at all if the rows already live in `invocations`?
// The dispatch tick (commit #14) is the only consumer that needs
// the new batching + ReportBatchItemFailures machinery. We can't
// route every `invocations` row through that machinery (existing
// async_invoke traffic keeps its single-record semantics), so the
// poller is the seam that decides which rows opt in.
//
// Ack semantics are a no-op: the underlying `invocations` rows
// stay in state='pending' (they were already committed before the
// poller saw them; we don't translate them into the trigger FSM's
// succeed transition). The trigger_records rows we mint in commit
// #14 carry the operator-facing "did this trigger fire it?"
// answer; the in-platform queue's own persistence is the
// `invocations` table.
//
// Nack is a no-op for the same reason — the underlying broker
// (the postgres queue) is durable by definition; redelivery would
// just create a duplicate 'retry' row.

package sched

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/onebox-faas/faas/pkg/state/sqlc"
)

// queuePoller is the kind=queue trigger's broker adapter. It holds
// a pgxpool connection (the schedd is a long-lived daemon; the pool
// is shared with the rest of the sched).
//
// Each Queue trigger gets its own queuePoller instance because the
// Poll query uses the trigger's app_id + source filter — the
// connection state is shared (the pool), but the per-trigger
// bindings live on the instance.
type queuePoller struct {
	pool   *pgxpool.Pool
	source string

	// mu protects itemsInFlight — the dispatcher passes item
	// identifiers to Ack/Nack and we record them here so a
	// subsequent Poll sees consistent state. In practice this is
	// a no-op (Ack/Nack are no-ops today) but the field is in
	// place for when a future queue-poller variant (e.g. an
	// external Postgres with a separate CDC log) needs to track
	// in-flight items to dedupe.
	mu            sync.Mutex
	itemsInFlight map[string]struct{}
}

// newQueuePoller constructs the queue poller for a kind=queue
// trigger. Returns an error when the trigger's source field is
// missing or set to anything other than 'queue' / 'delayed_task' —
// the SQL trigger.kind='queue' CHECK permits both, but the poller
// needs to know which partition it belongs to.
func newQueuePoller(pool *pgxpool.Pool, t sqlc.Trigger) (triggerSource, error) {
	if !t.Source.Valid {
		return nil, fmt.Errorf("poller_queue: trigger missing source")
	}
	if t.Source.String != "queue" && t.Source.String != "delayed_task" {
		return nil, fmt.Errorf("poller_queue: unsupported source %q", t.Source.String)
	}
	q := &queuePoller{
		pool:          pool,
		source:        t.Source.String,
		itemsInFlight: map[string]struct{}{},
	}
	return q, nil
}

// Kind returns the trigger kind this poller handles. The
// dispatcher pairs triggers with pollers by matching on this value.
func (q *queuePoller) Kind() string { return "queue" }

// Poll reads the next batch of `invocations` rows whose (app_id,
// source) matches this trigger, ordered by created_at ASC. The
// dispatcher honours batch_size_max AFTER this returns — we
// always pull up to the broker-natural default of 256 (matches the
// old queue receive batch size from drain.go).
//
// Returned SourceRecord.ItemIdentifier is the invocation id (a
// UUID); the dispatch tick uses it for Ack/Nack bookkeeping but
// both methods are no-ops for this kind.
func (q *queuePoller) Poll(ctx context.Context, t sqlc.Trigger) PollResult {
	const pollLimit = 256
	rows, err := q.pool.Query(ctx,
		`select id, payload::text, headers::text, metadata::text,
		        created_at
		   from invocations
		  where app_id = $1
		    and source = $2
		    and outcome is null
		    and completed_at is null
		  order by created_at asc
		  limit $3`,
		t.AppID, q.source, pollLimit,
	)
	if err != nil {
		return PollResult{Error: fmt.Errorf("poller_queue: query invocations: %w", err)}
	}
	defer rows.Close()
	out := make([]SourceRecord, 0, pollLimit)
	for rows.Next() {
		var (
			idStr     string
			payload   string
			headers   string
			metadata  string
			createdAt pgtype.Timestamptz
		)
		if err := rows.Scan(&idStr, &payload, &headers, &metadata, &createdAt); err != nil {
			return PollResult{Error: fmt.Errorf("poller_queue: scan: %w", err)}
		}
		out = append(out, SourceRecord{
			ItemIdentifier: idStr,
			Payload:        []byte(payload),
			Headers:        parseJSONHeaders(headers),
			Metadata:       parseJSONMetadata(metadata),
			ReceivedAt:     createdAt.Time,
		})
	}
	if err := rows.Err(); err != nil {
		return PollResult{Error: fmt.Errorf("poller_queue: rows iter: %w", err)}
	}
	// Track in-flight items so a subsequent Ack/Nack on the same
	// trigger sees a consistent set.
	q.mu.Lock()
	for _, r := range out {
		q.itemsInFlight[r.ItemIdentifier] = struct{}{}
	}
	q.mu.Unlock()
	return PollResult{Records: out}
}

// Ack is a no-op for the in-platform queue. The underlying
// `invocations` row stays in place (it's persisted in Postgres;
// nothing the broker can forget about). The dispatcher removes
// it from in-flight tracking and writes the post-dispatch state
// (success/retry/dead_letter) on the trigger_records table (or
// for kind=queue, simply updates `invocations.outcome` and
// `invocations.completed_at`).
func (q *queuePoller) Ack(_ context.Context, _ sqlc.Trigger, ids []string) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	for _, id := range ids {
		delete(q.itemsInFlight, id)
	}
	return nil
}

// Nack is a no-op for the in-platform queue. The row is durable in
// Postgres; we can't "redeliver" it. The dispatcher's retry FSM
// will mint a new trigger_records row (commit #14) with
// attempts++ and next_fire_at bumped.
func (q *queuePoller) Nack(_ context.Context, _ sqlc.Trigger, ids []string, _ string) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	for _, id := range ids {
		delete(q.itemsInFlight, id)
	}
	return nil
}

// Close releases nothing — the pgxpool is owned by the sched, not
// this poller. Kept for the triggerSource interface contract.
func (q *queuePoller) Close() error {
	return nil
}

// parseJSONHeaders turns a Postgres jsonb-encoded headers blob
// into a map[string]string. The default is empty for NULL
// payloads. A JSON decode error is intentionally swallowed — a
// malformed header is one record's problem, not the batch's.
func parseJSONHeaders(s string) map[string]string {
	if s == "" {
		return nil
	}
	out := map[string]string{}
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		return nil
	}
	return out
}

// parseJSONMetadata same as parseJSONHeaders but the value shape
// is map[string]any.
func parseJSONMetadata(s string) map[string]any {
	if s == "" {
		return nil
	}
	out := map[string]any{}
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		return nil
	}
	return out
}

// currentLoopPool is set once at schedd startup by Loop.New so the
// init-time factory closure can reach the pool without each Loop
// having to thread it through the registry.
var currentLoopPool *pgxpool.Pool

// setCurrentLoopPool is invoked from Loop.New at schedd startup.
// Race-free at startup (called once before any Poll happens).
//
//nolint:unused // reserved for cmd/schedd boot wiring (PR-B).
func setCurrentLoopPool(p *pgxpool.Pool) { currentLoopPool = p }

// getCurrentLoopPool returns the pool a Loop registered at
// startup, or nil if no Loop has booted. The dispatcher treats
// nil as "this sched has no pool yet" and skips until set.
func getCurrentLoopPool() *pgxpool.Pool { return currentLoopPool }

func init() {
	registerPoller("queue", func(t sqlc.Trigger) (triggerSource, error) {
		pool := getCurrentLoopPool()
		if pool == nil {
			return nil, fmt.Errorf("poller_queue: no pool registered for schedd loop")
		}
		return newQueuePoller(pool, t)
	})
}
