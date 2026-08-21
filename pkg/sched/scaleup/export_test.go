// export_test.go — see pkg/sched/floor/export_test.go for the rationale.
// Same pattern, different constant. sched.TriggerScaleup must stay
// byte-identical to wakeBootTriggerScaleup (see pkg/sched/scaleup/trigger.go:20).
package scaleup

const WakeBootTriggerScaleup = wakeBootTriggerScaleup
