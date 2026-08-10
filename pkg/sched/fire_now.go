// pkg/sched/fire_now.go — schedd's fire-now consumer (ADR-090 PR-C).
//
// Producer: apid's POST /v1/crons/{id}/run handler inserts a row into
// cron_fire_now_requests (migrations/00193, status='pending') and emits
// db.NotifyCronRunNow on the wire.
//
// Consumer: this goroutine.
//
//   - LISTENs on db.NotifyCronRunNow via SubscribeWithReconnect.
//   - On each delivery (and on every 60s safety tick), calls
//     ClaimPendingFireNowRequest which atomically transitions one
//     pending row to `running` via FOR UPDATE SKIP LOCKED LIMIT 1.
//   - Calls (*Loop).RunCronNow (the canonical "fire this cron" helper
//     extracted from dispatchOneCron in PR-C step 1) to dispatch
//     the request.
//   - Stamps the row's terminal state via MarkFireNowRequestSucceeded
//     / MarkFireNowRequestFailed.
//
// Why a dedicated goroutine instead of another ticker arm in loop.go:
// loop.go is a 350-line switch with strict tick + watch responsibilities
// (spec §6.1) and adding a 13th arm bloats every reader's mental model.
// A dedicated goroutine + ctx-driven cancellation matches the existing
// placement_claim.go / retention.go pattern (pkg/sched/<feature>.go).

package sched

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/onebox-faas/faas/pkg/db"
	"github.com/onebox-faas/faas/pkg/state"
)

// fireNowSafetyTick is the recovery cadence for missed-notify scenarios.
// When a NotifyCronRunNow delivery is dropped (Postgres bounce, network
// blip, schedd restart), the pending rows in the table survive — the
// safety tick re-claims them. 60s matches cronT's cadence; a customer
// waiting on a fire-now that dropped during the blip waits up to 60s
// for recovery. Acceptable for an operator-initiated action.
const fireNowSafetyTick = 60 * time.Second

// fireNowSubscriberPayload is the JSON shape apid emits on
// NotifyCronRunNow. The request_id is informational — the row IS the
// source of truth, and ClaimPendingFireNowRequest selects the oldest
// pending row regardless of which id the notify carried. This matches
// the build_queued pattern (pkg/db/notify.go:88-90 + cmd/imaged
// consumer: subscriber re-reads the row to defend against notify loss).
type fireNowSubscriberPayload struct {
	RequestID string `json:"request_id"`
}

// FireNowRun starts the fire-now subscriber. Blocks until ctx is
// cancelled. The function is the canonical wire-up site for the
// cmd/schedd main goroutine; loop.go's Run() does not call this —
// the binary wires it explicitly so tests can opt out.
//
// Returns ctx.Err() on cancellation (a clean exit) or a wrapped
// error from the initial SubscribeWithReconnect failure (a fatal
// boot error).
func (l *Loop) FireNowRun(ctx context.Context) error {
	notif, err := db.SubscribeWithReconnect(ctx, l.pool, []string{db.NotifyCronRunNow}, l.log)
	if err != nil {
		return fmt.Errorf("sched: fire_now: subscribe: %w", err)
	}

	safetyT := time.NewTicker(fireNowSafetyTick)
	defer safetyT.Stop()

	l.log.Info("sched: fire_now: subscriber online", "channel", db.NotifyCronRunNow)

	// Drain pending rows on startup. A schedd bounce with rows in
	// `pending` would otherwise wait up to fireNowSafetyTick for the
	// first recovery tick — a 60s gap on a customer-visible API.
	// One-shot drain keeps the post-restart latency bounded to the
	// first claim round-trip.
	l.drainPendingFireNowRequests(ctx)

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-safetyT.C:
			l.drainPendingFireNowRequests(ctx)
		case n, ok := <-notif:
			if !ok {
				// SubscribeWithReconnect guarantees the outer channel
				// only closes on ctx done; if it does close, treat as
				// fatal and exit so the daemon restarts.
				return errors.New("sched: fire_now: subscriber channel closed unexpectedly")
			}
			// Informational payload — the row is the truth. Drain
			// handles everything; the notify is just a wakeup.
			var p fireNowSubscriberPayload
			if err := json.Unmarshal([]byte(n.Payload), &p); err != nil {
				l.log.Warn("sched: fire_now: malformed notify payload", "err", err, "payload", n.Payload)
			}
			l.drainPendingFireNowRequests(ctx)
		}
	}
}

// drainPendingFireNowRequests claims + dispatches every pending row,
// one at a time. A single drain call claims all currently-pending
// rows sequentially; a follow-up notify or tick will pick up anything
// that arrives mid-drain. The single-row LIMIT is the contention
// primitive — N schedd instances process N rows in parallel without
// colliding.
//
// The function does not return an error; per-row failures are logged
// and the row is stamped to `failed` so a poison row cannot wedge the
// queue forever.
func (l *Loop) drainPendingFireNowRequests(ctx context.Context) {
	for {
		req, err := l.engine.Store().ClaimPendingFireNowRequest(ctx)
		if errors.Is(err, state.ErrFireNowRequestNotFound) {
			return // empty queue — caller exits cleanly
		}
		if err != nil {
			l.log.Warn("sched: fire_now: claim failed", "err", err)
			return // transient error; the safety tick retries
		}
		l.processFireNowRequest(ctx, req)
	}
}

// processFireNowRequest is the per-row dispatch. Loads the cron
// (defence in depth — the cron may have been disabled/deleted between
// INSERT and claim), calls RunCronNow, stamps the terminal state.
// Errors are mapped to status: ErrCronDisabled / ErrAccountSuspended /
// ErrNoCapacity → failed; anything else → failed with the err.Error()
// text capped at 1 KB.
func (l *Loop) processFireNowRequest(ctx context.Context, req state.FireNowRequest) {
	run, err := l.RunCronNow(ctx, req.CronID, req.AccountID)

	switch {
	case err == nil:
		// Best-effort: RunCronNow returns CronRun{Success: true} with
		// InvocationID="" when the dispatch path took the
		// "enqueued" route (the cron tick + invoke are decoupled). We
		// still mark succeeded because the fire was accepted — the
		// invocation_id, when present, lets the customer correlate
		// the audit row with /v1/invocations/{id}.
		if err := l.engine.Store().MarkFireNowRequestSucceeded(ctx, req.ID, run.InvocationID); err != nil {
			l.log.Warn("sched: fire_now: mark succeeded failed",
				"request_id", req.ID, "err", err)
		}
		l.log.Info("sched: fire_now: succeeded", "request_id", req.ID, "cron_id", req.CronID)

	case errors.Is(err, ErrCronDisabled):
		if mErr := l.engine.Store().MarkFireNowRequestFailed(ctx, req.ID, "cron disabled"); mErr != nil {
			l.log.Warn("sched: fire_now: mark failed", "request_id", req.ID, "err", mErr)
		}
		l.log.Info("sched: fire_now: cron disabled", "request_id", req.ID)

	case errors.Is(err, ErrAccountSuspended):
		if mErr := l.engine.Store().MarkFireNowRequestFailed(ctx, req.ID, "account suspended"); mErr != nil {
			l.log.Warn("sched: fire_now: mark failed", "request_id", req.ID, "err", mErr)
		}
		l.log.Info("sched: fire_now: account suspended", "request_id", req.ID)

	default:
		// Unknown error. Stamp as failed with the err string so the
		// customer's `GET /v1/crons/{id}/runs` shows the failure
		// reason. MarkFireNowRequestFailed caps the string at 1 KB.
		msg := err.Error()
		if mErr := l.engine.Store().MarkFireNowRequestFailed(ctx, req.ID, msg); mErr != nil {
			l.log.Warn("sched: fire_now: mark failed", "request_id", req.ID, "err", mErr)
		}
		l.log.Warn("sched: fire_now: dispatch error",
			"request_id", req.ID, "cron_id", req.CronID, "err", err)
	}
}

// _ = slog.Default // keep the slog import in case future logging
// helpers want a default logger; remove on next edit if unused.
var _ = slog.Default
