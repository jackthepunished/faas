package meter

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/onebox-faas/faas/pkg/billing"
	"github.com/onebox-faas/faas/pkg/billing/paddle"
	"github.com/onebox-faas/faas/pkg/billing/stripe"
	"github.com/onebox-faas/faas/pkg/state"
	"github.com/onebox-faas/faas/pkg/wire"
)

// Pusher is the meterd daemon's billing-pusher loop. It walks every paid
// account with a customer id (Stripe cus_… or Paddle ctm_…), sums the past
// billing window's billable mb_seconds, and pushes a single metered usage
// record. The active provider (FAAS_BILLING_PROVIDER=paddle for Paddle, "" or
// "stripe" for Stripe) is dispatched through billing.Provider — the local
// StripePusher interface that predated PR #3 was collapsed into the Provider
// interface so the same pusher loop runs on either provider.
//
// Stripe's API idempotency-key (per (account, hour)) sits on top of the SDK
// call so a retry is safe; meterd's own loop is at-least-once too — a
// redelivered window is just a duplicate that the provider collapses.
//
// The production cadence is one push per day (cfg.StripeInterval =
// 24h). The pusher reads usage_minutes across the past 24h window and
// hands the integer sum to the SDK; the SDK converts to wire units
// with one deterministic integer division. The historical per-hour
// float path was retired in M7 §14 — see docs/STATUS.md and the
// acceptance test in pkg/billing/stripe/sandbox_test.go.
type Pusher struct {
	store  state.Store
	pusher billing.Provider
	log    *slog.Logger
	now    func() time.Time
	ops    *wire.OpsMetrics
}

// NewPusher wires the pusher. now defaults to time.Now if nil so callers
// in production can leave it blank; tests inject a clock. ops defaults
// to a fresh test registry when nil so existing tests don't have to
// construct one — mirrors Loop.NewLoop's nil coercion.
func NewPusher(store state.Store, pusher billing.Provider, log *slog.Logger, now func() time.Time, ops *wire.OpsMetrics) *Pusher {
	if now == nil {
		now = time.Now
	}
	if ops == nil {
		ops = wire.NewOpsMetrics("meter_test")
	}
	return &Pusher{store: store, pusher: pusher, log: log, now: now, ops: ops}
}

// HourWindow returns the [start, end) hour boundary the pusher aggregates
// against. end is exclusive so a tick at 14:00:00 covers 13:00–14:00. The
// caller (PushHour) reads usage rows whose minute ∈ [start, end).
func HourWindow(at time.Time) (start, end time.Time) {
	start = at.UTC().Truncate(time.Hour).Add(-time.Hour)
	end = at.UTC().Truncate(time.Hour)
	return start, end
}

// PushHour pushes the billable mb_seconds for one billing window for
// every paid account. Free accounts are skipped (no customer id, no
// overage). Returns the number of accounts it pushed for so the loop
// can log a line; errors push loop backoff decisions up to the caller.
//
// TODO(M7-followup): rename to PushWindow (or similar) once
// pkg/meter/loop.go is updated to a daily cadence. The function name
// is kept as PushHour for historical / interface consistency with the
// loop driver; the underlying billing window is HourWindow(now), which
// spans the past 24h under the production cadence
// (cfg.StripeInterval = 24h). The integer mb_seconds sum is handed to
// the SDK which converts to wire units in pure int64 arithmetic.
//
// Each non-skip SDK call is observed under the "stripe" op with a code
// label from stripe.ClassifyPushError — "ok" on success, a stable
// failure-mode label (card-error, rate-limit, invalid-request, etc.)
// on failure. The skip branches (mbSec <= 0, free plan, suspended,
// missing usage rows) are silent so the dashboard distinguishes
// "did nothing" from "tried and the provider bounced it".
//
// Paddle's CreateUsageRecord is the parallel surface — PR #3 wires the
// loop dispatch through billing.Provider so the same body drives
// either SDK. The per-provider classify/observe pair
// (pusherDispatch, classifyProviderError — below) is the dispatch
// seam: each provider gets a stable label set and a parallel
// `_paddle_push_duration_seconds` histogram so the dashboard panels
// for Paddle and Stripe render with the same grain. The classifiers
// (stripe.ClassifyPushError, paddle.ClassifyPushError) are
// SDK-typed and live in each provider's package so the pusher
// doesn't depend on either SDK's internal error shape.
func (p *Pusher) PushHour(ctx context.Context) (int, error) {
	if p.pusher == nil {
		return 0, errors.New("meter: billing pusher not configured")
	}
	now := p.now()
	start, end := HourWindow(now)
	accounts, err := p.store.ListAllAccounts(ctx)
	if err != nil {
		return 0, err
	}
	// Snapshot the provider dispatch once so the per-account loop
	// doesn't re-type-switch on every iteration. opLabel is stamped
	// on ops_total and the per-provider PushDuration histogram; it
	// doesn't change per-account so hoisting is safe.
	opLabel, pushDurObs := pusherDispatch(p.pusher)
	pushed := 0
	for _, acct := range accounts {
		if acct.Plan == "free" {
			// Free plan: no customer id, no overage — skip.
			continue
		}
		if acct.Status == state.AccountSuspended || acct.Status == state.AccountDeletedPending {
			continue
		}
		// The provider impl's PushUsageRecord is the source-of-truth skip
		// when an account has no customer id (stripe.Client::PushUsageRecord
		// returns nil silently on empty cus_… — see
		// pkg/billing/stripe/client.go:117-125). We forward every
		// non-Free, non-Suspended/DeletedPending account so the SDK
		// gets a chance to log the skip + the dedupe record stays
		// consistent for both providers.
		rows, err := p.store.UsageByHour(ctx, acct.ID, start, end)
		if err != nil {
			p.log.Warn("meter: usage_by_hour", "account", acct.ID, "err", err)
			continue
		}
		var mbSec int64
		for _, u := range rows {
			mbSec += u.MBSeconds
		}
		if mbSec <= 0 {
			continue
		}
		pushStart := time.Now()
		perr := p.pusher.PushUsageRecord(ctx, acct, start, mbSec)
		code := classifyProviderError(p.pusher, perr)
		dur := time.Since(pushStart)
		p.ops.ObserveCode(opLabel, code, dur)
		pushDurObs(p.ops, code, dur)
		if perr != nil {
			p.log.Warn("meter: push usage", "account", acct.ID, "hour", start,
				"code", code, "err", perr)
			continue
		}
		pushed++
	}
	return pushed, nil
}

// pusherDispatch returns the per-provider observation seam
// (opLabel + PushDuration observer) for a billing.Provider. The
// dispatcher is intentionally SDK-typed locally — the Provider
// interface is SDK-agnostic by design (ADR-025), but the dispatch
// itself needs to know which SDK to call. Two SDK shapes today
// (Stripe, Paddle); a new provider would extend the switch.
//
// Returns ("stripe", StripePushDuration observer) for *stripe.Client;
// ("paddle", PaddlePushDuration observer) for *paddle.Provider; falls
// back to ("stripe", StripePushDuration observer) for an unknown
// provider so a future addition doesn't silently lose
// observations. A new provider PR should add a case here + a new
// histogram in metrics.go.
//
// The signature uses a closure so the per-provider observer captures
// the SDK-typed selector method; the alternative — returning a
// method value — would force the pusher to know the method name
// ahead of dispatch.
func pusherDispatch(prov billing.Provider) (opLabel string, obs func(*wire.OpsMetrics, string, time.Duration)) {
	switch prov.(type) {
	case *stripe.Client:
		return "stripe", func(m *wire.OpsMetrics, code string, d time.Duration) {
			m.StripePushDuration(code).Observe(d.Seconds())
		}
	case *paddle.Provider:
		return "paddle", func(m *wire.OpsMetrics, code string, d time.Duration) {
			m.PaddlePushDuration(code).Observe(d.Seconds())
		}
	default:
		// Unknown provider — fall back to the Stripe observer so
		// observations are not silently dropped. The op label stays
		// "stripe" because the histogram name encodes it; renaming it
		// here would require a new histogram in metrics.go.
		return "stripe", func(m *wire.OpsMetrics, code string, d time.Duration) {
			m.StripePushDuration(code).Observe(d.Seconds())
		}
	}
}

// classifyProviderError maps a pusher-side error to the per-provider
// closed label set. The dispatch goes through the
// billing.Classifier optional interface (declared at pkg/billing/provider.go)
// so SDK-typed classification stays in the provider's own package
// (which knows about *stripe.Error / *paddleerr.Error) without
// forcing billing.Provider wider. Test doubles can also implement
// Classifier to feed synthetic errors through the same path.
//
// nil → "ok" unconditionally (the closed label set's success label
// is identical for both providers — the dashboard panel joins on
// `code="ok"`). A provider that doesn't implement Classifier → "other"
// so the dashboard panel doesn't render "no data" for a misconfigured
// env. The label set is the per-provider closed list — see
// stripe.PushResultLabels() / paddle.PushResultLabels().
func classifyProviderError(prov billing.Provider, err error) string {
	if err == nil {
		return "ok"
	}
	if c, ok := prov.(billing.Classifier); ok {
		return c.ClassifyPushError(err)
	}
	return "other"
}
