// Package targets is the per-app concurrent_requests scale-up
// trigger (PR-C, issue #462). It runs as a schedd loop worker on
// a 1s tick and admits an additional instance of an app when its
// measured per-instance inflight count exceeds the customer's
// target.concurrent_requests value AND the per-app
// scale_out_cooldown has elapsed. The trigger is purely advisory
// — it never forces an admission beyond plan.MaxConcurrency and
// never holds a request. A request that arrives via the gateway
// still calls Engine.AdmitInstance and gets the at-capacity no-op
// on its own; the trigger just races ahead of request-driven wakes
// when the signal is hot.
//
// Mirrors pkg/sched/scaleup in shape (Tick + Outcome + decide +
// closed-set metric pre-instantiation) but reads from a different
// signal source: the *instancestats.Reader.MaxInflightForApp
// accessor (PR-B wires the wire path; PR-C wires the consumer).
// The scaleup package's RPS / CPU axis is unchanged — this package
// is a sibling trigger, not a replacement.
//
// # Inputs
//
//   - appStore: read-only access to the apps table
//     (ScalingPolicy.Target, ScalingPolicy.MaxInstances, plus
//     LastScaleOutAt for the cooldown consult).
//   - instats: per-instance InflightRequests from
//     pkg/sched/instancestats.Reader.MaxInflightForApp (PR-B +
//     PR-C). Nil-safe; if instats is nil, every app falls into
//     OutcomeNoSignal and the trigger no-ops.
//   - ledger: pkg/sched.NodeLedger.Concurrency(appID) gives the
//     current per-app live instance count used as the divisor
//     and the cooldown discriminator.
//   - engine: pkg/sched.Engine.AdmitInstance performs the actual
//     admission. The trigger never bypasses it.
//
// # Outputs
//
//   - schedd_scale_up_decisions_total{app,outcome} counter (shared
//     with pkg/sched/scaleup; the closed outcome label set is
//     {admit, reject_at_cap, no_signal, cooldown_held} — adding a
//     new outcome requires extending the pre-instantiation loop in
//     pkg/wire.NewOpsMetrics).
//
// The package is designed to be testable in isolation: the pure
// decide() function takes a snapshot of inputs and returns a Decision,
// so the trigger can be exercised without a live engine or store.
package targets
