// auditor is the IAM-4 (ADR-035) seam that apid handlers call to
// record a security event. It is intentionally thin: a best-effort
// wrapper around state.Store.AppendEvent that logs failures and
// increments the audit_write_failures counter. Mirrors the schedd
// pattern at pkg/sched/engine.go:1317-1329 — a failed audit write
// never rolls back the action that produced it.
//
// The events table is append-only (spec §5) and shared with schedd;
// IAM-4 is the auth-event front-end on the same surface.
//
// Issue #286 added a second emit path, EmitFailedLogin, that goes
// through a buffered channel drained by a single background goroutine.
// ADR-035 explicitly rejected a synchronous INSERT on every failed
// login as a DoS amplifier under credential-stuffing; the async
// flusher is the seal that closes that gap. The downstream emit
// path is the same state.Store.AppendEvent call used by Emit — no
// new SQL surface, no new migration.
package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"time"

	"github.com/onebox-faas/faas/pkg/logsanitize"
	"github.com/onebox-faas/faas/pkg/state"
	"github.com/prometheus/client_golang/prometheus"
)

// auditActor is the value written to events.actor for every IAM-4
// audit row. Free-form text per spec §5; schedd uses "schedd" so the
// convention is <daemon-name>.
const auditActor = "apid"

// KindAuthLoginFailed is the events.kind value written for every
// failed login attempt on the dashboard auth surface (issue #286,
// SOC 2 CC7.2). The discriminator in the data payload is fixed —
// {ip, email_hash, user_agent} — across every emit site so the
// audit reader does not need to disambiguate between auth surfaces.
// The kind is deliberately distinct from the success-path
// "auth.login" so a filters by `kind_prefix=auth.` continue to
// surface both branches in the customer dashboard.
const KindAuthLoginFailed = "auth.login.failed"

// failedLoginChanCapacity is the buffered channel capacity for the
// async-batched failed-login audit path (issue #286). Sized at 4096
// rows — modest enough that the memory footprint stays well below
// the 6 GB control-plane slice, large enough that a single
// credential-stuffing burst that escapes the auth-limit bucket (30/s
// per IP, 10/min across the bucket) does not immediately drop rows
// before the 250 ms flush cadence fires. A non-blocking select with
// the `default` branch as the drop arm means the customer-facing
// 401 is never gated on the channel write.
const failedLoginChanCapacity = 4096

// failedLoginFlushEvery is the cadence at which the flusher goroutine
// drains the channel into batched AppendEvent calls. Sized at 250 ms
// so the audit row appears in the table within a quarter-second of
// the failed attempt — fast enough that an operator triaging a
// FaasFailedLoginSpike alert can read the audit row before the burst
// already moved on.
const failedLoginFlushEvery = 250 * time.Millisecond

// failedLoginFlushBatch is the upper bound on rows the flusher will
// drain in a single batch. Sized at 1000 so a 4096-row burst
// flushes in 4 batches worth of AppendEvent calls (~1 s total at
// 250 ms cadence) which keeps the per-row latency bounded without
// pinning a Postgres connection for hundreds of inserts at once.
const failedLoginFlushBatch = 1000

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

// auditFailedOps is the narrow counter surface the failed-login
// channel needs (issue #286). Same testing pattern as auditOps —
// defined as an interface so the auditor can be unit-tested with a
// stub. The counter values flow through the bounded
// ipLabelSet/audit write set documented on wire.OpsMetrics.
type auditFailedOps interface {
	FailedLoginTotal(ip string) prometheus.Counter
	FailedLoginDropped() prometheus.Counter
}

// failedLoginRow is the in-memory shape that flows through the
// async-batched audit channel. The fields are pre-computed at the
// handler site so the flusher does not need to re-read
// r.Header.Get("User-Agent") etc.; the At timestamp is the
// failure-detection time (not the flush time) so an audit row
// reader can correlate the row with the offending request.
type failedLoginRow struct {
	IP        string
	EmailHash string
	UserAgent string
	At        time.Time
}

// auditor is the IAM-4 audit seam. Constructed once per server in
// newServerWithDeps and held as s.audit. Emit is the synchronous
// path (success-path audit rows); EmitFailedLogin is the async
// path (failed-login audit rows, issue #286).
type auditor struct {
	store state.Store
	log   *slog.Logger
	ops   auditOps

	// failed-login async channel (issue #286). failedCh is the
	// buffered enqueue point; failedOps is the per-IP counter
	// surface; flushEvery / flushBatch are the draining pacer.
	// closeSignal is the wakeup channel Close signals on so the
	// flusher goroutine exits immediately rather than waiting
	// for the next ticker tick on shutdown.
	failedCh    chan failedLoginRow
	closeSignal chan struct{}
	failedOps   auditFailedOps
	flushEvery  time.Duration
	flushBatch  int

	// closeOnce guards double-Close (idempotent on shutdown). wg
	// waits for the flusher goroutine to drain in-flight rows
	// before Close returns, so the daemon can stop on a clean
	// silence after the audit row is in the table.
	closeOnce sync.Once
	wg        sync.WaitGroup
}

// newAuditor builds the helper. nil ops is allowed — Emit will skip
// the counter increment so unit tests can run without an OpsMetrics.
// The async failed-login channel is NOT started here — the caller
// must invoke Start(ctx) after WithOpsMetrics has wired the
// failed-login counter, so the flusher goroutine can talk to a
// fully-constructed OpsMetrics. This split keeps the
// newServerWithDeps → WithOpsMetrics → startFlusher → Close
// lifecycle explicit.
func newAuditor(store state.Store, log *slog.Logger, ops auditOps) *auditor {
	return &auditor{
		store:       store,
		log:         log,
		ops:         ops,
		failedCh:    make(chan failedLoginRow, failedLoginChanCapacity),
		closeSignal: make(chan struct{}),
		flushEvery:  failedLoginFlushEvery,
		flushBatch:  failedLoginFlushBatch,
	}
}

// SetFailedOps wires the failed-login counter surface. Called by
// WithOpsMetrics after the OpsMetrics is constructed (issue #286).
// Mirrors the lazy ops binding pattern used for the success-path
// auditOps — the auditor is constructed at server-build time so the
// handler closure has a non-nil receiver, while the metrics layer
// is wired later so the daemons-without-OpsMetrics test path
// remains a valid call shape.
func (a *auditor) SetFailedOps(ops auditFailedOps) {
	if a == nil {
		return
	}
	a.failedOps = ops
}

// Start spins up the flusher goroutine. Called once per server
// after SetFailedOps. The goroutine terminates when the channel is
// closed via Close.
func (a *auditor) Start(ctx context.Context) {
	if a == nil || a.failedCh == nil {
		return
	}
	a.wg.Add(1)
	go a.runFlusher(ctx)
}

// Close stops the flusher goroutine and blocks until all
// in-flight rows have been drained into the events table. Idempotent
// — calling Close twice is a no-op. The daemon shutdown path
// (cmd/apid/main.go) defers this so SIGTERM leaves the audit table
// consistent with the in-process queue.
func (a *auditor) Close() {
	if a == nil || a.failedCh == nil {
		return
	}
	a.closeOnce.Do(func() {
		close(a.failedCh)
		// Wake the flusher goroutine so the final drain runs
		// immediately rather than waiting for the next ticker
		// tick (up to flushEvery = 250ms in production).
		close(a.closeSignal)
		a.wg.Wait()
	})
}

// Emit writes one events row synchronously. accountID is optional
// (nil allowed for system-level events, e.g. cron-fired). data may
// be nil; marshal into {} on the way down so the column is always
// valid JSON.
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

// EmitFailedLogin is the handler-side entry point for the
// failed-login audit row (issue #286). Non-blocking — the channel
// write either succeeds or the row is dropped into
// apid_failed_login_audit_dropped_total. The handler then returns
// the 401 unconditionally; the audit row is observation, not source
// of truth.
//
// ip is the source IP extracted via pkg/middleware.ClientIP (the
// same loopback+XFF trust contract the auth-limit bucket uses —
// diverging from that contract would silently make a credential-
// stuffing burst look like a different (smaller) attack).
// emailHash is pkg/auth.HashEmail (lowercase+trimmed SHA-256 hex).
// userAgent is the raw inbound User-Agent header — the slog
// warning-on-drop path below runs it through logsanitize.Field so
// the metric label stays free of CRLF/log-injection characters.
func (a *auditor) EmitFailedLogin(ip, emailHash, userAgent string) {
	if a == nil || a.failedCh == nil {
		return
	}
	// Increment the per-IP counter BEFORE the channel write so a
	// dropped row still produces the Prometheus signal. The audit
	// row is observation; the counter is the SOC 2 evidence.
	if a.failedOps != nil {
		a.failedOps.FailedLoginTotal(ip).Inc()
	}
	row := failedLoginRow{
		IP:        ip,
		EmailHash: emailHash,
		UserAgent: userAgent,
		At:        time.Now(),
	}
	select {
	case a.failedCh <- row:
		// Enqueued. The flusher will pick it up on the next tick.
	default:
		// Channel is full. The audit row is observation, NOT
		// source of truth — the auth response is the customer-facing
		// invariant and was already queued before the EmitFailedLogin
		// call. Drop the row, increment the unlabelled drop counter,
		// and log a sanitized Warn so the operator can correlate
		// "apid_failed_login_audit_dropped_total rising" with a
		// slog line carrying the IP.
		if a.failedOps != nil {
			a.failedOps.FailedLoginDropped().Inc()
		}
		a.log.Warn("audit: failed-login channel full, dropped",
			"ip", logsanitize.Field(ip))
	}
}

// runFlusher is the single-goroutine drain loop for the async
// failed-login audit channel (issue #286). One loop per daemon;
// the channel is closed at daemon shutdown and the loop exits naturally.
//
// Cadence: every flushEvery (250 ms) OR whenever the channel has
// flushBatch (1000) rows buffered, whichever fires first. The size
// threshold is checked at the top of every iteration so a soak-load
// burst can't outrun the timer.
//
// On every drain:
//  1. Read up to flushBatch rows off the channel.
//  2. For each row, AppendEvent one row of events. We don't batch
//     into a single multi-row INSERT — AppendEvent is one row in
//     the state.Store contract, and a per-row call shape keeps the
//     schema upgrade path local (this is the same contract Emit
//     uses; no new SQL surface).
//  3. On AppendEvent failure, log Warn and increment the audit
//     failure counter — same posture as Emit. The row is dropped
//     from the audit surface; the Prometheus counter has already
//     counted it (EmitFailedLogin ran before the channel send).
//
// runFlusher is the single-goroutine drain loop for the async
// failed-login audit channel (issue #286). One loop per daemon;
// the channel is closed at daemon shutdown and the loop exits naturally.
//
// Cadence: the loop waits on the ticker + a wakeup channel. The
// ticker is the low-traffic bound (flushEvery=250ms — the audit
// row appears within a quarter second of the failed attempt even
// when the request rate is zero). A separate wakeup channel lets
// Close notify the goroutine that the queue is closed so the
// daemon shutdown path doesn't wait up to flushEvery for the
// final drain.
//
// The channel receive on a.failedCh is folded into drainFlusher
// — a naive "receive-and-discard" in the select would drop the
// row on the floor. drainFlusher is the only place that pops rows.
//
// On ctx.Done (daemon shutdown), Close() closes the channel AND
// sends on the wakeup channel. The loop's drainFlusher sees the
// `!ok` and exits. The wg.Wait in Close blocks until runFlusher
// has returned.
func (a *auditor) runFlusher(ctx context.Context) {
	defer a.wg.Done()
	ticker := time.NewTicker(a.flushEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			a.drainFlusher()
			return
		case <-a.closeSignal:
			// Channel was closed. The final drain picks up any
			// rows still buffered (drainFlusher's `case row, ok
			// := <-a.failedCh` returns on `!ok`).
			a.drainFlusher()
			return
		case <-ticker.C:
			// Cadence tick. Drain whatever is buffered so the
			// audit row latency stays bounded even when the
			// request rate is low.
			a.drainFlusher()
		}
	}
}

// drainFlusher reads up to a.flushBatch rows off the channel and
// writes one events row per read. Called from runFlusher (size
// threshold OR tick) and from the ctx.Done branch (shutdown drain).
// Best-effort — a failed AppendEvent logs Warn and increments the
// audit failure counter; the row is dropped from the audit surface.
// The per-IP counter has already been incremented by EmitFailedLogin
// at enqueue time, so the SOC 2 counter is unaffected by audit-write
// failures.
func (a *auditor) drainFlusher() {
	for i := 0; i < a.flushBatch; i++ {
		select {
		case row, ok := <-a.failedCh:
			if !ok {
				// Channel closed. The loop in runFlusher has
				// already exited; this drain is the final
				// pass from the shutdown path.
				return
			}
			a.flushOne(row)
		default:
			// Channel empty. The drain is done for this tick.
			return
		}
	}
}

// flushOne writes one failed-login audit row via the same
// AppendEvent path Emit uses. Subject is nil because the failed
// login cannot be attributed to a known account id (the email
// hash is the joinable seam, not the on-disk row subject). The
// best-effort posture is identical to Emit — Warn + counter on
// failure, never roll back the auth response (which has already
// been returned by the time we get here).
func (a *auditor) flushOne(row failedLoginRow) {
	data := map[string]any{
		"ip":         row.IP,
		"email_hash": row.EmailHash,
		"user_agent": row.UserAgent,
	}
	payload, err := json.Marshal(data)
	if err != nil {
		// map[string]any of strings cannot fail marshalling.
		// Treat as a programmer bug — log Error and skip.
		a.log.Error("audit: failed-login marshal", "ip", logsanitize.Field(row.IP), "err", err)
		return
	}
	// Use the row's At timestamp as the audit-row at value via
	// a derived context. AppendEvent does not accept a timestamp
	// parameter — the events row's at column is server-side
	// now(). We accept the tiny drift (handler-timestamp vs.
	// INSERT-now) as the trade-off for not changing the
	// AppendEvent signature.
	flushCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	start := time.Now()
	err = a.store.AppendEvent(flushCtx, auditActor, KindAuthLoginFailed, nil, payload)
	dur := time.Since(start)
	cancel()
	if a.ops != nil {
		if err != nil {
			a.ops.AuditWriteFailureDuration("failed").Observe(dur.Seconds())
		} else {
			a.ops.AuditWriteFailureDuration("ok").Observe(dur.Seconds())
		}
	}
	if err != nil {
		a.log.Warn("audit: failed-login append event",
			"ip", logsanitize.Field(row.IP),
			"err", err)
		if a.ops != nil {
			// Failure semantics mirror Emit: the row's subject
			// is nil so the counter is labelled by the empty
			// string, which accountLabel() maps to "anonymous".
			// Operators drill into the slog for the IP.
			a.ops.AuditWriteFailures("").Inc()
		}
	}
}
