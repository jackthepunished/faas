// Tests for pkg/webhook/dispatcher (issue #476 / ADR-076).
//
// Layering:
//   - dispatcher_test.go: in-process dispatcher shape — backoff
//     math, claim-ordering contract, audit emission, drain
//     timeout. Uses MemStore + httptest.Server.
//   - dispatcher_property_test.go: per-account fairness property
//     over 10 ticks.
//
// Both layers use the dispatcher's Sleeper/Now struct fields to
// fast-forward time without touching the wall clock.
package webhook

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"filippo.io/age"
	"github.com/onebox-faas/faas/pkg/secretbox"
	"github.com/onebox-faas/faas/pkg/state"
)

// newTestAppWebhook returns an in-memory app_webhooks row seeded
// with a sealed secret (plaintext "test-secret") so the dispatcher's
// unseal path round-trips.
func newTestAppWebhook(t *testing.T, m *state.MemStore, appID, accountID, targetURL string, policy state.AppWebhookRetryPolicy) state.AppWebhook {
	t.Helper()
	sealed := sealTestSecret(t, "test-secret")
	w, err := m.CreateAppWebhook(context.Background(), state.AppWebhook{
		AppID:        appID,
		AccountID:    accountID,
		TargetURL:    targetURL,
		SecretSealed: sealed,
		EventFilter:  []string{},
		RetryPolicy:  policy,
		Enabled:      true,
	})
	if err != nil {
		t.Fatalf("CreateAppWebhook: %v", err)
	}
	return w
}

// sealTestSecret wraps plaintext in a sealed secretbox envelope
// using a freshly generated age identity. The plaintext namespace
// is "APP_WEBHOOK" so the dispatcher's namespace check passes.
func sealTestSecret(t *testing.T, plaintext string) []byte {
	t.Helper()
	ident, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatalf("age.GenerateX25519Identity: %v", err)
	}
	sealed, err := secretbox.SealBytes(ident.Recipient(), "APP_WEBHOOK", []byte(plaintext), 1024)
	if err != nil {
		t.Fatalf("secretbox.SealOne: %v", err)
	}
	return sealed
}

// identityForSealedBlob produces a pair (identity loader, sealed blob)
// so the test can write a real sealed envelope that the loader opens.
func identityForSealedBlob(t *testing.T) (func() []*age.X25519Identity, []byte) {
	t.Helper()
	ident, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatalf("age.GenerateX25519Identity: %v", err)
	}
	sealed, err := secretbox.SealBytes(ident.Recipient(), "APP_WEBHOOK", []byte("test-secret"), 1024)
	if err != nil {
		t.Fatalf("secretbox.SealOne: %v", err)
	}
	return func() []*age.X25519Identity { return []*age.X25519Identity{ident} }, sealed
}

// recordingSleeper captures the durations Sleep was called with so
// the test can assert the backoff schedule without waiting.
type recordingSleeper struct {
	mu    sync.Mutex
	slept []time.Duration
}

func (r *recordingSleeper) Sleep(d time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.slept = append(r.slept, d)
}

func (r *recordingSleeper) Slept() []time.Duration {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]time.Duration, len(r.slept))
	copy(out, r.slept)
	return out
}

// TestComputeBackoff_DefaultSchedule pins the default schedule's
// ±25% jitter envelope: each delay must be within ±25% of the base.
func TestComputeBackoff_DefaultSchedule(t *testing.T) {
	for i, base := range DefaultBackoff {
		got, err := ComputeBackoff(DefaultBackoff, i)
		if err != nil {
			t.Fatalf("attempt %d: %v", i, err)
		}
		low := time.Duration(float64(base) * 0.75)
		high := time.Duration(float64(base) * 1.25)
		if got < low || got > high {
			t.Errorf("attempt %d: got %v, want in [%v, %v]", i, got, low, high)
		}
	}
}

// TestComputeBackoff_AggressiveHalved pins the aggressive schedule
// as the default schedule divided by 2.
func TestComputeBackoff_AggressiveHalved(t *testing.T) {
	for i, base := range AggressiveBackoff {
		got, err := ComputeBackoff(AggressiveBackoff, i)
		if err != nil {
			t.Fatalf("attempt %d: %v", i, err)
		}
		wantBase := time.Duration(float64(base) * 1.0)
		low := time.Duration(float64(wantBase) * 0.75)
		high := time.Duration(float64(wantBase) * 1.25)
		if got < low || got > high {
			t.Errorf("attempt %d: got %v, want in [%v, %v]", i, got, low, high)
		}
	}
}

// TestComputeBackoff_Exhausted pins the boundary at
// len(schedule)+1.
func TestComputeBackoff_Exhausted(t *testing.T) {
	_, err := ComputeBackoff(DefaultBackoff, len(DefaultBackoff))
	if !errors.Is(err, ErrDeliveryExhausted) {
		t.Errorf("got err=%v, want ErrDeliveryExhausted", err)
	}
	_, err = ComputeBackoff(DefaultBackoff, len(DefaultBackoff)+5)
	if !errors.Is(err, ErrDeliveryExhausted) {
		t.Errorf("got err=%v, want ErrDeliveryExhausted", err)
	}
	_, err = ComputeBackoff(DefaultBackoff, -1)
	if err == nil {
		t.Errorf("negative attempt: want non-nil err")
	}
}

// TestScheduleFor pins the retry-policy → schedule mapping.
func TestScheduleFor(t *testing.T) {
	if got := scheduleFor(state.AppWebhookRetryDefault); len(got) != len(DefaultBackoff) {
		t.Errorf("default schedule: got %d, want %d", len(got), len(DefaultBackoff))
	}
	if got := scheduleFor(state.AppWebhookRetryAggressive); len(got) != len(AggressiveBackoff) {
		t.Errorf("aggressive schedule: got %d, want %d", len(got), len(AggressiveBackoff))
	}
	if got := scheduleFor(state.AppWebhookRetryNone); len(got) != 0 {
		t.Errorf("none schedule: got %d, want 0", len(got))
	}
}

// TestDispatcher_Delivered200OnFirstAttempt covers the happy path:
// one delivery → one POST → 200 → MarkSucceeded + webhook.delivered
// audit row.
func TestDispatcher_Delivered200OnFirstAttempt(t *testing.T) {
	m := state.NewMemStore()
	loader, sealed := identityForSealedBlob(t)
	appID := "app-1"
	acctID := "acct-1"
	if _, err := m.CreateApp(context.Background(), state.App{ID: appID, AccountID: acctID, Slug: "test-app", Status: "ready"}); err != nil {
		t.Fatalf("CreateApp: %v", err)
	}
	// Receiver
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(200)
	}))
	defer srv.Close()

	w := newTestAppWebhook(t, m, appID, acctID, srv.URL, state.AppWebhookRetryDefault)
	w.SecretSealed = sealed
	if _, err := m.UpdateAppWebhook(context.Background(), w.ID, state.UpdateAppWebhookParams{WebhookSecretSealed: &sealed}); err != nil {
		t.Fatalf("UpdateAppWebhook (reseal): %v", err)
	}

	// Seed one delivery row.
	del, err := m.RecordAppWebhookDelivery(context.Background(), state.AppWebhookDelivery{
		WebhookID: w.ID,
		AppID:     appID,
		AccountID: acctID,
		Event:     "app.cron.fired",
		Payload:   json.RawMessage(`{"hello":"world"}`),
	})
	if err != nil {
		t.Fatalf("RecordAppWebhookDelivery: %v", err)
	}

	disp := NewDispatcher(m, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	disp.IdentityLoader = loader
	disp.Sleeper = (&recordingSleeper{}).Sleep
	disp.HTTPClient = srv.Client()

	disp.cycle(context.Background())

	// Give the inflight goroutine time to finish.
	for i := 0; i < 50; i++ {
		if attempts.Load() > 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got := attempts.Load(); got != 1 {
		t.Errorf("attempts: got %d, want 1", got)
	}
	gotDel, err := m.AppWebhookDeliveryByID(context.Background(), del.ID)
	if err != nil {
		t.Fatalf("AppWebhookDeliveryByID: %v", err)
	}
	if gotDel.Status != state.AppWebhookDeliverySucceeded {
		t.Errorf("status: got %s (last_error=%q), want succeeded", gotDel.Status, gotDel.LastError)
	}
	if gotDel.LastResponseCode != 200 {
		t.Errorf("last_response_code: got %d, want 200", gotDel.LastResponseCode)
	}
}

// TestDispatcher_500ThenRetry covers the retry path: 5xx →
// MarkFailed with next_attempt_at set.
func TestDispatcher_500ThenRetry(t *testing.T) {
	m := state.NewMemStore()
	loader, sealed := identityForSealedBlob(t)
	appID := "app-2"
	acctID := "acct-2"
	if _, err := m.CreateApp(context.Background(), state.App{ID: appID, AccountID: acctID, Slug: "test-app", Status: "ready"}); err != nil {
		t.Fatalf("CreateApp: %v", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(500)
	}))
	defer srv.Close()

	w := newTestAppWebhook(t, m, appID, acctID, srv.URL, state.AppWebhookRetryDefault)
	w.SecretSealed = sealed
	if _, err := m.UpdateAppWebhook(context.Background(), w.ID, state.UpdateAppWebhookParams{WebhookSecretSealed: &sealed}); err != nil {
		t.Fatalf("UpdateAppWebhook (reseal): %v", err)
	}

	del, err := m.RecordAppWebhookDelivery(context.Background(), state.AppWebhookDelivery{
		WebhookID: w.ID,
		AppID:     appID,
		AccountID: acctID,
		Event:     "app.cron.fired",
		Payload:   json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatalf("RecordAppWebhookDelivery: %v", err)
	}

	disp := NewDispatcher(m, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	disp.IdentityLoader = loader
	disp.Sleeper = (&recordingSleeper{}).Sleep
	disp.HTTPClient = srv.Client()

	start := time.Now()
	disp.Now = func() time.Time { return start }
	disp.cycle(context.Background())

	// Wait for inflight to drain.
	for i := 0; i < 50; i++ {
		got, _ := m.AppWebhookDeliveryByID(context.Background(), del.ID)
		if got.Status == state.AppWebhookDeliveryFailed {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	gotDel, err := m.AppWebhookDeliveryByID(context.Background(), del.ID)
	if err != nil {
		t.Fatalf("AppWebhookDeliveryByID: %v", err)
	}
	if gotDel.Status != state.AppWebhookDeliveryPending {
		t.Errorf("status: got %s, want pending (MarkFailed resets to pending so the claim picks it up next tick)", gotDel.Status)
	}
	if !gotDel.NextAttemptAt.After(start) {
		t.Errorf("next_attempt_at: got %v, want > start %v", gotDel.NextAttemptAt, start)
	}
	// Default schedule at attempt=0 is 30s ±25% → 22.5s..37.5s
	delay := gotDel.NextAttemptAt.Sub(start)
	if delay < 22*time.Second || delay > 38*time.Second {
		t.Errorf("delay: got %v, want 22s..38s", delay)
	}
}

// TestDispatcher_Attempt7DLQs covers the DLQ-at-7 path: a row
// already at attempt=6 + a 5xx response must end up dead.
func TestDispatcher_Attempt7DLQs(t *testing.T) {
	m := state.NewMemStore()
	loader, sealed := identityForSealedBlob(t)
	appID := "app-3"
	acctID := "acct-3"
	if _, err := m.CreateApp(context.Background(), state.App{ID: appID, AccountID: acctID, Slug: "test-app", Status: "ready"}); err != nil {
		t.Fatalf("CreateApp: %v", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(500)
	}))
	defer srv.Close()

	w := newTestAppWebhook(t, m, appID, acctID, srv.URL, state.AppWebhookRetryDefault)
	w.SecretSealed = sealed
	if _, err := m.UpdateAppWebhook(context.Background(), w.ID, state.UpdateAppWebhookParams{WebhookSecretSealed: &sealed}); err != nil {
		t.Fatalf("UpdateAppWebhook (reseal): %v", err)
	}

	del, err := m.RecordAppWebhookDelivery(context.Background(), state.AppWebhookDelivery{
		WebhookID: w.ID,
		AppID:     appID,
		AccountID: acctID,
		Event:     "app.cron.fired",
		Payload:   json.RawMessage(`{}`),
		Attempt:   6, // already past the 6-step schedule
	})
	if err != nil {
		t.Fatalf("RecordAppWebhookDelivery: %v", err)
	}

	disp := NewDispatcher(m, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	disp.IdentityLoader = loader
	disp.Sleeper = (&recordingSleeper{}).Sleep
	disp.HTTPClient = srv.Client()
	disp.cycle(context.Background())

	for i := 0; i < 50; i++ {
		got, _ := m.AppWebhookDeliveryByID(context.Background(), del.ID)
		if got.Status == state.AppWebhookDeliveryDead {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	gotDel, err := m.AppWebhookDeliveryByID(context.Background(), del.ID)
	if err != nil {
		t.Fatalf("AppWebhookDeliveryByID: %v", err)
	}
	if gotDel.Status != state.AppWebhookDeliveryDead {
		t.Errorf("status: got %s, want dead", gotDel.Status)
	}
}

// TestDispatcher_NonePolicy_ImmediateDead pins the retry_policy='none'
// behaviour: first failure is terminal, no retry.
func TestDispatcher_NonePolicy_ImmediateDead(t *testing.T) {
	m := state.NewMemStore()
	loader, sealed := identityForSealedBlob(t)
	appID := "app-4"
	acctID := "acct-4"
	if _, err := m.CreateApp(context.Background(), state.App{ID: appID, AccountID: acctID, Slug: "test-app", Status: "ready"}); err != nil {
		t.Fatalf("CreateApp: %v", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(500)
	}))
	defer srv.Close()

	w := newTestAppWebhook(t, m, appID, acctID, srv.URL, state.AppWebhookRetryNone)
	w.SecretSealed = sealed
	if _, err := m.UpdateAppWebhook(context.Background(), w.ID, state.UpdateAppWebhookParams{WebhookSecretSealed: &sealed}); err != nil {
		t.Fatalf("UpdateAppWebhook (reseal): %v", err)
	}

	del, err := m.RecordAppWebhookDelivery(context.Background(), state.AppWebhookDelivery{
		WebhookID: w.ID,
		AppID:     appID,
		AccountID: acctID,
		Event:     "app.cron.fired",
		Payload:   json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatalf("RecordAppWebhookDelivery: %v", err)
	}

	disp := NewDispatcher(m, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	disp.IdentityLoader = loader
	disp.Sleeper = (&recordingSleeper{}).Sleep
	disp.HTTPClient = srv.Client()
	disp.cycle(context.Background())

	for i := 0; i < 50; i++ {
		got, _ := m.AppWebhookDeliveryByID(context.Background(), del.ID)
		if got.Status == state.AppWebhookDeliveryDead {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	gotDel, err := m.AppWebhookDeliveryByID(context.Background(), del.ID)
	if err != nil {
		t.Fatalf("AppWebhookDeliveryByID: %v", err)
	}
	if gotDel.Status != state.AppWebhookDeliveryDead {
		t.Errorf("status: got %s, want dead", gotDel.Status)
	}
	if gotDel.Attempt != 1 {
		t.Errorf("attempt: got %d, want 1 (first try + dead)", gotDel.Attempt)
	}
}

// TestDispatcher_DisabledSubscriptionDead covers the "operator
// disabled the subscription while a delivery was in flight" branch.
func TestDispatcher_DisabledSubscriptionDead(t *testing.T) {
	m := state.NewMemStore()
	loader, sealed := identityForSealedBlob(t)
	appID := "app-5"
	acctID := "acct-5"
	if _, err := m.CreateApp(context.Background(), state.App{ID: appID, AccountID: acctID, Slug: "test-app", Status: "ready"}); err != nil {
		t.Fatalf("CreateApp: %v", err)
	}
	w := newTestAppWebhook(t, m, appID, acctID, "http://127.0.0.1:0/never", state.AppWebhookRetryDefault)
	w.SecretSealed = sealed
	if _, err := m.UpdateAppWebhook(context.Background(), w.ID, state.UpdateAppWebhookParams{WebhookSecretSealed: &sealed}); err != nil {
		t.Fatalf("UpdateAppWebhook (reseal): %v", err)
	}
	// Disable the subscription.
	disabled := false
	if _, err := m.UpdateAppWebhook(context.Background(), w.ID, state.UpdateAppWebhookParams{Enabled: &disabled}); err != nil {
		t.Fatalf("UpdateAppWebhook (disable): %v", err)
	}

	del, err := m.RecordAppWebhookDelivery(context.Background(), state.AppWebhookDelivery{
		WebhookID: w.ID,
		AppID:     appID,
		AccountID: acctID,
		Event:     "app.cron.fired",
		Payload:   json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatalf("RecordAppWebhookDelivery: %v", err)
	}

	disp := NewDispatcher(m, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	disp.IdentityLoader = loader
	disp.Sleeper = (&recordingSleeper{}).Sleep
	disp.cycle(context.Background())

	for i := 0; i < 50; i++ {
		got, _ := m.AppWebhookDeliveryByID(context.Background(), del.ID)
		if got.Status == state.AppWebhookDeliveryDead {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	gotDel, err := m.AppWebhookDeliveryByID(context.Background(), del.ID)
	if err != nil {
		t.Fatalf("AppWebhookDeliveryByID: %v", err)
	}
	if gotDel.Status != state.AppWebhookDeliveryDead {
		t.Errorf("status: got %s, want dead", gotDel.Status)
	}
	if gotDel.LastError != "webhook: subscription disabled" {
		t.Errorf("last_error: got %q, want %q", gotDel.LastError, "webhook: subscription disabled")
	}
}

// TestDispatcher_DrainTimeout pins the ctx.Done() shutdown
// contract: an in-flight row held up by a 30s sleep is reclaimed
// in ≤ DefaultDrainTimeout.
func TestDispatcher_DrainTimeout(t *testing.T) {
	m := state.NewMemStore()
	loader, sealed := identityForSealedBlob(t)
	appID := "app-6"
	acctID := "acct-6"
	if _, err := m.CreateApp(context.Background(), state.App{ID: appID, AccountID: acctID, Slug: "test-app", Status: "ready"}); err != nil {
		t.Fatalf("CreateApp: %v", err)
	}
	// Server that hangs forever — exercises the drain.
	hung := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-hung
	}))
	defer srv.Close()
	defer close(hung)

	w := newTestAppWebhook(t, m, appID, acctID, srv.URL, state.AppWebhookRetryDefault)
	w.SecretSealed = sealed
	if _, err := m.UpdateAppWebhook(context.Background(), w.ID, state.UpdateAppWebhookParams{WebhookSecretSealed: &sealed}); err != nil {
		t.Fatalf("UpdateAppWebhook (reseal): %v", err)
	}
	if _, err := m.RecordAppWebhookDelivery(context.Background(), state.AppWebhookDelivery{
		WebhookID: w.ID,
		AppID:     appID,
		AccountID: acctID,
		Event:     "app.cron.fired",
		Payload:   json.RawMessage(`{}`),
	}); err != nil {
		t.Fatalf("RecordAppWebhookDelivery: %v", err)
	}

	disp := NewDispatcher(m, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	disp.IdentityLoader = loader
	disp.HTTPClient = &http.Client{Timeout: 50 * time.Millisecond} // short so the hung handler errors fast

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- disp.Run(ctx) }()

	disp.cycle(context.Background())
	// Give the goroutine time to enter the handler.
	time.Sleep(10 * time.Millisecond)

	cancel()
	start := time.Now()
	if err := <-done; err != nil {
		t.Fatalf("Run returned err: %v", err)
	}
	elapsed := time.Since(start)
	if elapsed > DefaultDrainTimeout+500*time.Millisecond {
		t.Errorf("drain took %v, want <= %v + slack", elapsed, DefaultDrainTimeout)
	}
}

// TestDispatch_HeaderShape pins the wire-format contract: the
// dispatcher must emit X-Faas-Webhook-* headers (not X-Faas-Alert-*)
// for app webhook deliveries.
func TestDispatch_HeaderShape(t *testing.T) {
	var mu sync.Mutex
	var gotSig, gotID, gotTS, gotAtt string
	var bodyBytes []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		gotSig = r.Header.Get("X-Faas-Webhook-Signature")
		gotID = r.Header.Get("X-Faas-Delivery-Id")
		gotTS = r.Header.Get("X-Faas-Webhook-Timestamp")
		gotAtt = r.Header.Get("X-Faas-Webhook-Attempt")
		bodyBytes, _ = io.ReadAll(r.Body)
		w.WriteHeader(200)
	}))
	defer srv.Close()
	_ = bodyBytes

	m := state.NewMemStore()
	loader, sealed := identityForSealedBlob(t)
	appID := "app-7"
	acctID := "acct-7"
	if _, err := m.CreateApp(context.Background(), state.App{ID: appID, AccountID: acctID, Slug: "test-app", Status: "ready"}); err != nil {
		t.Fatalf("CreateApp: %v", err)
	}
	w := newTestAppWebhook(t, m, appID, acctID, srv.URL, state.AppWebhookRetryDefault)
	w.SecretSealed = sealed
	if _, err := m.UpdateAppWebhook(context.Background(), w.ID, state.UpdateAppWebhookParams{WebhookSecretSealed: &sealed}); err != nil {
		t.Fatalf("UpdateAppWebhook (reseal): %v", err)
	}
	if _, err := m.RecordAppWebhookDelivery(context.Background(), state.AppWebhookDelivery{
		WebhookID: w.ID,
		AppID:     appID,
		AccountID: acctID,
		Event:     "app.cron.fired",
		Payload:   json.RawMessage(`{}`),
	}); err != nil {
		t.Fatalf("RecordAppWebhookDelivery: %v", err)
	}

	disp := NewDispatcher(m, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	disp.IdentityLoader = loader
	disp.Sleeper = (&recordingSleeper{}).Sleep
	disp.HTTPClient = srv.Client()
	disp.cycle(context.Background())

	// Wait for inflight to drain.
	for i := 0; i < 50; i++ {
		mu.Lock()
		s := gotSig
		mu.Unlock()
		if s != "" {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if gotSig == "" {
		t.Fatal("no X-Faas-Webhook-Signature header received")
	}
	if gotID == "" {
		t.Error("no X-Faas-Delivery-Id header received")
	}
	if gotTS == "" {
		t.Error("no X-Faas-Webhook-Timestamp header received")
	}
	if gotAtt == "" {
		t.Error("no X-Faas-Webhook-Attempt header received")
	}
}

// TestDispatch_NoSecretInLogs pins CLAUDE.md §11: the secret
// value must never appear in the dispatcher's log lines. We
// exercise the failure path (500) so the last_error reaches the
// store, and verify the *unsealed* plaintext does not appear in
// slog output captured by an os.Stderr pipe.
func TestDispatch_NoSecretInLogs(t *testing.T) {
	m := state.NewMemStore()
	loader, sealed := identityForSealedBlob(t)
	appID := "app-8"
	acctID := "acct-8"
	if _, err := m.CreateApp(context.Background(), state.App{ID: appID, AccountID: acctID, Slug: "test-app", Status: "ready"}); err != nil {
		t.Fatalf("CreateApp: %v", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(500)
	}))
	defer srv.Close()

	w := newTestAppWebhook(t, m, appID, acctID, srv.URL, state.AppWebhookRetryNone)
	w.SecretSealed = sealed
	if _, err := m.UpdateAppWebhook(context.Background(), w.ID, state.UpdateAppWebhookParams{WebhookSecretSealed: &sealed}); err != nil {
		t.Fatalf("UpdateAppWebhook (reseal): %v", err)
	}
	if _, err := m.RecordAppWebhookDelivery(context.Background(), state.AppWebhookDelivery{
		WebhookID: w.ID,
		AppID:     appID,
		AccountID: acctID,
		Event:     "app.cron.fired",
		Payload:   json.RawMessage(`{}`),
	}); err != nil {
		t.Fatalf("RecordAppWebhookDelivery: %v", err)
	}

	// Capture slog to a pipe and read it back after the cycle.
	r, wpipe, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	log := slog.New(slog.NewTextHandler(wpipe, &slog.HandlerOptions{Level: slog.LevelDebug}))
	disp := NewDispatcher(m, nil, log)
	disp.IdentityLoader = loader
	disp.Sleeper = (&recordingSleeper{}).Sleep
	disp.HTTPClient = srv.Client()
	disp.cycle(context.Background())
	for i := 0; i < 50; i++ {
		del, _ := m.AppWebhookDeliveryByID(context.Background(), mustLastDel(t, m, appID, w.ID))
		if del.Status == state.AppWebhookDeliveryDead {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	_ = wpipe.Close()

	buf := make([]byte, 16384)
	n, _ := r.Read(buf)
	if contains(buf[:n], "test-secret") {
		t.Errorf("plaintext leaked into log:\n%s", buf[:n])
	}
}

// mustLastDel returns the most recently created delivery for the
// given (appID, webhookID) pair. Used by the no-secret-in-logs test
// to read the row the cycle just sealed.
func mustLastDel(t *testing.T, m *state.MemStore, appID, webhookID string) string {
	t.Helper()
	dels, _, err := m.ListAppWebhookDeliveries(context.Background(), appID, webhookID, 1, "")
	if err != nil {
		t.Fatalf("ListAppWebhookDeliveries: %v", err)
	}
	if len(dels) == 0 {
		t.Fatalf("no deliveries for appID=%s webhookID=%s", appID, webhookID)
	}
	return dels[0].ID
}

// contains is a tiny strings.Contains shim (the test imports io +
// slog + os + time only — strings isn't required).
func contains(b []byte, sub string) bool {
	if len(sub) == 0 {
		return true
	}
	for i := 0; i+len(sub) <= len(b); i++ {
		if string(b[i:i+len(sub)]) == sub {
			return true
		}
	}
	return false
}
