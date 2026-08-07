// DNS handoff orchestrator (Tier A8 / ADR-083 / §14 M8 row
// "Gate-A runbook (2nd box active-passive)"). Implements the
// drain protocol:
//
//  1. StandbyState → draining (gauge flip is the public signal).
//  2. Wait for in-flight requests to drain, bounded by
//     api.HADNSRecordStaleSeconds (default 30 s).
//  3. Call dns.DeleteRecord(leader.name) — removes the public
//     DNS A record so new traffic stops arriving at the dying
//     leader.
//  4. Increment
//     activePassiveFailoversTotal{outcome="dns_flipped"} (or
//     "dns_stale" / "peer_unreachable" on the failure paths).
//  5. The new leader's election fires
//     dns.UpsertRecord(newLeader.name); existing standbys
//     continue warming.
//
// The 30 s budget is bounded so a stuck drain doesn't block
// the operator's `kubectl drain` analog. If the budget blows,
// the leader marks itself `peer_unreachable` and the operator
// falls through to the manual drain command in the runbook.
//
// # Architecture invariants
//
//   - schedd is the ONLY writer to `instances`. This orchestrator
//     does not touch the instances table; it only flips the
//     StandbyState gauge + mutates public DNS.
//   - vmmd is the only root component. This orchestrator is
//     userland (gatewayd-public is unprivileged).
//   - apid does NOT touch vmmd directly. The orchestrator
//     delegates DNS work to the DNSProvider interface — no new
//     direct calls that bypass an owner.
//
// # Failure modes (called out explicitly)
//
//   - InFlight never reaches zero within HADNSRecordStaleSeconds:
//     outcome="dns_stale" + manual drain command.
//   - dns.DeleteRecord returns err after 5 retries: outcome=
//     "dns_stale" + runbook's manual DNS flip command.
//   - leader.ElectLeader returns Leader{} (no active peer):
//     outcome="peer_unreachable" + the alert rule
//     `FaasNoActivePeer` fires.

package gateway

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/onebox-faas/faas/pkg/api"
	"github.com/onebox-faas/faas/pkg/gateway/leader"
	"github.com/onebox-faas/faas/pkg/wire"
)

// DNSHandoff orchestrates the active-passive flip on a
// leader. cmd/gatewayd-public constructs one per process;
// callers (the compute_node_changed consumer) invoke Run() on
// every drain event.
type DNSHandoff struct {
	// LeaderStore backs leader.ElectLeader. Today the
	// production surface is pkg/state.PGStore (which already
	// powers pkg/sched/placement.go); tests pass an in-memory
	// fake.
	LeaderStore leader.LeaderStore
	// DNSProvider is the provider the leader calls
	// DeleteRecord on. Today either HetznerRecordProvider or
	// ManualDNSProvider.
	DNSProvider DNSProvider
	// NodeName is THIS node's compute_nodes.name. The
	// orchestrator only acts when the elected leader is
	// self (we drain when WE are the dying leader).
	NodeName string
	// NodeIP is the leader's egress IP — passed to
	// DNSProvider.UpsertRecord when a NEW leader is elected
	// and we are the leader. The drain path doesn't need it.
	NodeIP string
	// Metrics is the shared OpsMetrics accessor. Required.
	Metrics *wire.OpsMetrics
	// InFlight is the in-flight request counter; the drain
	// waits for InFlight() to reach zero (bounded by
	// api.HADNSRecordStaleSeconds). nil → use a default no-op
	// counter (PR-A's tests don't wire one).
	InFlight InFlightCounter

	// Now is the clock injection point (tests override).
	// nil → time.Now.
	Now func() time.Time
	// Budget overrides the drain budget. nil → derive from
	// api.HADNSRecordStaleSeconds at Run() time. Tests pass a
	// short Budget (e.g. 100ms) so the dns_stale retry loop
	// doesn't take 31s of wall-clock.
	Budget *time.Duration

	mu sync.Mutex // serializes Run() invocations
}

// InFlightCounter is the surface the orchestrator needs to
// know how many requests are still in flight. The production
// surface is pkg/gateway/handler.go's LastSeenSink-style
// counter; tests pass a fake.
type InFlightCounter interface {
	Count() int
}

// noopInFlight is the default counter (returns 0).
type noopInFlight struct{}

func (noopInFlight) Count() int { return 0 }

// Outcome is the success / failure shape Run() returns. Maps
// 1:1 to the activePassiveFailoversTotal{outcome} label
// vocabulary so the metric and the return value agree.
type Outcome string

const (
	// OutcomeDNSFlipped — drain completed; DNS A record removed;
	// new leader's UpsertRecord fires within the next election.
	OutcomeDNSFlipped Outcome = "dns_flipped"
	// OutcomeDNSStale — drain completed but
	// dns.DeleteRecord returned an error after 5 retries
	// (Hetzner 5xx) OR the manual provider returned
	// errManualDNSRequiresOperator (operator has not yet
	// flipped DNS by hand). The operator falls through to
	// the manual DNS flip command in
	// docs/runbooks/active-passive-ha.md. The tripwire for
	// "DNS flipped" was NEVER bumped in the manual path
	// (review finding #14 — the manual provider now returns
	// a sentinel error instead of nil).
	OutcomeDNSStale Outcome = "dns_stale"
	// OutcomePeerUnreachable — leader election returned a
	// zero-value Leader (no active peer) OR
	// HADNSRecordStaleSeconds elapsed with InFlight > 0.
	// The alert rule `FaasNoActivePeer` fires.
	OutcomePeerUnreachable Outcome = "peer_unreachable"
	// OutcomeManualDrain — operator-initiated drain via the
	// runbook's manual command. Not currently emitted by
	// Run(); reserved for a future v1.1 that wires the
	// runbook escalation into the orchestrator.
	OutcomeManualDrain Outcome = "manual_drain"
)

// Run executes the drain protocol when called by the leader.
// Returns the Outcome that was bumped into the metric so the
// caller can log/audit it.
//
// Pre-condition: this node is the elected leader. The caller
// (cmd/gatewayd-public's compute_node_changed consumer) is
// responsible for that gate; Run() does NOT re-check.
//
// Side effects on success:
//
//   - StandbyState → wire.StandbyStateDraining (3) for at
//     most HADNSRecordStaleSeconds.
//   - dns.DeleteRecord(self.Name) called once.
//   - activePassiveFailoversTotal{outcome=OutcomeDNSFlipped}
//     incremented.
//
// Side effects on failure paths:
//
//   - dns_stale: dns.DeleteRecord failed after 5 retries OR
//     the manual provider returned
//     errManualDNSRequiresOperator (review finding #14).
//   - peer_unreachable: leader.ElectLeader returned Leader{}
//     (no active peer), or HADNSRecordStaleSeconds budget
//     elapsed with InFlight > 0.
func (d *DNSHandoff) Run(ctx context.Context) Outcome {
	if d == nil {
		return OutcomePeerUnreachable
	}
	// Serialize Run() invocations so a flood of
	// compute_node_changed events doesn't double-drain.
	d.mu.Lock()
	defer d.mu.Unlock()

	now := d.now()
	budget := d.budgetDuration()
	deadline := now.Add(budget)

	if d.Metrics == nil {
		// Test path — no metrics wired. Still drain
		// (orchestrator correctness is independent of the
		// counter).
		return d.drainNoMetrics(ctx, deadline)
	}

	// Step 1: StandbyState → draining (review finding #9:
	// typed constant, not the raw int 3).
	d.Metrics.SetStandbyState(wire.StandbyStateDraining)

	// Step 2: Wait for in-flight to drain, bounded.
	inFlight := d.inFlight()
	if err := d.waitInFlightZero(ctx, deadline, inFlight); err != nil {
		d.Metrics.SetStandbyState(wire.StandbyStateWarm) // back to warm — manual drain path
		d.Metrics.ActivePassiveFailovers(string(OutcomePeerUnreachable)).Inc()
		return OutcomePeerUnreachable
	}

	// Step 3: dns.DeleteRecord with retry. The retry loop
	// distinguishes transient errors (retry) from the
	// manual-provider sentinel (no retry — operator has to
	// act; review finding #14).
	outcome, err := d.deleteRecordWithRetry(ctx, deadline)
	if err != nil {
		d.Metrics.ActivePassiveFailovers(string(outcome)).Inc()
		return outcome
	}

	// Step 4: success — bump dns_flipped.
	d.Metrics.ActivePassiveFailovers(string(OutcomeDNSFlipped)).Inc()
	return OutcomeDNSFlipped
}

// drainNoMetrics is the test-only path that runs the drain
// without touching the shared metrics accessor.
func (d *DNSHandoff) drainNoMetrics(ctx context.Context, deadline time.Time) Outcome {
	inFlight := d.inFlight()
	if err := d.waitInFlightZero(ctx, deadline, inFlight); err != nil {
		return OutcomePeerUnreachable
	}
	outcome, err := d.deleteRecordWithRetry(ctx, deadline)
	if err != nil {
		return outcome
	}
	return OutcomeDNSFlipped
}

// waitInFlightZero blocks until InFlight().Count() reaches 0
// or the deadline elapses. Returns nil on drain complete, an
// error on timeout / ctx cancel.
func (d *DNSHandoff) waitInFlightZero(ctx context.Context, deadline time.Time, initial InFlightCounter) error {
	if initial.Count() == 0 {
		return nil
	}
	t := time.NewTicker(50 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			if d.now().After(deadline) {
				return fmt.Errorf("drain: HADNSRecordStaleSeconds (%d) elapsed with in-flight > 0",
					api.HADNSRecordStaleSeconds)
			}
			if d.inFlight().Count() == 0 {
				return nil
			}
		}
	}
}

// deleteRecordWithRetry calls DNSProvider.DeleteRecord with
// exponential backoff (1s → 2s → 4s → 8s → 16s, capped at
// deadline). 5 retries total.
//
// Returns:
//
//   - (OutcomeDNSFlipped, nil) on success.
//   - (OutcomeDNSStale, err) on retry exhaustion.
//   - (OutcomeDNSStale, errManualDNSRequiresOperator) on the
//     manual provider path (review finding #14 — the manual
//     provider never returns nil; retry is a no-op).
//
// The backoff sleep is bounded by the deadline: a single
// retry never sleeps past the run's deadline (review finding
// #2 — without the race, the worst-case 5 retries would
// sleep 1+2+4+8+16 = 31s on top of the 5 RPCs, blowing past
// the 30s HADNSRecordStaleSeconds budget by ~46s of wall-
// clock and blocking the operator's drain).
func (d *DNSHandoff) deleteRecordWithRetry(ctx context.Context, deadline time.Time) (Outcome, error) {
	if d.DNSProvider == nil {
		return OutcomeDNSStale, fmt.Errorf("drain: nil DNSProvider")
	}
	backoff := time.Second
	var lastErr error
	for attempt := 0; attempt < 5; attempt++ {
		// Always attempt at least once (attempt == 0), even
		// when the deadline has already passed — a partial
		// drain is better than no drain.
		if attempt > 0 && d.now().After(deadline) {
			return OutcomeDNSStale, fmt.Errorf("drain: deadline elapsed after %d attempts: %w", attempt, lastErr)
		}
		if err := d.DNSProvider.DeleteRecord(ctx, d.NodeName); err != nil {
			lastErr = err
			// Review finding #14: the manual provider's
			// sentinel error is non-retryable — the
			// operator has to act, no point in 5 more
			// curls. Surface dns_stale immediately.
			if errors.Is(err, errManualDNSRequiresOperator) {
				return OutcomeDNSStale, err
			}
			// Sleep bounded by the deadline: race the
			// backoff against the deadline so a slow
			// retry doesn't block the operator.
			remaining := time.Until(deadline)
			if remaining <= 0 {
				return OutcomeDNSStale, fmt.Errorf("drain: deadline elapsed after %d attempts: %w", attempt+1, lastErr)
			}
			sleep := backoff
			if sleep > remaining {
				sleep = remaining
			}
			select {
			case <-ctx.Done():
				return OutcomeDNSStale, ctx.Err()
			case <-time.After(sleep):
			}
			backoff *= 2
			if backoff > 30*time.Second {
				backoff = 30 * time.Second
			}
			continue
		}
		return OutcomeDNSFlipped, nil
	}
	return OutcomeDNSStale, fmt.Errorf("drain: DeleteRecord failed after 5 retries: %w", lastErr)
}

func (d *DNSHandoff) inFlight() InFlightCounter {
	if d.InFlight == nil {
		return noopInFlight{}
	}
	return d.InFlight
}

func (d *DNSHandoff) now() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now()
}

// budgetDuration resolves the drain budget. Tests override
// via the Budget field; production reads from
// api.HADNSRecordStaleSeconds.
func (d *DNSHandoff) budgetDuration() time.Duration {
	if d.Budget != nil {
		return *d.Budget
	}
	return time.Duration(api.HADNSRecordStaleSeconds) * time.Second
}
