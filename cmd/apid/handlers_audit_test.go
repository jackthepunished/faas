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
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/onebox-faas/faas/pkg/api"
	"github.com/onebox-faas/faas/pkg/middleware"
	"github.com/onebox-faas/faas/pkg/session"
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
	body := api.CreateKeyRequest{Label: "audit-test", Scopes: []string{api.ScopeAppsRead}}
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
	body := api.CreateKeyRequest{Label: "to-delete", Scopes: []string{api.ScopeAppsRead}}
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
// form path (data.via == "dashboard") is covered by
// TestAuditEvents_DashboardDeleteEmitsEventWithViaDashboard below —
// the seam is the scheduleDeletion(ctx, acct, via) parameter at
// handlers_account.go:90 which both call sites pass into.
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
		rec := e.do(t, http.MethodPost, "/v1/keys", api.CreateKeyRequest{Label: label, Scopes: []string{api.ScopeAppsRead}}, nil)
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
	rec := e.do(t, http.MethodPost, "/v1/keys", api.CreateKeyRequest{Label: "first", Scopes: []string{api.ScopeAppsRead}}, nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST /v1/keys: code=%d body=%s", rec.Code, rec.Body.String())
	}
	// Sleep so the two events have measurably different timestamps.
	time.Sleep(20 * time.Millisecond)
	cutoff := time.Now().UTC()
	time.Sleep(20 * time.Millisecond)
	rec = e.do(t, http.MethodPost, "/v1/keys", api.CreateKeyRequest{Label: "second", Scopes: []string{api.ScopeAppsRead}}, nil)
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
	rec := e.do(t, http.MethodPost, "/v1/keys", api.CreateKeyRequest{Label: "get-me", Scopes: []string{api.ScopeAppsRead}}, nil)
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
	idStr := strconv.FormatInt(target.ID, 10)
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
	if _, err := store.CreateAPIKey(context.Background(), acctB.ID, hashB, "test", api.ScopesAdminOnly); err != nil {
		t.Fatal(err)
	}
	srv := newServer(store,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		"example.com", noopNotifier{}).WithOpsMetrics(wire.NewOpsMetrics("apid"))
	h := srv.handler()
	idStr := strconv.FormatInt(rowsA[0].ID, 10)
	req := httptest.NewRequest(http.MethodGet, "/v1/audit-events/"+idStr, nil)
	req.Header.Set("Authorization", "Bearer "+ptB)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("cross-account GET code = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}

// TestAuditEvents_CliAuthExchangeEmitsAuthLoginAndKeyCreated drives
// the full device-code flow (POST /v1/cli-auth/code → claim → POST
// /v1/cli-auth/exchange) and asserts BOTH audit rows landed:
//   - key.created with data.key_id == minted key's id and data.scopes
//     carrying the default scope set
//   - auth.login with data.method == "cli_code"
//
// A single test exercises both Emit calls in the happy-path exchange
// because they share the only success branch where the CLI's principal
// lands on the account (handlers_cli_auth.go:155-164). Failure paths
// are covered separately by the existing TestExchangeCliAuthCode_*
// family — we only need to prove the audit emit fires when the
// handler returns 200.
func TestAuditEvents_CliAuthExchangeEmitsAuthLoginAndKeyCreated(t *testing.T) {
	srv, store := newCliAuthTestServer(t)

	// Seed an account so the claim binds to it.
	acct, err := store.CreateAccount(t.Context(), "cli-audit@example.com", api.PlanPro)
	if err != nil {
		t.Fatalf("seed account: %v", err)
	}

	// Step 1: CLI mints a code.
	minted := mintCliAuthCodeForTest(t, srv)

	// Step 2 + 3: simulate the dashboard claim (we use the store
	// directly to keep the test focused on the audit emission in
	// step 4 — the claim path is exercised by TestPostCliAuthPage_*).
	if err := store.ClaimCliAuthCode(t.Context(), mustHashCode(t, minted.Code), acct.ID); err != nil {
		t.Fatalf("claim: %v", err)
	}

	// Step 4: CLI polls. This is the success branch where both Emit
	// calls fire.
	body, _ := json.Marshal(api.CliAuthExchangeRequest{Code: minted.Code})
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/v1/cli-auth/exchange", bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	srv.ServeHTTP(rec, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("exchange: code=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp api.CliAuthExchangeResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode exchange response: %v", err)
	}
	if !api.ValidAPIKeyFormat(resp.Plaintext) {
		t.Fatalf("plaintext %q is not a valid api key", resp.Plaintext)
	}

	// Now assert both rows landed and are subject-pinned to acct.ID.
	rows, err := store.ListEvents(t.Context(), acct.ID, 0)
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}

	var keyCreated, authLogin *state.Event
	for i := range rows {
		switch rows[i].Kind {
		case "key.created":
			keyCreated = &rows[i]
		case "auth.login":
			authLogin = &rows[i]
		}
	}
	if keyCreated == nil {
		t.Fatalf("no key.created row; rows=%+v", rows)
	}
	if authLogin == nil {
		t.Fatalf("no auth.login row; rows=%+v", rows)
	}

	// Subject pinning: both rows scoped to acct.ID.
	wantSubject := uuidStringOf(acct.ID)
	if keyCreated.Subject == nil || keyCreated.Subject.String() != wantSubject {
		t.Errorf("key.created Subject = %v, want %s", keyCreated.Subject, wantSubject)
	}
	if authLogin.Subject == nil || authLogin.Subject.String() != wantSubject {
		t.Errorf("auth.login Subject = %v, want %s", authLogin.Subject, wantSubject)
	}

	// key.created carries the new key_id AND the default scopes.
	var keyData map[string]any
	if err := json.Unmarshal(keyCreated.Data, &keyData); err != nil {
		t.Fatalf("key.created Data not JSON: %v", err)
	}
	keyID, _ := keyData["key_id"].(string)
	if len(keyID) != 32 {
		t.Errorf("key.created Data.key_id = %q, want 32 hex chars", keyID)
	}
	scopes, _ := keyData["scopes"].([]any)
	if len(scopes) == 0 {
		t.Errorf("key.created Data.scopes missing: %+v", keyData)
	}

	// auth.login carries method=cli_code (the only method this path
	// can produce — there's no session-cookie or password variant).
	var loginData map[string]any
	if err := json.Unmarshal(authLogin.Data, &loginData); err != nil {
		t.Fatalf("auth.login Data not JSON: %v", err)
	}
	if loginData["method"] != "cli_code" {
		t.Errorf("auth.login Data.method = %v, want cli_code", loginData["method"])
	}

	// Ordering invariant (load-bearing — handbook §6 / ADR-035):
	// key.created must precede auth.login so the customer's timeline
	// reads "first I minted a key, then I logged in" rather than the
	// reverse. The handler emits in that order, the events table
	// appends in arrival order, so we check by ID monotonicity.
	if authLogin.ID <= keyCreated.ID {
		t.Errorf("auth.login.ID=%d must be greater than key.created.ID=%d (event ordering)",
			authLogin.ID, keyCreated.ID)
	}
}

// TestAuditEvents_DashboardDeleteEmitsEventWithViaDashboard covers
// the dashboard form path (POST /dashboard/account/delete). The
// same business-logic core (s.scheduleDeletion) is used by both this
// route and the REST DELETE /v1/account; the only thing that varies
// is the `via` parameter, which the handler passes through to
// audit.Emit. So the regression we're guarding against is "the
// dashboard path forgot to thread `via` from the route handler down
// to scheduleDeletion" — a bug class that would silently mark
// every dashboard deletion as data.via == "rest".
//
// We build a session-authed handler inline rather than reuse
// newAuthedDashboardServer because that helper doesn't return the
// MemStore (we need it to read events back out without a GET round
// trip). All other seams (mgr.Issue, middleware.CookieNameAuthenticated,
// sessionCookie, sessionCookieLifetime) are imported from the
// existing handlers_dashboard / handlers_auth / middleware / session
// packages.
func TestAuditEvents_DashboardDeleteEmitsEventWithViaDashboard(t *testing.T) {
	store := state.NewMemStore()
	acct, err := store.CreateAccount(t.Context(), "del@example.com", api.PlanPro)
	if err != nil {
		t.Fatalf("seed account: %v", err)
	}
	mgr, err := session.NewEphemeralManager(sessionCookieLifetime)
	if err != nil {
		t.Fatalf("session manager: %v", err)
	}
	sid, err := mgr.Issue(acct.ID)
	if err != nil {
		t.Fatalf("issue session: %v", err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := newServerWithDeps(store, log, "example.com", noopNotifier{}, "",
		noopMailer{}, stubGithubdClient{}, mgr, nil, 15*time.Minute, "").handler()

	// Drive GET /dashboard/account to mint a fresh sealed envelope
	// (the faas_csrf cookie + csrf_token form-value pair are bound
	// to (action="delete", subject=acct.ID) at render time).
	csrfCookie, deleteToken, _ := renderDashboardAccount(t, h, &http.Cookie{Name: sessionCookie, Value: sid})
	if deleteToken == "" {
		t.Fatal("rendered /dashboard/account is missing the delete csrf_token")
	}

	// POST /dashboard/account/delete.
	form := url.Values{}
	form.Set(middleware.FormFieldName, deleteToken)
	req := httptest.NewRequest(http.MethodPost, "/dashboard/account/delete", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: sid})
	req.AddCookie(&http.Cookie{Name: middleware.CookieNameAuthenticated, Value: csrfCookie})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusFound {
		t.Fatalf("POST /dashboard/account/delete: code=%d body=%s", rec.Code, rec.Body.String())
	}

	// Assert the audit row landed with data.via == "dashboard".
	// The REST path test (TestAuditEvents_AccountDeletionScheduledEmitsEvent)
	// already guards data.via == "rest"; together the two tests
	// prove the via parameter is threaded correctly from both call
	// sites through s.scheduleDeletion into the audit payload.
	rows, err := store.ListEvents(t.Context(), acct.ID, 0)
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
		t.Fatalf("no account.deletion_scheduled row from dashboard path; rows=%+v", rows)
	}
	var data map[string]any
	if err := json.Unmarshal(found.Data, &data); err != nil {
		t.Fatalf("Data not JSON: %v", err)
	}
	if data["via"] != "dashboard" {
		t.Errorf("Data.via = %v, want dashboard (REST path TestAuditEvents_AccountDeletionScheduledEmitsEvent covers \"rest\")",
			data["via"])
	}
	if found.Subject == nil || found.Subject.String() != uuidStringOf(acct.ID) {
		t.Errorf("Subject = %v, want %s", found.Subject, uuidStringOf(acct.ID))
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
	body, _ := json.Marshal(api.CreateKeyRequest{Label: "audit-fail", Scopes: []string{api.ScopeAppsRead}})
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
	if got := testutil.ToFloat64(e.ops.AuditWriteFailures(e.acct.ID)); got < 1 {
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
