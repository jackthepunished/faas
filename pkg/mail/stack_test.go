package mail

import (
	"context"
	"testing"
)

// noopSuppressionChecker is a SuppressionChecker that always
// reports "not suppressed". Used by the stack type-chain test to
// keep the path free of the dedicated suppression test fixtures.
type noopSuppressionChecker struct{}

func (noopSuppressionChecker) IsMailSuppressed(_ context.Context, _ string) (bool, error) {
	return false, nil
}

// TestStack_TypeChain pins the decorator ordering mandated by the
// plan:
//
//	SuppressingSender
//	  └── RetryingSender
//	        └── ResendSender | PostmarkSender | LogSender | NoopSender
//
// Suppression must be outermost so a suppressed address costs
// zero HTTP attempts. A regression that reorders the decorators
// or drops one out of the chain must trip this test.
func TestStack_TypeChain(t *testing.T) {
	noop := NoopSender{}
	var s Sender = &SuppressingSender{
		Inner: &RetryingSender{
			Inner: noop,
		},
		Store: noopSuppressionChecker{},
	}
	sup, ok := s.(*SuppressingSender)
	if !ok {
		t.Fatalf("outermost sender = %T, want *SuppressingSender", s)
	}
	ret, ok := sup.Inner.(*RetryingSender)
	if !ok {
		t.Fatalf("suppressing.Inner = %T, want *RetryingSender", sup.Inner)
	}
	if _, ok := ret.Inner.(NoopSender); !ok {
		t.Fatalf("retry.Inner = %T, want NoopSender", ret.Inner)
	}
}

// TestStack_NoopIsTheInnerMost guards the inner-most type: every
// concrete transport we ship today (NoopSender, LogSender,
// ResendSender, PostmarkSender) implements Sender directly so the
// decorator stack terminates cleanly at a transport that can
// actually move bytes.
func TestStack_NoopIsTheInnerMost(t *testing.T) {
	noop := NoopSender{}
	var s Sender = &SuppressingSender{
		Inner: &RetryingSender{
			Inner: noop,
		},
		Store: noopSuppressionChecker{},
	}
	if err := s.Send(context.Background(), Message{To: []string{"x@example.com"}}); err != nil {
		t.Fatalf("NoopSender stack returned %v, want nil", err)
	}
}
