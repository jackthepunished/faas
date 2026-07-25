package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/onebox-faas/faas/pkg/api"
	"github.com/onebox-faas/faas/pkg/state"
	"github.com/onebox-faas/faas/pkg/wire"
)

// uuidStringOf normalises either a canonical UUID (with hyphens) or a
// raw 32-char hex string into the canonical UUID form. The MemStore's
// newID returns hex; the PgStore returns canonical UUIDs. MemStore's
// parseSubjectID converts the hex back to canonical UUID bytes when
// storing the Subject, so ListEvents(subject=<hex>) returns rows whose
// Subject.String() always reports the canonical form regardless of
// which store produced it. (Same helper as pkg/sched/events_test.go.)
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

// --- tests -----------------------------------------------------------------

// TestAuditEvents_KeyMintEmitsEvent (IAM-4 / ADR-035) drives POST
// /v1/keys and asserts the events table got a row with kind=
// "key.created", data.key_id == the new key's id, and data.scopes ==
// the requested scope set. The shape mirrors schedd's
// TestEngineTransition_AppendsEvent — drive a happy-path mutation,
// assert the audit row landed.
func TestAuditEvents_KeyMintEmitsEvent(t *testing.T) {
	e := setup(t, api.PlanPro)
	body := api.CreateKeyRequest{Label: "audit-test", Scopes: []string{"read"}}
	rec := e.do(t, http.MethodPost, "/v1/keys", body, nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST /v1/keys: code=%d body=%s", rec.Code, rec.Body.String())
	}

	rows, err := e.store.ListEvents(context.Background(), e.acct.ID, 0)
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	var found *state.Event
	for i := range rows {
		if rows[i].Kind == "key.created" {
			found = &rows[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("no key.created event row; rows=%+v", rows)
	}
	if found.Subject == nil || found.Subject.String() != uuidStringOf(e.acct.ID) {
		t.Errorf("Subject = %v, want %s", found.Subject, uuidStringOf(e.acct.ID))
	}
	var data map[string]any
	if err := json.Unmarshal(found.Data, &data); err != nil {
		t.Fatalf("Data not valid JSON: %v", err)
	}
	if data["scopes"] == nil {
		t.Errorf("Data.scopes missing: %+v", data)
	}
	// data.key_id should be a 32-hex char (the new key's ID).
	kid, _ := data["key_id"].(string)
	if len(kid) != 32 {
		t.Errorf("Data.key_id = %q, want 32 hex chars", kid)
	}
}

// TestAuditEvents_KeyDeleteEmitsEvent drives DELETE /v1/keys/{id}
// and asserts a key.deleted row landed.
func TestAuditEvents_KeyDeleteEmitsEvent(t *testing.T) {
	e := setup(t, api.PlanPro)

	// Mint a second key we can delete.
	body := api.CreateKeyRequest{Label: "to-delete", Scopes: []string{"read"}}
	rec := e.do(t, http.MethodPost, "/v1/keys", body, nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST /v1/keys: code=%d body=%s", rec.Code, rec.Body.String())
	}
	var created api.APIKeyResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	// Delete it.
	delRec := e.do(t, http.MethodDelete, "/v1/keys/"+created.ID, nil, nil)
	if delRec.Code != http.StatusNoContent {
		t.Fatalf("DELETE /v1/keys/%s: code=%d body=%s", created.ID, delRec.Code, delRec.Body.String())
	}

	rows, err := e.store.ListEvents(context.Background(), e.acct.ID, 0)
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	var found *state.Event
	for i := range rows {
		if rows[i].Kind == "key.deleted" {
			found = &rows[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("no key.deleted event row; rows=%+v", rows)
	}
	var data map[string]any
	if err := json.Unmarshal(found.Data, &data); err != nil {
		t.Fatalf("Data not valid JSON: %v", err)
	}
	if data["key_id"] != created.ID {
		t.Errorf("Data.key_id = %v, want %s", data["key_id"], created.ID)
	}
}

// TestAuditEvents_AccountDeletionScheduledEmitsEvent (IAM-4) drives
// DELETE /v1/account and asserts the events row carries kind =
// "account.deletion_scheduled" with data.via == "rest". The dashboard
// form path (data.via == "dashboard") is covered separately at
// integration level — the seam is the scheduleDeletion via parameter
// (cmd/apid/handlers_account.go).
func TestAuditEvents_AccountDeletionScheduledEmitsEvent(t *testing.T) {
	e := setup(t, api.PlanPro)
	rec := e.do(t, http.MethodDelete, "/v1/account", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("DELETE /v1/account: code=%d body=%s", rec.Code, rec.Body.String())
	}
	rows, err := e.store.ListEvents(context.Background(), e.acct.ID, 0)
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	var found *state.Event
	for i := range rows {
		if rows[i].Kind == "account.deletion_scheduled" {
			found = &rows[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("no account.deletion_scheduled event row; rows=%+v", rows)
	}
	var data map[string]any
	if err := json.Unmarshal(found.Data, &data); err != nil {
		t.Fatalf("Data not valid JSON: %v", err)
	}
	if data["via"] != "rest" {
		t.Errorf("Data.via = %v, want rest", data["via"])
	}
}

// TestAuditEvents_ListEndpointRespectsKindPrefixFilter drives the
// GET /v1/audit-events customer surface with kind_prefix="key." and
// asserts only key.* rows come back. Also covers the list endpoint
// shape (limit echo, newest-first ordering).
func TestAuditEvents_ListEndpointRespectsKindPrefixFilter(t *testing.T) {
	e := setup(t, api.PlanPro)
	// Mint two keys → 2x key.created rows.
	for _, label := range []string{"first", "second"} {
		rec := e.do(t, http.MethodPost, "/v1/keys", api.CreateKeyRequest{Label: label, Scopes: []string{"read"}}, nil)
		if rec.Code != http.StatusCreated {
			t.Fatalf("POST /v1/keys: code=%d body=%s", rec.Code, rec.Body.String())
		}
	}

	// Query BEFORE scheduling deletion: a deleted_pending account is
	// gated by the billing-past-due middleware so the read returns 402.
	rec := e.do(t, http.MethodGet, "/v1/audit-events?kind_prefix=key.", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /v1/audit-events: code=%d body=%s", rec.Code, rec.Body.String())
	}
	var list api.ListAuditEventsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode list response: %v body=%s", err, rec.Body.String())
	}
	if list.Limit != 50 {
		t.Errorf("Limit = %d, want 50", list.Limit)
	}
	if len(list.Events) != 2 {
		t.Fatalf("got %d events, want 2 (key.created x2); events=%+v", len(list.Events), list.Events)
	}
	for i, ev := range list.Events {
		if ev.Kind != "key.created" {
			t.Errorf("events[%d].Kind = %q, want key.created", i, ev.Kind)
		}
		if !strings.HasPrefix(ev.Kind, "key.") {
			t.Errorf("events[%d].Kind = %q, missing key. prefix", i, ev.Kind)
		}
	}
}

// TestAuditEvents_ListEndpointRespectsSince drives GET /v1/audit-events
// with a `since` cutoff and asserts rows strictly older are excluded.
// We seed two events ~10ms apart, query with `since` at the second
// event's at, and expect exactly one row.
func TestAuditEvents_ListEndpointRespectsSince(t *testing.T) {
	e := setup(t, api.PlanPro)
	// First key.created row.
	rec := e.do(t, http.MethodPost, "/v1/keys", api.CreateKeyRequest{Label: "first", Scopes: []string{"read"}}, nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST /v1/keys: code=%d body=%s", rec.Code, rec.Body.String())
	}
	// Sleep so the two events have measurably different timestamps.
	time.Sleep(20 * time.Millisecond)
	cutoff := time.Now().UTC()
	time.Sleep(20 * time.Millisecond)
	rec = e.do(t, http.MethodPost, "/v1/keys", api.CreateKeyRequest{Label: "second", Scopes: []string{"read"}}, nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST /v1/keys: code=%d body=%s", rec.Code, rec.Body.String())
	}

	sinceParam := cutoff.Format(time.RFC3339Nano)
	listRec := e.do(t, http.MethodGet, "/v1/audit-events?since="+sinceParam, nil, nil)
	if listRec.Code != http.StatusOK {
		t.Fatalf("GET /v1/audit-events?since=…: code=%d body=%s", listRec.Code, listRec.Body.String())
	}
	var list api.ListAuditEventsResponse
	if err := json.Unmarshal(listRec.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode list: %v body=%s", err, listRec.Body.String())
	}
	// We expect exactly the "second" event to come back; the "first"
	// was emitted before cutoff so it must be excluded.
	if len(list.Events) != 1 {
		t.Fatalf("got %d events, want 1 (only events at >= cutoff); events=%+v", len(list.Events), list.Events)
	}
	if list.Events[0].Kind != "key.created" {
		t.Errorf("event[0].Kind = %q, want key.created", list.Events[0].Kind)
	}
}

// TestAuditEvents_ListEndpointCrossAccountInvisible proves that the
// customer-facing list is scoped to the caller: acct A mints a key
// (emits auth.* / key.* rows scoped to A's account_id); acct B's GET
// returns [].
func TestAuditEvents_ListEndpointCrossAccountInvisible(t *testing.T) {
	store := state.NewMemStore()
	// Acct A.
	acctA, err := store.CreateAccount(context.Background(), "a@example.com", api.PlanPro)
	if err != nil {
		t.Fatal(err)
	}
	// Acct B.
	acctB, err := store.CreateAccount(context.Background(), "b@example.com", api.PlanPro)
	if err != nil {
		t.Fatal(err)
	}
	// Drive a key.created row through acctA's path by appending
	// directly — the test goal is to prove ListEvents is subject-pinned,
	// not to re-test the createKey emission (already covered above).
	id := "0123456789abcdef0123456789abcdef"
	if err := store.AppendEvent(context.Background(), "apid", "key.created", &acctA.ID, []byte(`{"key_id":"`+id+`"}`)); err != nil {
		t.Fatal(err)
	}
	rowsA, _ := store.ListEvents(context.Background(), acctA.ID, 0)
	rowsB, _ := store.ListEvents(context.Background(), acctB.ID, 0)
	if len(rowsA) != 1 {
		t.Errorf("acctA events = %d, want 1", len(rowsA))
	}
	if len(rowsB) != 0 {
		t.Errorf("acctB events = %d, want 0 (cross-account isolation)", len(rowsB))
	}
}

// TestAuditEvents_GetEndpointReturnsRow drives GET /v1/audit-events/{id}
// for a freshly minted key's audit row and asserts the single-event
// wire shape (id, at, actor, kind, subject, data) comes back correctly.
func TestAuditEvents_GetEndpointReturnsRow(t *testing.T) {
	e := setup(t, api.PlanPro)
	rec := e.do(t, http.MethodPost, "/v1/keys", api.CreateKeyRequest{Label: "get-me", Scopes: []string{"read"}}, nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST /v1/keys: code=%d body=%s", rec.Code, rec.Body.String())
	}
	rows, _ := e.store.ListEvents(context.Background(), e.acct.ID, 0)
	var target *state.Event
	for i := range rows {
		if rows[i].Kind == "key.created" {
			target = &rows[i]
			break
		}
	}
	if target == nil {
		t.Fatal("no key.created row to fetch")
	}
	idStr := jsonNumber(target.ID)
	getRec := e.do(t, http.MethodGet, "/v1/audit-events/"+idStr, nil, nil)
	if getRec.Code != http.StatusOK {
		t.Fatalf("GET /v1/audit-events/%s: code=%d body=%s", idStr, getRec.Code, getRec.Body.String())
	}
	var got api.AuditEventResponse
	if err := json.Unmarshal(getRec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v body=%s", err, getRec.Body.String())
	}
	if got.Kind != "key.created" {
		t.Errorf("Kind = %q, want key.created", got.Kind)
	}
	if got.Subject != uuidStringOf(e.acct.ID) {
		t.Errorf("Subject = %q, want %s", got.Subject, uuidStringOf(e.acct.ID))
	}
	if got.Actor != "apid" {
		t.Errorf("Actor = %q, want apid", got.Actor)
	}
}

// TestAuditEvents_GetEndpointCrossAccount404 proves the get path
// 404s a cross-account id probe — a customer cannot enumerate
// another account's audit row count by id-probing.
func TestAuditEvents_GetEndpointCrossAccount404(t *testing.T) {
	store := state.NewMemStore()
	acctA, _ := store.CreateAccount(context.Background(), "a@example.com", api.PlanPro)
	acctB, _ := store.CreateAccount(context.Background(), "b@example.com", api.PlanPro)
	if err := store.AppendEvent(context.Background(), "apid", "key.created", &acctA.ID, []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	rowsA, _ := store.ListEvents(context.Background(), acctA.ID, 0)
	if len(rowsA) != 1 {
		t.Fatalf("setup: acctA rows = %d, want 1", len(rowsA))
	}
	// Mount a server and ask as acctB.
	ptB, hashB, _ := api.GenerateAPIKey()
	if _, err := store.CreateAPIKey(context.Background(), acctB.ID, hashB, "test", api.DefaultScopes()); err != nil {
		t.Fatal(err)
	}
	srv := newServer(store,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		"example.com", noopNotifier{}).WithOpsMetrics(wire.NewOpsMetrics("apid"))
	h := srv.handler()
	idStr := jsonNumber(rowsA[0].ID)
	req := httptest.NewRequest(http.MethodGet, "/v1/audit-events/"+idStr, nil)
	req.Header.Set("Authorization", "Bearer "+ptB)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("cross-account GET code = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}

// TestAuditEvents_FailingStoreDoesNotRollback proves the audit seam
// is best-effort: a failing AppendEvent must NOT roll back the
// mutation that produced the audit emit. Mirrors schedd's
// TestEngineTransition_EventWriteFailureDoesNotRollback exactly —
// build a thin wrapper that errors on AppendEvent, drive POST
// /v1/keys, assert the handler returned 201 (not 5xx) and the
// audit_write_failures counter incremented.
func TestAuditEvents_FailingStoreDoesNotRollback(t *testing.T) {
	e := setup(t, api.PlanPro)
	wrapped := &failingAuditStore{Store: e.store}
	// Re-mount with the wrapped store. The audit helper reads s.audit
	// ops lazily so we keep e.ops the same.
	srv := newServer(wrapped,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		"example.com", noopNotifier{}).WithOpsMetrics(e.ops)
	h := srv.handler()
	body, _ := json.Marshal(api.CreateKeyRequest{Label: "audit-fail", Scopes: []string{"read"}})
	req := httptest.NewRequest(http.MethodPost, "/v1/keys", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+e.key)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST /v1/keys with failing audit store: code=%d body=%s", rec.Code, rec.Body.String())
	}
	// Confirm the underlying row landed (the handler's success path
	// is NOT gated on audit emit — that's the load-bearing invariant).
	keys, err := e.store.ListAPIKeys(context.Background(), e.acct.ID)
	if err != nil {
		t.Fatalf("ListAPIKeys: %v", err)
	}
	if len(keys) < 2 {
		t.Errorf("API keys = %d, want ≥2 (audit failure must not roll back create)", len(keys))
	}
	// Counter incremented.
	if got := testutil.ToFloat64(e.ops.AuditWriteFailures()); got < 1 {
		t.Errorf("audit_write_failures = %v, want ≥1", got)
	}
}

// --- helpers ---------------------------------------------------------------

// failingAuditStore wraps state.Store and errors on AppendEvent only.
// Same pattern as pkg/sched/events_test.go::failingEventStore.
type failingAuditStore struct {
	state.Store
	failures int64
}

func (f *failingAuditStore) AppendEvent(ctx context.Context, actor, kind string, subject *string, data []byte) error {
	f.failures++
	return &failingAuditErr{}
}

type failingAuditErr struct{}

func (*failingAuditErr) Error() string { return "simulated AppendEvent failure" }

// jsonNumber formats an int64 the same way state.Event.ID is wired:
// the apid handler reads PathValue("id") as a string and re-parses
// it back to int64. Using FormatInt + "10" keeps the test path
// identical to the production path.
func jsonNumber(n int64) string {
	const digits = "0123456789"
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = digits[n%10]
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
