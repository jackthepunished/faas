// export_test.go exposes package-private symbols to cross-package tests
// (specifically pkg/sched/trigger_lockstep_test.go) without growing the
// production API surface. The file is compiled only under `go test`, never
// shipped, so the unexported mirrors wakeBootTriggerFloor / FloorDep stay
// package-local in production while the lockstep test can read them from
// outside. Pattern: https://pkg.go.dev/testing#hdr-Testing_Package_Links —
// canonical for unexported constants that must be visible to external
// tests across package boundaries.
//
// PR-A follow-on (ADR-123): the values here MUST stay byte-identical to
// sched.TriggerFloor / sched.TriggerFloorDep (see pkg/sched/triggers.go).
// The central lockstep test pins the contract; any drift fires a
// t.Errorf at `go test` time, before merge.
package floor

const (
	WakeBootTriggerFloor    = wakeBootTriggerFloor
	WakeBootTriggerFloorDep = wakeBootTriggerFloorDep
)
