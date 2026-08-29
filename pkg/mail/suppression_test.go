package mail

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/onebox-faas/faas/pkg/api"
)

// fakeSuppressionChecker is the test double for SuppressionChecker.
// It records calls so the table can assert the cache is hit when
// expected (no second IsMailSuppressed call within TTL) and
// bypassed when the TTL expires.
type fakeSuppressionChecker struct {
	calls atomic.Int64
	hits  map[string]bool
	errs  map[string]error
}

func newFakeSuppressionChecker() *fakeSuppressionChecker {
	return &fakeSuppressionChecker{hits: map[string]bool{}, errs: map[string]error{}}
}

func (f *fakeSuppressionChecker) IsMailSuppressed(_ context.Context, email string) (bool, error) {
	f.calls.Add(1)
	if err, ok := f.errs[email]; ok {
		return false, err
	}
	return f.hits[email], nil
}

// recordingSender captures every Message it receives and lets a
// test fail when a suppressed send reaches it.
type recordingSender struct {
	got    atomic.Int64
	lastTo []string
}

func (r *recordingSender) Send(_ context.Context, msg Message) error {
	r.got.Add(1)
	r.lastTo = msg.To
	return nil
}

// failingSender returns a configured error so the test can verify
// the decorator passes the inner error through when the recipient
// is NOT suppressed.
type failingSender struct{ err error }

func (f *failingSender) Send(_ context.Context, _ Message) error { return f.err }

// fixedClock returns the same time until advanced.
type fixedClock struct{ t atomic.Int64 }

func (c *fixedClock) Now() time.Time {
	return time.Unix(0, c.t.Load())
}

func (c *fixedClock) Advance(d time.Duration) { c.t.Add(int64(d)) }

func newFixedClock() *fixedClock { return &fixedClock{} }

// silentLogger keeps test output clean while still exercising the
// logger field on the decorator.
func silentLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// TestSuppressingSender_DropsSuppressedRecipient pins the headline
// contract: a recipient that the Store marks as suppressed produces
// nil from Send and never reaches the inner Sender.
func TestSuppressingSender_DropsSuppressedRecipient(t *testing.T) {
	store := newFakeSuppressionChecker()
	store.hits["bob@example.com"] = true
	inner := &recordingSender{}
	clock := newFixedClock()
	s := &SuppressingSender{
		Inner:    inner,
		Store:    store,
		Log:      silentLogger(),
		CacheTTL: time.Minute,
		Now:      clock.Now,
	}
	if err := s.Send(context.Background(), Message{To: []string{"bob@example.com"}}); err != nil {
		t.Fatalf("Send returned %v, want nil (suppression is a drop)", err)
	}
	if got := inner.got.Load(); got != 0 {
		t.Fatalf("inner Sender called %d times, want 0", got)
	}
	if got := store.calls.Load(); got != 1 {
		t.Fatalf("Store.IsMailSuppressed called %d times, want 1", got)
	}
}

// TestSuppressingSender_ForwardsUnsuppressedRecipient pins the
// happy path: a recipient NOT on the list reaches the inner Sender.
func TestSuppressingSender_ForwardsUnsuppressedRecipient(t *testing.T) {
	store := newFakeSuppressionChecker()
	store.hits["alice@example.com"] = false
	inner := &recordingSender{}
	clock := newFixedClock()
	s := &SuppressingSender{
		Inner:    inner,
		Store:    store,
		Log:      silentLogger(),
		CacheTTL: time.Minute,
		Now:      clock.Now,
	}
	if err := s.Send(context.Background(), Message{To: []string{"alice@example.com"}}); err != nil {
		t.Fatalf("Send returned %v, want nil", err)
	}
	if got := inner.got.Load(); got != 1 {
		t.Fatalf("inner Sender called %d times, want 1", got)
	}
	if got := inner.lastTo[0]; got != "alice@example.com" {
		t.Fatalf("inner Sender received %q, want alice@example.com", got)
	}
}

// TestSuppressingSender_PropagatesInnerError makes sure an unsuppressed
// recipient still surfaces an upstream failure — the decorator must
// not swallow real errors.
func TestSuppressingSender_PropagatesInnerError(t *testing.T) {
	store := newFakeSuppressionChecker()
	want := errors.New("upstream 500")
	inner := &failingSender{err: want}
	s := &SuppressingSender{
		Inner:    inner,
		Store:    store,
		Log:      silentLogger(),
		CacheTTL: time.Minute,
		Now:      newFixedClock().Now,
	}
	err := s.Send(context.Background(), Message{To: []string{"alice@example.com"}})
	if !errors.Is(err, want) {
		t.Fatalf("Send returned %v, want %v", err, want)
	}
}

// TestSuppressingSender_CachesDecision is the cache invariant: a
// second Send within the TTL must NOT consult the Store.
func TestSuppressingSender_CachesDecision(t *testing.T) {
	store := newFakeSuppressionChecker()
	store.hits["alice@example.com"] = false
	inner := &recordingSender{}
	clock := newFixedClock()
	s := &SuppressingSender{
		Inner:    inner,
		Store:    store,
		Log:      silentLogger(),
		CacheTTL: time.Minute,
		Now:      clock.Now,
	}
	for i := 0; i < 5; i++ {
		if err := s.Send(context.Background(), Message{To: []string{"alice@example.com"}}); err != nil {
			t.Fatalf("Send #%d returned %v", i, err)
		}
	}
	if got := inner.got.Load(); got != 5 {
		t.Fatalf("inner Sender called %d times, want 5", got)
	}
	if got := store.calls.Load(); got != 1 {
		t.Fatalf("Store.IsMailSuppressed called %d times, want 1 (cache hit)", got)
	}
}

// TestSuppressingSender_CacheExpiresAfterTTL pins the TTL eviction:
// after CacheTTL elapses the decorator consults the Store again.
func TestSuppressingSender_CacheExpiresAfterTTL(t *testing.T) {
	store := newFakeSuppressionChecker()
	store.hits["alice@example.com"] = false
	inner := &recordingSender{}
	clock := newFixedClock()
	s := &SuppressingSender{
		Inner:    inner,
		Store:    store,
		Log:      silentLogger(),
		CacheTTL: time.Minute,
		Now:      clock.Now,
	}
	// First call populates the cache.
	_ = s.Send(context.Background(), Message{To: []string{"alice@example.com"}})
	// Advance past TTL — the next Send must hit the Store.
	clock.Advance(2 * time.Minute)
	_ = s.Send(context.Background(), Message{To: []string{"alice@example.com"}})
	if got := store.calls.Load(); got != 2 {
		t.Fatalf("Store.IsMailSuppressed called %d times, want 2 (cache expired)", got)
	}
}

// TestSuppressingSender_CacheKeyLowercased pins the case-insensitivity
// contract: "Alice@Example.COM" and "alice@example.com" share one
// cache entry.
func TestSuppressingSender_CacheKeyLowercased(t *testing.T) {
	store := newFakeSuppressionChecker()
	store.hits["alice@example.com"] = false
	inner := &recordingSender{}
	clock := newFixedClock()
	s := &SuppressingSender{
		Inner:    inner,
		Store:    store,
		Log:      silentLogger(),
		CacheTTL: time.Minute,
		Now:      clock.Now,
	}
	_ = s.Send(context.Background(), Message{To: []string{"Alice@Example.com"}})
	_ = s.Send(context.Background(), Message{To: []string{"ALICE@EXAMPLE.COM"}})
	_ = s.Send(context.Background(), Message{To: []string{"alice@example.com"}})
	if got := store.calls.Load(); got != 1 {
		t.Fatalf("Store.IsMailSuppressed called %d times, want 1 (case-insensitive cache key)", got)
	}
}

// TestSuppressingSender_StoreErrorTreatedAsSuppressed pins the
// fail-closed contract: a Store error must drop the send rather
// than fall through to the inner Sender.
func TestSuppressingSender_StoreErrorTreatedAsSuppressed(t *testing.T) {
	store := newFakeSuppressionChecker()
	store.errs["alice@example.com"] = errors.New("postgres down")
	inner := &recordingSender{}
	clock := newFixedClock()
	s := &SuppressingSender{
		Inner:    inner,
		Store:    store,
		Log:      silentLogger(),
		CacheTTL: time.Minute,
		Now:      clock.Now,
	}
	if err := s.Send(context.Background(), Message{To: []string{"alice@example.com"}}); err != nil {
		t.Fatalf("Send returned %v, want nil (Store error must be a drop, not an error)", err)
	}
	if got := inner.got.Load(); got != 0 {
		t.Fatalf("inner Sender called %d times, want 0 (fail-closed)", got)
	}
}

// TestSuppressingSender_StoreErrorNotCached pins the second half of
// the fail-closed contract: a Store error must NOT poison the cache,
// so a subsequent recovery is observable on the next Send.
func TestSuppressingSender_StoreErrorNotCached(t *testing.T) {
	store := newFakeSuppressionChecker()
	store.errs["alice@example.com"] = errors.New("transient")
	inner := &recordingSender{}
	clock := newFixedClock()
	s := &SuppressingSender{
		Inner:    inner,
		Store:    store,
		Log:      silentLogger(),
		CacheTTL: time.Minute,
		Now:      clock.Now,
	}
	// First call: Store errors, message dropped, cache not poisoned.
	_ = s.Send(context.Background(), Message{To: []string{"alice@example.com"}})
	// Recover: remove the error.
	delete(store.errs, "alice@example.com")
	store.hits["alice@example.com"] = false
	_ = s.Send(context.Background(), Message{To: []string{"alice@example.com"}})
	if got := inner.got.Load(); got != 1 {
		t.Fatalf("inner Sender called %d times, want 1 (recovery must not be cached)", got)
	}
}

// TestSuppressingSender_AnySuppressedRecipientsDropsMessage pins the
// "any-recipient-suppressed → drop entire message" policy. Partial
// delivery risks the remaining recipient seeing a confusing
// "to a colleague who never got it" context.
func TestSuppressingSender_AnySuppressedRecipientsDropsMessage(t *testing.T) {
	store := newFakeSuppressionChecker()
	store.hits["a@example.com"] = false
	store.hits["b@example.com"] = true
	inner := &recordingSender{}
	clock := newFixedClock()
	s := &SuppressingSender{
		Inner:    inner,
		Store:    store,
		Log:      silentLogger(),
		CacheTTL: time.Minute,
		Now:      clock.Now,
	}
	if err := s.Send(context.Background(), Message{To: []string{"a@example.com", "b@example.com"}}); err != nil {
		t.Fatalf("Send returned %v, want nil", err)
	}
	if got := inner.got.Load(); got != 0 {
		t.Fatalf("inner Sender called %d times, want 0 (any-suppressed drops all)", got)
	}
}

// TestSuppressingSender_DefaultsToAPILimitTTL pins that a zero
// CacheTTL falls back to api.MailSuppressionCacheTTLSeconds.
func TestSuppressingSender_DefaultsToAPILimitTTL(t *testing.T) {
	s := &SuppressingSender{CacheTTL: 0}
	want := time.Duration(api.MailSuppressionCacheTTLSeconds) * time.Second
	if got := s.ttl(); got != want {
		t.Fatalf("ttl() = %v, want %v (api.MailSuppressionCacheTTLSeconds)", got, want)
	}
}

// TestSuppressingSender_NilInnerRejected pins the constructor-side
// guard: a missing Inner produces a clear error rather than a panic.
func TestSuppressingSender_NilInnerRejected(t *testing.T) {
	store := newFakeSuppressionChecker()
	s := &SuppressingSender{
		Inner:    nil,
		Store:    store,
		Log:      silentLogger(),
		CacheTTL: time.Minute,
		Now:      newFixedClock().Now,
	}
	if err := s.Send(context.Background(), Message{To: []string{"alice@example.com"}}); err == nil {
		t.Fatal("Send with nil Inner should fail")
	}
}

// TestSuppressingSender_NilStoreRejected pins the fail-closed
// constructor-side guard: a missing Store means we cannot verify
// the recipient, so we must refuse to send rather than bypass the
// check.
func TestSuppressingSender_NilStoreRejected(t *testing.T) {
	inner := &recordingSender{}
	s := &SuppressingSender{
		Inner:    inner,
		Store:    nil,
		Log:      silentLogger(),
		CacheTTL: time.Minute,
		Now:      newFixedClock().Now,
	}
	err := s.Send(context.Background(), Message{To: []string{"alice@example.com"}})
	if err == nil {
		t.Fatal("Send with nil Store should fail (fail-closed)")
	}
	if got := inner.got.Load(); got != 0 {
		t.Fatalf("inner Sender called %d times, want 0 (fail-closed)", got)
	}
}

// TestSuppressingSender_RecordsSuppressedMetric pins that a drop
// increments the suppressed counter (via RecordFailure + the
// ReasonSuppressed label).
func TestSuppressingSender_RecordsSuppressedMetric(t *testing.T) {
	store := newFakeSuppressionChecker()
	store.hits["bob@example.com"] = true
	inner := &recordingSender{}
	clock := newFixedClock()
	metrics := &fakeMetrics{}
	s := &SuppressingSender{
		Inner:    inner,
		Store:    store,
		Log:      silentLogger(),
		Metrics:  metrics,
		CacheTTL: time.Minute,
		Now:      clock.Now,
	}
	_ = s.Send(context.Background(), Message{To: []string{"bob@example.com"}})
	if got := metrics.failures[ReasonSuppressed]; got != 1 {
		t.Fatalf("ReasonSuppressed recorded %d times, want 1", got)
	}
}

// fakeMetrics is a tiny test double for Metrics; records each
// (reason, transport) pair it sees.
type fakeMetrics struct {
	failures map[string]int
	retries  map[string]int
}

func (m *fakeMetrics) RecordFailure(reason string) {
	if m.failures == nil {
		m.failures = map[string]int{}
	}
	m.failures[reason]++
}

func (m *fakeMetrics) RecordRetry(transport string) {
	if m.retries == nil {
		m.retries = map[string]int{}
	}
	m.retries[transport]++
}
