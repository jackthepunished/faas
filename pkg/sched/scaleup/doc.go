// Package scaleup is the per-app reactive scale-up trigger (issue #169 /
// #172). It runs as a schedd loop worker on a 1s tick and admits
// additional instances of an app when its measured load exceeds the
// configured per-instance target. The trigger is purely advisory — it
// never forces an admission beyond plan.MaxConcurrency and never
// holds a request. A request that arrives via the gateway still calls
// Backend.Admit and gets the at-capacity no-op on its own; the trigger
// just races ahead of request-driven wakes when the signal is hot.
//
// # Inputs
//
//   - appStore: read-only access to the apps table (autoscale_target_rps,
//     autoscale_target_cpu_pct, MaxConcurrency).
//   - instats: per-instance CPU% from pkg/sched/instancestats.Reader
//     (PR #205). Nil-safe; if instats is nil, the CPU signal is skipped
//     and the trigger runs in RPS-only mode.
//   - promScraper: scrapes gatewayd's /metrics for the per-app RPS
//     signal. Nil-safe; if nil, the trigger no-ops on the RPS path.
//   - ledger: pkg/sched.NodeLedger.Concurrency(appID) gives the
//     current per-app live instance count used as the divisor.
//   - engine: pkg/sched.Engine.AdmitInstance performs the actual
//     admission. The trigger never bypasses it.
//
// # Outputs
//
//   - schedd_scale_up_decisions_total{app,outcome} counter
//   - schedd_scale_up_admit_rps histogram (per-instance RPS at admit)
//
// The package is designed to be testable in isolation: the pure
// decide() function takes a snapshot of inputs and returns a Decision,
// so the trigger can be exercised without a live engine or store.
package scaleup
