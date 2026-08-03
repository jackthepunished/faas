// Package floor — proactive per-app min-instances floor reconciler (issue #557 / ADR-071).
//
// Background: pkg/state.App has carried an EffectiveMinInstances floor
// since pre-M8. The reaper honors it downward (don't park below the
// floor) and the meterd sampler bills it (ADR-060, "billed from t=0").
// What was missing was the upward direction — nothing ever woke
// instances up to the floor. The reactive triggers in
// pkg/sched/scaleup (RPS/CPU) and pkg/sched/targets (in-flight) only
// fire on observed signal; a customer who configured min_instances=1
// but never sent traffic saw 0 instances resident.
//
// This package closes that gap. Tick() walks every app the schedd
// owns and, for each app whose floor > running count, calls
// Engine.AdmitInstance to wake one. The trigger is idempotent:
// ledger.Concurrency counts only {WAKING, COLD_BOOTING, RUNNING}
// (pkg/state/machine.go CountsForConcurrency), so PARKED instances
// don't block the wake. The engine's admitGate short-circuits with
// wakeMinFloorAlready once concurrency >= floor, so over-admitting
// is impossible without a bypass flag.
//
// Three failure modes are bounded:
//
//   - The §6.2-2 RAM ceiling is consulted before each admit
//     (OutcomeRamCeiling); live wakes see no new throttling.
//   - Per-app exponential backoff (capped at
//     api.MaxFloorBackoffSeconds = 60s) bounds the FAILED-row hazard
//     if AdmitInstance keeps returning errors.
//   - AtCapacity is success (no FAILED row); only non-nil errors
//     trigger backoff + warn-log.
//
// Mirrors the pkg/sched/scaleup skeleton exactly (AppStore / Ledger /
// Engine interfaces, WithOwnerNodeID, Interval, Tick) to keep the
// schedd-side wiring symmetric.
package floor
