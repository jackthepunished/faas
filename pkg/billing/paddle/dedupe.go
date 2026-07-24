package paddle

import (
	"context"
	"time"
)

// PaddleOverageDedupe is the cross-process dedupe gate the meterd
// overage pusher reads before issuing a Paddle CreateTransaction POST.
// The (account_id, month) row is written AFTER a successful POST and
// read BEFORE the POST as a no-op gate; a meterd that crashes between
// POST and stamp cannot cause a second POST for the same month from
// any subsequent process boot. Both state.MemStore and state.PgStore
// satisfy this through the same state.Store interface methods, so the
// meterd loader can pass `store` directly. Mirrors the PushDedupe
// shape in pkg/billing/stripe/client.go so the two providers share
// the same pattern even though their grain differs (hour vs. month).
type PaddleOverageDedupe interface {
	HasPaddleOverageMonth(ctx context.Context, accountID string, month time.Time) (bool, error)
	RecordPaddleOverageMonth(ctx context.Context, accountID string, month time.Time) error
}
