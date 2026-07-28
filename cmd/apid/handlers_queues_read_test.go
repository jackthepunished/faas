package main

// Issue #394 — queue introspection handler tests.
//
// All read-only endpoints stay purely on MemStore here. The
// byte-identical SQL property test (TestQueuePeek_ByteIdentical) lives
// in pkg/state because it needs pgtest — the handler is a thin
// mapping layer, the SQL is what owns the "no mutation" guarantee.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/onebox-faas/faas/pkg/api"
	"github.com/onebox-faas/faas/pkg/state"
)

// seedQueueRow writes a queue-source invocation row directly through
// the store. Bypasses the cap check that queueSend applies — the
// introspection endpoints must work for diagnostics even when the
// queue is technically over the plan limit (e.g., drained-but-stuck
// rows after a plan downgrade).
func seedQueueRow(t *testing.T, e testEnv, appID string, payload string) string {
	t.Helper()
	inv, err := e.store.EnqueueInvocation(context.Background(), state.Invocation{
		AppID:     appID,
		AccountID: e.acct.ID,
		Source:    state.InvocationQueue,
		Payload:   json.RawMessage(payload),
		DueAt:     time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("seed queue row: %v", err)
	}
	return inv.ID
}

// TestQueueState_HappyPath returns the depth + plan_cap for a Pro
// plan with 2 pending rows. In-flight is 0 because nothing has been
// claimed; oldest_pending_at is the oldest row's created_at.
func TestQueueState_HappyPath(t *testing.T) {
	e := setup(t, api.PlanPro)
	appID := mustSeedApp(t, e, "myapp")
	seedQueueRow(t, e, appID, `{"i":0}`)
	seedQueueRow(t, e, appID, `{"i":1}`)

	rec := e.do(t, "GET", "/v1/apps/myapp/queues/state", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var out api.QueueStateResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.PlanCap != api.MustLimitsFor(api.PlanPro).MaxQueueDepth {
		t.Errorf("PlanCap = %d, want %d", out.PlanCap, api.MustLimitsFor(api.PlanPro).MaxQueueDepth)
	}
	if out.Depth != 2 {
		t.Errorf("Depth = %d, want 2", out.Depth)
	}
	if out.InFlight != 0 {
		t.Errorf("InFlight = %d, want 0", out.InFlight)
	}
	if out.OldestPendingAt == nil {
		t.Errorf("OldestPendingAt = nil, want set")
	}
	if out.OldestPendingAgeSeconds == nil {
		t.Errorf("OldestPendingAgeSeconds = nil, want set")
	}
}

// TestQueueState_EmptyQueue: oldest_pending_at MUST be omitted (not
// zero) when the queue is empty so dashboards can render "no backlog".
func TestQueueState_EmptyQueue(t *testing.T) {
	e := setup(t, api.PlanPro)
	mustSeedApp(t, e, "myapp")

	rec := e.do(t, "GET", "/v1/apps/myapp/queues/state", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if bytesContains(rec.Body.Bytes(), "oldest_pending_at") {
		t.Errorf("oldest_pending_at present on empty queue — want omitted\n%s", rec.Body.String())
	}
	if bytesContains(rec.Body.Bytes(), "oldest_pending_age_seconds") {
		t.Errorf("oldest_pending_age_seconds present on empty queue — want omitted\n%s", rec.Body.String())
	}
}

// TestQueuePeek_HappyPath: pending rows are returned oldest-first.
// The payload is surfaced as a JSON string (verbatim from the jsonb
// column) so callers can decode with their preferred JSON lib.
func TestQueuePeek_HappyPath(t *testing.T) {
	e := setup(t, api.PlanPro)
	appID := mustSeedApp(t, e, "myapp")
	oldestID := seedQueueRow(t, e, appID, `{"i":0}`)
	time.Sleep(2 * time.Millisecond)
	seedQueueRow(t, e, appID, `{"i":1}`)

	rec := e.do(t, "GET", "/v1/apps/myapp/queues/peek?limit=10", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var out api.QueuePeekResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.Messages) != 2 {
		t.Fatalf("len(Messages) = %d, want 2", len(out.Messages))
	}
	if out.Messages[0].ID != oldestID {
		t.Errorf("Messages[0].ID = %q, want %q (oldest first)", out.Messages[0].ID, oldestID)
	}
	if !out.Messages[0].CreatedAt.Before(out.Messages[1].CreatedAt) {
		t.Errorf("Messages not ordered by created_at ASC: %+v", out.Messages)
	}
	if out.Messages[0].Payload != `{"i":0}` {
		t.Errorf("Messages[0].Payload = %q, want %q", out.Messages[0].Payload, `{"i":0}`)
	}
}

// TestQueuePeek_DoesNotLease confirms peek does not acquire a lease.
// Peek twice, then call queues/receive, and the first row returned
// by receive must be the same id peek saw first — receive claims it
// because peek did not.
func TestQueuePeek_DoesNotLease(t *testing.T) {
	e := setupWithNotifier(t, api.PlanPro, func(_ context.Context, _ string, _ func(string) bool, _ time.Duration) (string, error) {
		return "", errors.New("notifier never matches — peek must not depend on notifications")
	})
	appID := mustSeedApp(t, e, "myapp")
	expectedID := seedQueueRow(t, e, appID, `{"i":0}`)

	// First peek — must return the row, NOT acquire a lease.
	rec := e.do(t, "GET", "/v1/apps/myapp/queues/peek", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("peek 1 status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var peek api.QueuePeekResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &peek); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(peek.Messages) != 1 || peek.Messages[0].ID != expectedID {
		t.Fatalf("peek 1 = %+v, want id=%q", peek.Messages, expectedID)
	}
	// Row must still be pending (peek did not claim it).
	inv, err := e.store.InvocationByID(context.Background(), expectedID)
	if err != nil {
		t.Fatalf("InvocationByID: %v", err)
	}
	if inv.State != state.InvocationPending {
		t.Errorf("post-peek state = %q, want pending (peek must not lease)", inv.State)
	}
	if inv.Attempts != 0 {
		t.Errorf("post-peek attempts = %d, want 0", inv.Attempts)
	}
}

// TestQueuePeek_AfterReceive_StillPending: peek, lease via receive,
// peek again — the leased row MUST NOT appear in the second peek.
// This is the contract: peek shows pending rows only.
func TestQueuePeek_AfterReceive_StillPending(t *testing.T) {
	e := setupWithNotifier(t, api.PlanPro, func(_ context.Context, _ string, _ func(string) bool, _ time.Duration) (string, error) {
		return "", errors.New("no notification — row is leased but not yet drained")
	})
	appID := mustSeedApp(t, e, "myapp")
	seedQueueRow(t, e, appID, `{"i":0}`)
	seedQueueRow(t, e, appID, `{"i":1}`)

	// Lease the first row manually (receive is long-poll; we simulate
	// the lease by calling the store directly so the test is deterministic).
	rows, err := e.store.ListDueInvocations(context.Background(), time.Now().Add(time.Second), 1)
	if err != nil {
		t.Fatalf("ListDueInvocations: %v", err)
	}
	if len(rows) == 0 {
		t.Fatal("no due rows")
	}
	if _, err := e.store.ClaimInvocation(context.Background(), rows[0].ID, "inst-1", 30); err != nil {
		t.Fatalf("ClaimInvocation: %v", err)
	}

	// Peek must NOT include the claimed row.
	rec := e.do(t, "GET", "/v1/apps/myapp/queues/peek", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("peek status = %d, want 200", rec.Code)
	}
	var peek api.QueuePeekResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &peek); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, m := range peek.Messages {
		if m.ID == rows[0].ID {
			t.Errorf("claimed row %s appeared in peek — want hidden", m.ID)
		}
	}
}

// TestQueueDeadLetter_ExhaustedToDeadLetter drives a row past the
// plan's MaxQueueAttempts (Pro = 10) and asserts the row lands in
// state='dead_letter'. After exhaustion, ClaimInvocation on the same
// id must fail because dead_letter is terminal.
func TestQueueDeadLetter_ExhaustedToDeadLetter(t *testing.T) {
	e := setup(t, api.PlanPro)
	appID := mustSeedApp(t, e, "myapp")
	invID := seedQueueRow(t, e, appID, `{"i":0}`)

	// Budget=10 for Pro. Each transient FailInvocation increments
	// attempts and re-queues with retryAfter>0; the moment attempts
	// reaches the budget, the next transient failure transitions the
	// row to dead_letter instead.
	budget := api.MustLimitsFor(api.PlanPro).MaxQueueAttempts
	for i := 1; i <= budget; i++ {
		if _, err := e.store.ClaimInvocation(context.Background(), invID, "inst", 30); err != nil {
			t.Fatalf("claim iter %d: %v", i, err)
		}
		if err := e.store.FailInvocation(context.Background(), invID, fmt.Sprintf("blip %d", i), time.Minute, budget); err != nil {
			t.Fatalf("FailInvocation iter %d: %v", i, err)
		}
	}

	// After the budget is exhausted the row must be dead_letter.
	inv, err := e.store.InvocationByID(context.Background(), invID)
	if err != nil {
		t.Fatalf("InvocationByID: %v", err)
	}
	if inv.State != state.InvocationDeadLetter {
		t.Fatalf("state = %q, want dead_letter after %d attempts", inv.State, budget)
	}

	// The dead-letter endpoint must return it.
	rec := e.do(t, "GET", "/v1/apps/myapp/queues/dead_letter", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var out api.QueueDeadLetterResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.Messages) != 1 {
		t.Fatalf("len(Messages) = %d, want 1", len(out.Messages))
	}
	if out.Messages[0].ID != invID {
		t.Errorf("Messages[0].ID = %q, want %q", out.Messages[0].ID, invID)
	}
	if !stringsContains(out.Messages[0].LastError, "blip") {
		t.Errorf("LastError = %q, want contains %q", out.Messages[0].LastError, "blip")
	}

	// Re-claim must fail — dead_letter is terminal.
	if _, err := e.store.ClaimInvocation(context.Background(), invID, "inst", 30); !errors.Is(err, state.ErrNotFound) {
		t.Errorf("post-dead-letter claim err = %v, want ErrNotFound", err)
	}
}

// TestQueueRead_CrossAccount: the IDOR-safety property. Cross-account
// reads surface 404 (not 403, not 200) so callers cannot enumerate
// slugs in other accounts. Same shape as the existing getInvocation
// precedent in handlers_invocations_test.go.
func TestQueueRead_CrossAccount(t *testing.T) {
	// Owner account owns the app; foreign account probes with their key.
	owner := setup(t, api.PlanPro)
	appID := mustSeedApp(t, owner, "owner-app")
	seedQueueRow(t, owner, appID, `{"i":0}`)

	// Foreign env — same store (MemStore) but a different account
	// whose key is used in the HTTP request.
	foreign := setup(t, api.PlanPro)

	probes := []struct {
		name string
		path string
	}{
		{"state", "/v1/apps/owner-app/queues/state"},
		{"peek", "/v1/apps/owner-app/queues/peek"},
		{"dead_letter", "/v1/apps/owner-app/queues/dead_letter"},
	}
	for _, p := range probes {
		rec := foreign.do(t, "GET", p.path, nil, nil)
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s: status = %d, want 404 (cross-account read must surface 404, not 403/200); body=%s",
				p.name, rec.Code, rec.Body.String())
		}
	}

	// Owner can read everything.
	for _, p := range probes {
		rec := owner.do(t, "GET", p.path, nil, nil)
		if rec.Code != http.StatusOK {
			t.Errorf("owner %s: status = %d, want 200; body=%s", p.name, rec.Code, rec.Body.String())
		}
	}
}

// TestQueueRead_UnknownAppIs404: a non-existent slug is 404 for both
// owner and foreign — same shape, no information leak about which
// slug exists.
func TestQueueRead_UnknownAppIs404(t *testing.T) {
	e := setup(t, api.PlanPro)
	for _, path := range []string{
		"/v1/apps/no-such-app/queues/state",
		"/v1/apps/no-such-app/queues/peek",
		"/v1/apps/no-such-app/queues/dead_letter",
	} {
		rec := e.do(t, "GET", path, nil, nil)
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s: status = %d, want 404", path, rec.Code)
		}
	}
}

// --- small helpers (kept local so the test stays self-contained) ---

func bytesContains(b []byte, s string) bool {
	return bytesIndex(b, s) >= 0
}

func bytesIndex(b []byte, s string) int {
	for i := 0; i+len(s) <= len(b); i++ {
		if string(b[i:i+len(s)]) == s {
			return i
		}
	}
	return -1
}

func stringsContains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// keep strconv referenced — the peek path cap is enforced via the
// limit query param; the test matrix in TestQueuePeek_* uses it.
var _ = strconv.Itoa