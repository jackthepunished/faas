// Unit tests for the pkg/audit.Auditor seam (issue #278 surface,
// lifted from cmd/apid/audit.go for cross-daemon reuse). The
// end-to-end counterpart that pins the action-not-rolled-back invariant
// under ADR-035 lives at the call site (cmd/apid/handlers_audit_test.go,
// pkg/sched/cron_test.go for cron.fired).
package audit_test

import (
	"context"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/onebox-faas/faas/pkg/audit"
	"github.com/onebox-faas/faas/pkg/state"
)

// uuidStringOf normalises either a canonical UUID (with hyphens) or a
// raw 32-char hex string into the canonical UUID form. MemStore's
// newID returns hex; parseSubjectID converts the hex back to canonical
// UUID bytes when storing the Subject, so ListEvents(subject=<hex>)
// returns rows whose Subject.String() always reports the canonical
// form regardless of which store produced it. (Same helper as
// pkg/sched/events_test.go and cmd/apid/handlers_audit_test.go.)
func uuidStringOf(s string) string {
	if strings.Contains(s, "-") {
		return s
	}
	if len(s) != 32 {
		return s
	}
	b, err := hex.DecodeString(s)
	if err != nil {
		return s
	}
	return uuid.UUID(b).String()
}

// stubAuditOps is an in-memory Ops for unit tests. The full
// wire.OpsMetrics has a Prometheus counter+histogram; this stub
// exposes the same shape so the Auditor's interface contract is
// pinned without dragging the registry into the audit unit tests.
type stubAuditOps struct {
	mu        sync.Mutex
	registry  *prometheus.Registry
	counters  map[string]prometheus.Counter
	durations []stubDuration // ordered observations
}

type stubDuration struct {
	result string
	secs   float64
}

func newStubAuditOps() *stubAuditOps {
	return &stubAuditOps{
		registry: prometheus.NewRegistry(),
		counters: make(map[string]prometheus.Counter),
	}
}

func (s *stubAuditOps) AuditWriteFailures(accountID string) prometheus.Counter {
	s.mu.Lock()
	defer s.mu.Unlock()
	if c, ok := s.counters[accountID]; ok {
		return c
	}
	c := prometheus.NewCounter(prometheus.CounterOpts{
		Name: "stub_audit_write_failures",
		Help: "test stub for the audit-write-failure counter",
	})
	s.registry.MustRegister(c)
	s.counters[accountID] = c
	return c
}

func (s *stubAuditOps) AuditWriteFailureDuration(result string) prometheus.Observer {
	return stubObserver{s: s, result: result}
}

func (s *stubAuditOps) failureCount(t *testing.T, accountID string) float64 {
	t.Helper()
	s.mu.Lock()
	c, ok := s.counters[accountID]
	s.mu.Unlock()
	if !ok {
		return 0
	}
	return testutil.ToFloat64(c)
}

func (s *stubAuditOps) durationCount(t *testing.T, result string) int {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, d := range s.durations {
		if d.result == result {
			n++
		}
	}
	return n
}

type stubObserver struct {
	s      *stubAuditOps
	result string
}

func (o stubObserver) Observe(secs float64) {
	o.s.mu.Lock()
	defer o.s.mu.Unlock()
	o.s.durations = append(o.s.durations, stubDuration{result: o.result, secs: secs})
}

// failingStore wraps a state.Store and returns boomErr from
// AppendEvent only. Mirrors cmd/apid/audit_test.go::failingStore.
type failingStore struct {
	state.Store
}

var boomErr = errors.New("simulated AppendEvent failure")

func (failingStore) AppendEvent(_ context.Context, _, _ string, _ *string, _ []byte) error {
	return boomErr
}

// silentLog discards slog output so test runs stay clean.
func silentLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestAuditor_Emit_WritesRowWithActor(t *testing.T) {
	store := state.NewMemStore()
	// MemStore.ListEvents parses the subject via parseSubjectID
	// (memstore.go:2834) which accepts canonical UUIDs (with hyphens)
	// or 32-char hex. CreateAccount returns a real UUID, so the
	// ListEvents filter below will match.
	acctRec, err := store.CreateAccount(context.Background(), "schedd-audit@example.com", "schedd")
	if err != nil {
		t.Fatalf("CreateAccount: %v", err)
	}
	ops := newStubAuditOps()
	a := audit.New(store, silentLog(), ops, "schedd")

	a.Emit(context.Background(), "cron.fired", &acctRec.ID, map[string]any{
		"cron_id": "c-1",
		"app_id":  "a-1",
	})

	rows, err := store.ListEvents(context.Background(), acctRec.ID, 0)
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	if rows[0].Actor != "schedd" {
		t.Errorf("Actor = %q, want schedd", rows[0].Actor)
	}
	if rows[0].Kind != "cron.fired" {
		t.Errorf("Kind = %q, want cron.fired", rows[0].Kind)
	}
	if rows[0].Subject == nil || rows[0].Subject.String() != uuidStringOf(acctRec.ID) {
		t.Errorf("Subject = %v, want %s", rows[0].Subject, uuidStringOf(acctRec.ID))
	}
	if got := ops.durationCount(t, "ok"); got != 1 {
		t.Errorf("ok observations = %d, want 1", got)
	}
	if got := ops.failureCount(t, acctRec.ID); got != 0 {
		t.Errorf("failure counter for %s = %v, want 0", acctRec.ID, got)
	}
}

func TestAuditor_Emit_NilAccountIDAllowed(t *testing.T) {
	store := state.NewMemStore()
	a := audit.New(store, silentLog(), newStubAuditOps(), "schedd")

	// nil subject is allowed for system-level events (e.g. cron.fired
	// when the account resolution failed earlier in the path). The
	// row still lands, just with an empty subject.
	a.Emit(context.Background(), "system.boot", nil, map[string]any{"k": "v"})

	rows, err := store.ListEvents(context.Background(), "", 0)
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	if rows[0].Subject != nil {
		t.Errorf("Subject = %v, want nil", rows[0].Subject)
	}
}

func TestAuditor_Emit_NilDataMarshalsAsEmptyObject(t *testing.T) {
	store := state.NewMemStore()
	a := audit.New(store, silentLog(), newStubAuditOps(), "apid")
	acctRec, err := store.CreateAccount(context.Background(), "apid-audit@example.com", "apid")
	if err != nil {
		t.Fatalf("CreateAccount: %v", err)
	}

	a.Emit(context.Background(), "auth.logout", &acctRec.ID, nil)

	rows, _ := store.ListEvents(context.Background(), acctRec.ID, 0)
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	if string(rows[0].Data) != "{}" {
		t.Errorf("Data = %q, want \"{}\"", rows[0].Data)
	}
}

func TestAuditor_Emit_AppendEventFailureDoesNotPanic(t *testing.T) {
	base := state.NewMemStore()
	acctRec, err := base.CreateAccount(context.Background(), "schedd-fail@example.com", "schedd")
	if err != nil {
		t.Fatalf("CreateAccount: %v", err)
	}
	store := failingStore{base}
	ops := newStubAuditOps()
	a := audit.New(store, silentLog(), ops, "schedd")

	// Must NOT panic; failure semantics (ADR-035) require this to be
	// observable only — the action has already returned 200 by the
	// time Emit fires, so the audit row is observation, not source of
	// truth. The Warn log + counter increment + "failed" duration
	// observation are the only effects.
	a.Emit(context.Background(), "cron.fired", &acctRec.ID, map[string]any{"cron_id": "c-1"})

	if got := ops.durationCount(t, "failed"); got != 1 {
		t.Errorf("failed observations = %d, want 1", got)
	}
	if got := ops.failureCount(t, acctRec.ID); got != 1 {
		t.Errorf("failure counter for %s = %v, want 1", acctRec.ID, got)
	}
}

func TestAuditor_Emit_NilOpsAllowed(t *testing.T) {
	// Unit tests without an OpsMetrics (and the cmd/apid test
	// harness when ops wiring is skipped) must still work. The
	// counter increment + duration observation are guarded.
	store := state.NewMemStore()
	acctRec, err := store.CreateAccount(context.Background(), "apid-nilops@example.com", "apid")
	if err != nil {
		t.Fatalf("CreateAccount: %v", err)
	}
	a := audit.New(store, silentLog(), nil, "apid")

	a.Emit(context.Background(), "key.created", &acctRec.ID, map[string]any{"key_id": "k-1"})

	rows, _ := store.ListEvents(context.Background(), acctRec.ID, 0)
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
}

func TestAuditor_Emit_AppendEventFailureWithNilOpsAlsoDoesNotPanic(t *testing.T) {
	// Double-failure: AppendEvent errors AND ops is nil. The
	// counter+observation branches must be no-ops, not panics.
	store := failingStore{state.NewMemStore()}
	a := audit.New(store, silentLog(), nil, "schedd")
	acct := "acct-1"

	a.Emit(context.Background(), "cron.fired", &acct, map[string]any{"cron_id": "c-1"})
}