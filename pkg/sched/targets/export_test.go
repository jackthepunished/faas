// export_test.go — see pkg/sched/floor/export_test.go for the rationale.
// Same pattern, different constant. sched.TriggerTargets must stay
// byte-identical to wakeBootTriggerTargets (see pkg/sched/targets/trigger.go:18).
package targets

const WakeBootTriggerTargets = wakeBootTriggerTargets
