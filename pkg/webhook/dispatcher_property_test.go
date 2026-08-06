// Per-account fairness property test (issue #476 / ADR-076).
//
// Property:
//
//	Given N accounts each with equal pending delivery depth, the
//	dispatcher's claim query must round-robin so no account gets
//	more than ceil(32/N) deliveries/tick over a 10-tick window.
//
// This is hand-rolled assertion (not pgregory.net/rapid) to match
// the precedent in pkg/sched/engine_test.go (memory:
// invariants-property-test-fakevmm-reuse).
package webhook

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/onebox-faas/faas/pkg/state"
)

// TestDispatcher_Fairness_PerAccountRoundRobin drives 10 cycles
// against 5 accounts × 100 pending rows each and asserts the
// max-min deliveries-per-account gap is ≤ 2 (within the
// expected round-robin envelope).
func TestDispatcher_Fairness_PerAccountRoundRobin(t *testing.T) {
	const (
		accounts   = 5
		perAccount = 100
		ticks      = 10
		cap        = 32
	)

	// Single 200-returning receiver; per-account fairness is about
	// which rows the dispatcher *picks*, not whether they succeed.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(200)
	}))
	defer srv.Close()

	loader, sealed := identityForSealedBlob(t)
	m := state.NewMemStore()
	var accountIDs []string
	// One webhook per account; deliveries fan-out from it.
	for i := 0; i < accounts; i++ {
		acct := fmt.Sprintf("acct-%d", i)
		appID := fmt.Sprintf("app-%d", i)
		accountIDs = append(accountIDs, acct)
		if _, err := m.CreateApp(context.Background(), state.App{ID: appID, AccountID: acct, Slug: fmt.Sprintf("fairness-app-%d", i), Status: "ready"}); err != nil {
			t.Fatalf("CreateApp[%d]: %v", i, err)
		}
		w := newTestAppWebhook(t, m, appID, acct, srv.URL, state.AppWebhookRetryDefault)
		w.SecretSealed = sealed
		if _, err := m.UpdateAppWebhook(context.Background(), w.ID, state.UpdateAppWebhookParams{WebhookSecretSealed: &sealed}); err != nil {
			t.Fatalf("UpdateAppWebhook[%d]: %v", i, err)
		}
		for j := 0; j < perAccount; j++ {
			if _, err := m.RecordAppWebhookDelivery(context.Background(), state.AppWebhookDelivery{
				WebhookID: w.ID,
				AppID:     appID,
				AccountID: acct,
				Event:     "app.cron.fired",
				Payload:   json.RawMessage(`{}`),
			}); err != nil {
				t.Fatalf("RecordAppWebhookDelivery[%d/%d]: %v", i, j, err)
			}
		}
	}

	disp := NewDispatcher(m, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	disp.IdentityLoader = loader
	disp.Sleeper = (&recordingSleeper{}).Sleep
	disp.HTTPClient = srv.Client()
	disp.Cap = cap

	// Drive the cycle synchronously so we can observe after each
	// tick without an inflight race.
	var succeededPerAccount [accounts]int
	for tk := 0; tk < ticks; tk++ {
		disp.cycle(context.Background())
		// cycle() fires goroutines via disp.inflight; sleep long
		// enough for them to land MarkSucceeded calls on the
		// MemStore before the per-tick snapshot.
		time.Sleep(20 * time.Millisecond)
		for i, acct := range accountIDs {
			deliveries, _, err := m.ListAppWebhookDeliveries(context.Background(), fmt.Sprintf("app-%d", i), "", 0, "")
			if err != nil {
				t.Fatalf("ListAppWebhookDeliveries[%d]: %v", i, err)
			}
			for _, d := range deliveries {
				if d.AccountID == acct && d.Status == state.AppWebhookDeliverySucceeded {
					succeededPerAccount[i]++
				}
			}
		}
	}

	// Compute max-min gap. With cap=32 across 5 accounts the
	// expected per-tick envelope is ceil(32/5) = 7; the round-robin
	// claim query keeps the cumulative gap small.
	var min, max int
	for i := 0; i < accounts; i++ {
		v := succeededPerAccount[i]
		if i == 0 || v < min {
			min = v
		}
		if v > max {
			max = v
		}
	}
	gap := max - min
	if gap > 2 {
		t.Errorf("fairness gap: got %d, want <= 2 (min=%d max=%d per-account=%v)",
			gap, min, max, succeededPerAccount)
	}
}
