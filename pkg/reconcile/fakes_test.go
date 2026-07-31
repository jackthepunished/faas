// Test fakes for pkg/reconcile. The fakeStore embeds *state.MemStore
// (which already implements the full state.Store interface) and
// overrides only the methods the reconcile package calls. This
// keeps the fake authoritative on the 200+ method interface while
// letting tests assert on the few that matter.
//
// The fakeAuditor is intentionally a thin wrapper around the real
// pkg/audit.Auditor. audit.New requires a Store (for AppendEvent),
// a Logger, and an Ops (counters). For tests we need:
//
//   - a Store that records AppendEvent calls so assertions can
//     check the audit-row payload structure
//   - a Logger (slog.Default is fine)
//   - an Ops (a no-op stub that satisfies the 2-method interface)
//
// The Auditor.Emit signature is void (no error), so the fakes
// don't need to model error injection — the auditor's contract
// is best-effort.

package reconcile

import (
	"context"
	"io"
	"log/slog"
	"sync"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/onebox-faas/faas/pkg/api"
	"github.com/onebox-faas/faas/pkg/audit"
	"github.com/onebox-faas/faas/pkg/state"
)

// fakeStore wraps *state.MemStore and adds three hooks the
// reconcile tests inspect:
//
//   - appsForProjectHook (lets a test inject a custom result for
//     one call without mutating the underlying MemStore)
//   - appendEventHook (records every AppendEvent call so the
//     audit-ordering assertion can pin the chronology)
//   - updateAppHook (lets a test override the UpdateApp payload
//     for the audit-ordering test)
//   - applyProjectPlanHook (lets a test inject the post-apply App
//     set for the create set)
//   - softDeleteAppCascadeHook (lets a test inject the post-delete
//     App for the removes)
//
// All hooks are optional; when nil, the embeds call through to
// MemStore.
type fakeStore struct {
	*state.MemStore

	mu sync.Mutex
	// appendEvents records every call to AppendEvent in order.
	// Pinned by the audit-ordering test.
	appendEvents []fakeEvent
	// accountPlan is the plan the fakeStore's AccountByID returns.
	// Defaults to api.PlanHobby.
	accountPlan api.Plan
}

type fakeEvent struct {
	Actor   string
	Kind    string
	Subject *string
	Data    []byte
}

// newFakeStore returns a fakeStore fed from a freshly-constructed
// MemStore. The MemStore is also returned so the test can pre-seed
// accounts, projects, and apps without going through the full
// CRUD surface.
func newFakeStore() *fakeStore {
	return &fakeStore{
		MemStore:    state.NewMemStore(),
		accountPlan: api.PlanHobby,
	}
}

// AppendEvent records the call then delegates to the underlying
// MemStore. The MemStore is fine for storing audit rows because
// the fakeAuditor delegates to it via real pkg/audit.
func (s *fakeStore) AppendEvent(ctx context.Context, actor, kind string, subject *string, data []byte) error {
	s.mu.Lock()
	s.appendEvents = append(s.appendEvents, fakeEvent{
		Actor:   actor,
		Kind:    kind,
		Subject: subject,
		Data:    data,
	})
	s.mu.Unlock()
	return s.MemStore.AppendEvent(ctx, actor, kind, subject, data)
}

// CreateAccount promotes the embedded *state.MemStore.CreateAccount
// up so the linter can see it. The staticcheck QF1008 hit on
// "s.MemStore.CreateAccount" is the kind of noise a fake
// shouldn't have to lint around.
func (s *fakeStore) CreateAccount(ctx context.Context, email string, plan api.Plan) (state.Account, error) {
	return s.MemStore.CreateAccount(ctx, email, plan)
}


// snapshotEvents returns a copy of the recorded AppendEvent calls
// since the last reset. Safe under concurrent mutation because
// the underlying slice is rebuilt on every call.
func (s *fakeStore) snapshotEvents() []fakeEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]fakeEvent, len(s.appendEvents))
	copy(out, s.appendEvents)
	return out
}

// putAccount seeds an Account with the fakeStore's plan. Used by
// the tests as the prereq for Project + Apps by AccountID.
func (s *fakeStore) putAccount(id string) state.Account {
	acct, err := s.CreateAccount(context.Background(), id+"@example.com", s.accountPlan)
	if err != nil {
		panic(err)
	}
	return acct
}

// noopOps is the audit.Ops stub. pkg/audit requires the interface
// but the tests don't assert on counters; the histogram values
// are observed but discarded via a no-op Counter / Observer.
type noopOps struct{}

func (noopOps) AuditWriteFailures(string) prometheus.Counter {
	// Discard is a real Counter that accepts .Inc() without
	// panicking or registering a metric. Returning nil would
	// crash audit.go:117 which calls .Observe() on the
	// returned Observer.
	return prometheus.NewCounter(prometheus.CounterOpts{})
}
func (noopOps) AuditWriteFailureDuration(string) prometheus.Observer {
	return prometheus.NewHistogram(prometheus.HistogramOpts{})
}

// newFakeAuditor builds a real pkg/audit.Auditor backed by the
// fakeStore. The "actor" is "reconcile" so the audit-row actor
// column matches the convention. The returned handle also
// exposes the audit hook so tests can introspect the calls.
func newFakeAuditor(store *fakeStore) *audit.Auditor {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	return audit.New(store, log, noopOps{}, "reconcile")
}

// suppress unused-import warnings if these helpers go untouched
// in a future refactor.
var _ = api.PlanHobby
