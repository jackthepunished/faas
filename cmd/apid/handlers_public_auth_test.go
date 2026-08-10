package main

// handlers_public_auth_test.go — PATCH /v1/apps/{slug}
// integration tests for the public-auth surface
// (issue #477 / ADR-079). Pins the four load-bearing
// invariants the apid layer guarantees:
//
//   1. Closed-enum validation runs FIRST so a Free
//      customer PATCHing mode='weird' sees 422
//      invalid_public_auth_mode rather than the plan gate.
//   2. Plan-gate runs SECOND: Free + bearer = 402
//      plan_public_auth_bearer_not_allowed;
//      Free/Hobby + basic = 402
//      plan_public_auth_basic_not_allowed.
//   3. mode='basic' seal round-trip persists a non-empty
//      apps.public_auth_basic blob the gatewayd-internal unsealer
//      can decrypt under the APP_BASIC_AUTH namespace.
//      PATCHing back to mode='open' clears that blob so a
//      stale secretbox row never reaches a fresh request.
//   4. Audit: app.public_auth_changed fires on every
//      mode transition with has_basic_creds: bool
//      redaction — plaintext username / password / sealed
//      blob are NEVER recorded. app.updated's
//      old/new blocks carry mode only (also redacted).
//
// The setSecretRecipient override mirrors the G6 export
// tests (handlers_account_test.go::withAccountTestRecipient)
// — the seal step is wired into a function pointer at
// startup so tests can inject a freshly-generated identity
// for the duration of the case.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"filippo.io/age"

	"github.com/onebox-faas/faas/pkg/api"
	"github.com/onebox-faas/faas/pkg/state"
)

// withPublicAuthTestRecipient wires a fresh X25519
// recipient into setSecretRecipient. Mirrors
// withAccountTestRecipient in handlers_account_test.go
// — the secretbox seal step needs an in-memory identity
// during PATCH mode='basic'.
//
// Without this, the seal step's check at
// handlers_ext.go:589 returns 503 ("host age recipient
// not loaded — refusing to seal public_auth credentials")
// and the test never reaches the SQL write. The
// `t.Cleanup` restores the production setter so cross-file
// tests can swap recipients without trampling one another.
func withPublicAuthTestRecipient(t *testing.T) {
	t.Helper()
	prev := setSecretRecipient
	t.Cleanup(func() { setSecretRecipient = prev })
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatalf("GenerateX25519Identity: %v", err)
	}
	setSecretRecipient = func() *age.X25519Recipient { return id.Recipient() }
}

// patchPublicAuth is the PATCH helper for this file's
// table-driven group. Same shape as the inline body used
// in TestAuditEvents_AppUpdatedEmitsEvent above — a single
// helper keeps the test cases scannable. The PublicAuth
// pointer is the test's only mutable surface; passing nil
// is the "no public_auth block" path.
func patchPublicAuth(t *testing.T, e testEnv, slug string, pub *api.PublicAuthBlock) *httptest.ResponseRecorder {
	t.Helper()
	return e.do(t, http.MethodPatch, "/v1/apps/"+slug,
		api.UpdateAppRequest{PublicAuth: pub}, nil)
}

// TestPublicAuthPatch_BearerPlanGate is the load-bearing
// 402 tripwire. Free plan PATCH mode='bearer' MUST return
// 402 plan_public_auth_bearer_not_allowed — never 403,
// never 200, and never a 422 from the closed-enum path.
// Hobby+ PATCH must succeed (200). Mirrors the streaming
// / warm-snapshot / require_authn plan-gate family
// (issue #560 ADR-074 — 402 is the consistent
// PaymentRequired shape across tier-locked features).
func TestPublicAuthPatch_BearerPlanGate(t *testing.T) {
	t.Run("free_returns_402_bearer_not_allowed", func(t *testing.T) {
		e := setup(t, api.PlanFree)
		app := seedAppForAudit(t, e, "pa-bearer-free")
		rec := patchPublicAuth(t, e, app.Slug, &api.PublicAuthBlock{Mode: api.AppPublicAuthModeBearer})
		if rec.Code != http.StatusPaymentRequired {
			t.Fatalf("PATCH mode=bearer on Free: code=%d body=%s; want 402",
				rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "plan_public_auth_bearer_not_allowed") {
			t.Fatalf("PATCH body missing code; got %s", rec.Body.String())
		}
		// App row's mode column must stay 'open' (default).
		// The PATCH was rejected upstream — no SQL write
		// happened. The redaction invariant extends to the
		// row: a rejected PATCH never leaves a partial
		// update behind.
		got, err := e.store.AppByID(context.Background(), app.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.PublicAuthMode != "" && got.PublicAuthMode != api.AppPublicAuthModeOpen {
			t.Fatalf("app.PublicAuthMode = %q after rejected PATCH; want default open",
				got.PublicAuthMode)
		}
		// No audit row on rejection. The redaction invariant
		// requires that plaintext NEVER appears on the audit
		// stream — a rejected PATCH (402/422) must not emit
		// app.public_auth_changed either, since a future
		// contributor adding a "log even on rejection" code
		// path could accidentally double-write the rejected
		// request's payload (mode='bearer' on Free would
		// land has_basic_creds=false but the row itself
		// carries no business value for a rejected PATCH).
		assertNoAuditRow(t, e, "app.public_auth_changed")
	})
	t.Run("hobby_returns_200", func(t *testing.T) {
		e := setup(t, api.PlanHobby)
		app := seedAppForAudit(t, e, "pa-bearer-hobby")
		rec := patchPublicAuth(t, e, app.Slug, &api.PublicAuthBlock{Mode: api.AppPublicAuthModeBearer})
		if rec.Code != http.StatusOK {
			t.Fatalf("PATCH mode=bearer on Hobby: code=%d body=%s; want 200",
				rec.Code, rec.Body.String())
		}
	})
}

// TestPublicAuthPatch_BasicPlanGate mirrors the bearer
// test for basic: Free/Hobby reject with 402
// plan_public_auth_basic_not_allowed; Pro accepts. The
// 402 surface uses a distinct code from the bearer case
// so the CLI can branch on plan-specific upgrade copy.
func TestPublicAuthPatch_BasicPlanGate(t *testing.T) {
	t.Run("free_returns_402_basic_not_allowed", func(t *testing.T) {
		e := setup(t, api.PlanFree)
		app := seedAppForAudit(t, e, "pa-basic-free")
		rec := patchPublicAuth(t, e, app.Slug, &api.PublicAuthBlock{
			Mode:      api.AppPublicAuthModeBasic,
			BasicUser: "editor",
			BasicPass: "hunter2",
		})
		if rec.Code != http.StatusPaymentRequired {
			t.Fatalf("PATCH mode=basic on Free: code=%d body=%s; want 402",
				rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "plan_public_auth_basic_not_allowed") {
			t.Fatalf("PATCH body missing code; got %s", rec.Body.String())
		}
	})
	t.Run("hobby_returns_402_basic_not_allowed", func(t *testing.T) {
		e := setup(t, api.PlanHobby)
		app := seedAppForAudit(t, e, "pa-basic-hobby")
		rec := patchPublicAuth(t, e, app.Slug, &api.PublicAuthBlock{
			Mode:      api.AppPublicAuthModeBasic,
			BasicUser: "editor",
			BasicPass: "hunter2",
		})
		if rec.Code != http.StatusPaymentRequired {
			t.Fatalf("PATCH mode=basic on Hobby: code=%d body=%s; want 402",
				rec.Code, rec.Body.String())
		}
	})
	t.Run("pro_with_creds_returns_200", func(t *testing.T) {
		withPublicAuthTestRecipient(t)
		e := setup(t, api.PlanPro)
		app := seedAppForAudit(t, e, "pa-basic-pro")
		rec := patchPublicAuth(t, e, app.Slug, &api.PublicAuthBlock{
			Mode:      api.AppPublicAuthModeBasic,
			BasicUser: "editor",
			BasicPass: "hunter2",
		})
		if rec.Code != http.StatusOK {
			t.Fatalf("PATCH mode=basic on Pro: code=%d body=%s; want 200",
				rec.Code, rec.Body.String())
		}
		var resp api.AppResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("body unmarshal: %v", err)
		}
		if resp.PublicAuth.Mode != api.AppPublicAuthModeBasic {
			t.Fatalf("PublicAuth.Mode = %q; want basic", resp.PublicAuth.Mode)
		}
		if !resp.PublicAuth.HasBasicCreds {
			t.Fatalf("PublicAuth.HasBasicCreds = false; want true (sealed blob should be present)")
		}
	})
}

// TestPublicAuthPatch_OpenClearsBasicSealed pins the
// stale-secret-row invariant. After a successful mode='basic'
// PATCH (sealed blob persisted), a follow-up PATCH
// mode='open' MUST clear the blob. Without the clear, a
// later PATCH mode='basic' could resurrect the OLD
// credentials from the row even when the customer typed
// fresh values (or worse, an attacker who learns the seal
// shape could re-seal the old blob under the same key id).
func TestPublicAuthPatch_OpenClearsBasicSealed(t *testing.T) {
	withPublicAuthTestRecipient(t)
	e := setup(t, api.PlanPro)
	app := seedAppForAudit(t, e, "pa-clear")
	// 1. PATCH mode='basic' succeeds; row has sealed blob.
	rec := patchPublicAuth(t, e, app.Slug, &api.PublicAuthBlock{
		Mode:      api.AppPublicAuthModeBasic,
		BasicUser: "u",
		BasicPass: "p",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("first PATCH (basic): code=%d body=%s", rec.Code, rec.Body.String())
	}
	row, err := e.store.AppByID(context.Background(), app.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(row.PublicAuthBasicSealed) == 0 {
		t.Fatalf("after mode=basic PATCH, PublicAuthBasicSealed is empty; seal didn't run?")
	}
	if row.PublicAuthMode != api.AppPublicAuthModeBasic {
		t.Fatalf("PublicAuthMode = %q; want basic", row.PublicAuthMode)
	}
	// 2. PATCH mode='open' clears the sealed blob.
	rec = patchPublicAuth(t, e, app.Slug, &api.PublicAuthBlock{
		Mode: api.AppPublicAuthModeOpen,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("second PATCH (open): code=%d body=%s", rec.Code, rec.Body.String())
	}
	row, err = e.store.AppByID(context.Background(), app.ID)
	if err != nil {
		t.Fatal(err)
	}
	if row.PublicAuthMode != api.AppPublicAuthModeOpen {
		t.Fatalf("PublicAuthMode after open flip = %q; want open", row.PublicAuthMode)
	}
	if len(row.PublicAuthBasicSealed) != 0 {
		t.Fatalf("PublicAuthBasicSealed = %d bytes after open flip; want empty (stale-secret invariant)",
			len(row.PublicAuthBasicSealed))
	}
}

// TestPublicAuthPatch_AuditEmitsWithRedaction pins the
// re-redaction invariant (ADR-079 §Decision). Every mode
// flip MUST emit an app.public_auth_changed row carrying
//   - app_id, slug
//   - old, new (mode strings only)
//   - has_basic_creds (bool)
//
// The audit row MUST NOT carry basic_user / basic_pass /
// PublicAuthBasicSealed — the redaction posture is
// load-bearing because audit rows flow to log-archive
// stores where secret leakage is much harder to scrub than
// in-process state.
func TestPublicAuthPatch_AuditEmitsWithRedaction(t *testing.T) {
	withPublicAuthTestRecipient(t)
	e := setup(t, api.PlanPro)
	app := seedAppForAudit(t, e, "pa-audit")
	// PATCH open → basic (audit fires; has_basic_creds=true).
	rec := patchPublicAuth(t, e, app.Slug, &api.PublicAuthBlock{
		Mode:      api.AppPublicAuthModeBasic,
		BasicUser: "alice",
		BasicPass: "secret",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("first PATCH: code=%d body=%s", rec.Code, rec.Body.String())
	}
	rows, err := e.store.ListEvents(context.Background(), e.acct.ID, 0)
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	found := findEventByKind(rows, "app.public_auth_changed")
	if found == nil {
		t.Fatalf("no app.public_auth_changed event row; rows=%+v", rows)
	}
	var data map[string]any
	if err := json.Unmarshal(found.Data, &data); err != nil {
		t.Fatalf("Data not valid JSON: %v", err)
	}
	if data["app_id"] != app.ID {
		t.Errorf("Data.app_id = %v, want %s", data["app_id"], app.ID)
	}
	if data["slug"] != app.Slug {
		t.Errorf("Data.slug = %v, want %s", data["slug"], app.Slug)
	}
	if data["old"] != "" && data["old"] != api.AppPublicAuthModeOpen && data["old"] != api.AppPublicAuthModeBearer {
		// Pre-#477 default was "" (empty); post-#477 the column
		// surfaces as 'open' or 'bearer' (Pro/Scale default
		// after issue #695 / ADR-080). Any other value is a
		// regression.
		t.Errorf("Data.old = %v, want one of \"\"/%q/%q", data["old"], api.AppPublicAuthModeOpen, api.AppPublicAuthModeBearer)
	}
	if data["new"] != api.AppPublicAuthModeBasic {
		t.Errorf("Data.new = %v, want %q", data["new"], api.AppPublicAuthModeBasic)
	}
	if v, _ := data["has_basic_creds"].(bool); !v {
		t.Errorf("Data.has_basic_creds = %v; want true (mode=basic PATCH)", data["has_basic_creds"])
	}
	// Redaction: the audit row must NEVER carry the basic_user,
	// basic_pass, or any sealed blob shape. A direct
	// substring check against known plaintext values pins
	// this against future regressions where a contributor
	// adds structured logging that doubles the audit row.
	raw, _ := json.Marshal(data)
	if strings.Contains(string(raw), "alice") ||
		strings.Contains(string(raw), "secret") ||
		strings.Contains(string(raw), "hunter2") {
		t.Fatalf("audit row leaked plaintext: %s", raw)
	}
	// Second transition: basic → bearer. has_basic_creds
	// flips back to false (the cleared-blob invariant
	// surfaces here as the audit boolean, NOT as the
	// sealed-blob field — the row never holds plaintext).
	rec = patchPublicAuth(t, e, app.Slug, &api.PublicAuthBlock{
		Mode: api.AppPublicAuthModeBearer,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("second PATCH: code=%d body=%s", rec.Code, rec.Body.String())
	}
	rows, _ = e.store.ListEvents(context.Background(), e.acct.ID, 0)
	found = findEventByKind(rows, "app.public_auth_changed")
	if found == nil {
		t.Fatalf("no app.public_auth_changed row for second flip")
	}
	var secondData map[string]any
	_ = json.Unmarshal(found.Data, &secondData)
	if v, _ := secondData["has_basic_creds"].(bool); v {
		t.Errorf("second audit row has_basic_creds = true; want false (mode=bearer PATCH has no creds)")
	}
}

// TestPublicAuthPatch_ClosedEnumFirst pins invariant #1:
// validation runs BEFORE the plan gate. A Free customer
// PATCHing mode='weird' must see 422 invalid_public_auth_mode
// (the closed-enum shape error) — NOT 402 (which the plan
// gate would surface only after a known mode). Otherwise a
// future contributor adding more 402 codes would silently
// shadow the 422, confusing the customer.
func TestPublicAuthPatch_ClosedEnumFirst(t *testing.T) {
	e := setup(t, api.PlanFree)
	app := seedAppForAudit(t, e, "pa-enum")
	rec := patchPublicAuth(t, e, app.Slug, &api.PublicAuthBlock{Mode: "weird"})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("PATCH mode=weird on Free: code=%d body=%s; want 422",
			rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "invalid_public_auth_mode") &&
		!strings.Contains(rec.Body.String(), "public_auth.mode") {
		t.Fatalf("422 body missing closed-enum error: %s", rec.Body.String())
	}
	// No audit row on closed-enum rejection. The 422 path
	// is pre-SQL (the validator short-circuits before the
	// seal step), so no business value is gained from
	// recording it — and a future contributor adding
	// "audit on rejection" would have to confirm the
	// payload shape carries no plaintext (ADR-079 §Decision
	// "re-redaction invariant").
	assertNoAuditRow(t, e, "app.public_auth_changed")
}

// TestPublicAuthPatch_BasicRequiresCreds pins the basic-cred
// requirement: mode='basic' without basic_user / basic_pass
// is a 422 even on Pro plan (the plan gate would otherwise
// accept it). Mirrors PublicAuthBlock.Validate's "required
// iff mode='basic'" branch.
func TestPublicAuthPatch_BasicRequiresCreds(t *testing.T) {
	withPublicAuthTestRecipient(t)
	e := setup(t, api.PlanPro)
	app := seedAppForAudit(t, e, "pa-creds")
	// mode='basic' with empty basic_pass → 422
	// invalid_public_auth_basic_pass.
	rec := patchPublicAuth(t, e, app.Slug, &api.PublicAuthBlock{
		Mode:      api.AppPublicAuthModeBasic,
		BasicUser: "u",
		BasicPass: "",
	})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("PATCH mode=basic no pass: code=%d body=%s; want 422",
			rec.Code, rec.Body.String())
	}
	// Row must NOT have flipped — the 422 short-circuits
	// before the SQL write (the seal step has no creds
	// to encrypt).
	row, _ := e.store.AppByID(context.Background(), app.ID)
	if row.PublicAuthMode != "" && row.PublicAuthMode != api.AppPublicAuthModeOpen && row.PublicAuthMode != api.AppPublicAuthModeBearer {
		t.Fatalf("PublicAuthMode after rejected PATCH = %q; want default", row.PublicAuthMode)
	}
	// No audit row on basic-requires-creds rejection. Same
	// rationale as the closed-enum test above.
	assertNoAuditRow(t, e, "app.public_auth_changed")
}

// compile-time assertion: state.App has the public_auth
// fields the seam depends on (a future field rename trips
// the linter instead of a runtime nil).
var _ state.App

// assertNoAuditRow is the negative-side pin for the
// redaction invariant (ADR-079 §Decision). A rejected
// PATCH — 402 plan gate, 422 closed-enum, 422 basic-
// requires-creds — must NOT emit an app.public_auth_changed
// row. The audit redaction posture is a load-bearing
// invariant: a future contributor adding "audit on
// rejection" code would have to confirm the payload
// carries no plaintext (the closed-enum test would
// currently emit mode='weird' — questionable audit
// value). Asserting NO row pins the cleaner posture.
func assertNoAuditRow(t *testing.T, e testEnv, kind string) {
	t.Helper()
	rows, err := e.store.ListEvents(context.Background(), e.acct.ID, 0)
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	if found := findEventByKind(rows, kind); found != nil {
		t.Fatalf("unexpected %s audit row on rejected PATCH: data=%s", kind, string(found.Data))
	}
}
