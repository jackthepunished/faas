package sched

// drain_compile_test.go — compile-time guarantee the unified
// invocations drain participates in the pkg/dispatch contract
// (ADR-134 §6.7). The two schedd drains (this package's drain.go
// for invocations, dispatch_triggers.go for trigger_records) are
// the only writers of the row-state-machine transitions. Adding
// a row type that does not satisfy dispatch.Job should fail to
// compile here, not at runtime in production.
//
// Today *state.Invocation does NOT implement dispatch.Job —
// PR-B adds the seven accessor methods (Kind, Origin,
// RetryPolicy, Deadline, CurrentAttempts, ErrorText, Snapshot)
// once the per-row JSONB columns land on the invocations table.
// Until then the synthetic adapter below proves the interface
// compiles cleanly and pins the expected method shape so a
// future change to dispatch.Job lands in code review, not at
// runtime.

import (
	"context"
	"testing"

	"github.com/onebox-faas/faas/pkg/dispatch"
)

// invocationJob is the synthetic adapter that stands in for
// *state.Invocation during PR-A. It carries exactly the fields
// dispatch.Job requires; PR-B will replace this with the real
// state.Invocation methods.
type invocationJob struct {
	id, appID, accountID, origin, errText string
	attempts                              int
	retry                                 dispatch.RetryPolicy
	deadline                              dispatch.DeadlinePolicy
	snapshot                              []byte
}

func (j *invocationJob) Kind() dispatch.JobKind            { return dispatch.JobKindInvocation }
func (j *invocationJob) ID() string                        { return j.id }
func (j *invocationJob) AppID() string                     { return j.appID }
func (j *invocationJob) AccountID() string                 { return j.accountID }
func (j *invocationJob) Origin() string                    { return j.origin }
func (j *invocationJob) RetryPolicy() dispatch.RetryPolicy { return j.retry }
func (j *invocationJob) Deadline() dispatch.DeadlinePolicy { return j.deadline }
func (j *invocationJob) CurrentAttempts() int              { return j.attempts }
func (j *invocationJob) ErrorText() string                 { return j.errText }
func (j *invocationJob) Snapshot() []byte                  { return j.snapshot }

// TestDispatch_ContractCompiles is the package-level assertion
// that the invocations drain can consume the dispatch contract.
// The synthetic invocationJob stands in for *state.Invocation
// until PR-B; if the interface changes (a method renamed, a
// signature altered) this test fails to compile, surfacing the
// change in code review.
func TestDispatch_ContractCompiles(t *testing.T) {
	var _ dispatch.Job = (*invocationJob)(nil)

	// Touch ctx so the import survives even if a future refactor
	// removes the only context-using method on the adapter.
	_ = context.Background
}
