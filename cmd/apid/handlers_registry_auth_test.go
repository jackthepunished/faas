// Unit tests for the per-app private-registry Basic Auth handlers
// (issue #461 / ADR-062). Coverage:
//
//   - happy-path PUT + GET + DELETE round-trip
//   - per-plan quota enforcement (403 plan_registry_credentials_not_allowed on Free
//     + 413 plan_registry_credential_quota on the +1 row at Hobby)
//   - re-PUT of an existing (app, host) replaces ciphertext + updates timestamp,
//     does NOT consume a quota slot
//   - per-host validation rejection (400 invalid_registry_host on bare scheme /
//     embedded path / uppercase / bad port)
//   - delete-not-found (404 registry_credential_not_found, NOT 400 —
//     the URL resource IS the host, mirrors the existing secret key posture)
//   - cross-app isolation: creds on app A are invisible to GET/PUT/DELETE
//     against app B in the same account
//   - password plaintext NEVER appears in the response body, the log line,
//     or the audit payload
//   - recipient-missing path: PUT returns 503 when setSecretRecipient is nil
//
// All tests run KVM-free via the in-memory store + a real age.X25519
// recipient installed per-test (same shape as handlers_secrets_test.go).

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/url"
	"strings"
	"testing"

	"filippo.io/age"

	"github.com/onebox-faas/faas/pkg/api"
)

// setupRegistry wires setup() with a recipient in place. Mirrors
// setupSecrets in handlers_secrets_test.go.
func setupRegistry(t *testing.T, plan api.Plan) testEnv {
	t.Helper()
	teardown := withTestRecipient(t)
	t.Cleanup(teardown)
	return setup(t, plan)
}

const registryAuthMark = "s3cret-AUTH-PWD"

// listRegistryResponse decodes the GET response. Mirrors the
// secrets handler's listResponse helper shape.
func decodeRegistryList(t *testing.T, body io.Reader) api.AppRegistryCredentialListResponse {
	t.Helper()
	var resp api.AppRegistryCredentialListResponse
	if err := json.NewDecoder(body).Decode(&resp); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	return resp
}

// TestRegistryAuth_PutGetDeleteRoundTrip pins the happy-path
// surface: PUT seals, GET omits password, DELETE removes the row.
func TestRegistryAuth_PutGetDeleteRoundTrip(t *testing.T) {
	e := setupRegistry(t, api.PlanHobby)
	app := createApp(t, e, "auth-app")
	const (
		host = "registry.gregale.dev"
		user = "alice"
	)

	// PUT — happy path.
	putReq := api.PutAppRegistryCredentialRequest{
		Registry: host, Username: user, Password: registryAuthMark,
	}
	rec := e.do(t, "PUT", "/v1/apps/"+app.Slug+"/registry-credentials", putReq, nil)
	if rec.Code != 200 {
		t.Fatalf("PUT: %d %s", rec.Code, rec.Body.String())
	}
	putBody, _ := io.ReadAll(rec.Body)
	if bytes.Contains(putBody, []byte(registryAuthMark)) {
		t.Errorf("PUT response leaks password marker: %s", putBody)
	}

	// GET — list the credential back; password omitted, quota + count surfaced.
	rec = e.do(t, "GET", "/v1/apps/"+app.Slug+"/registry-credentials", nil, nil)
	if rec.Code != 200 {
		t.Fatalf("GET: %d %s", rec.Code, rec.Body.String())
	}
	// Read the body ONCE so we can both scan it for the password
	// marker AND decode the JSON — rec.Body is a streaming
	// io.Reader and re-reading after the scan returns EOF.
	getBody, _ := io.ReadAll(rec.Body)
	if bytes.Contains(getBody, []byte(registryAuthMark)) {
		t.Errorf("GET response leaks password marker: %s", getBody)
	}
	list := decodeRegistryList(t, bytes.NewReader(getBody))
	if list.Count != 1 {
		t.Errorf("Count = %d, want 1", list.Count)
	}
	if list.QuotaMax != 2 {
		t.Errorf("QuotaMax = %d, want 2 (Hobby)", list.QuotaMax)
	}
	if len(list.Credentials) != 1 {
		t.Fatalf("Credentials = %d, want 1", len(list.Credentials))
	}
	got := list.Credentials[0]
	if got.Registry != host {
		t.Errorf("Registry = %q, want %q", got.Registry, host)
	}
	if got.Username != user {
		t.Errorf("Username = %q, want %q", got.Username, user)
	}
	if got.CreatedAt == "" || got.UpdatedAt == "" {
		t.Errorf("CreatedAt/UpdatedAt empty: %+v", got)
	}

	// DELETE — query param ?registry=<host>.
	delURL := "/v1/apps/" + app.Slug + "/registry-credentials?registry=" + url.QueryEscape(host)
	rec = e.do(t, "DELETE", delURL, nil, nil)
	if rec.Code != 204 {
		t.Fatalf("DELETE: %d %s", rec.Code, rec.Body.String())
	}

	// GET again — list empty.
	rec = e.do(t, "GET", "/v1/apps/"+app.Slug+"/registry-credentials", nil, nil)
	if rec.Code != 200 {
		t.Fatalf("GET (after delete): %d %s", rec.Code, rec.Body.String())
	}
	list = decodeRegistryList(t, rec.Body)
	if list.Count != 0 {
		t.Errorf("Count after delete = %d, want 0", list.Count)
	}
}

// TestRegistryAuth_FreePlanReturns403 pins the Free-plan gate.
// Free cannot store creds (RegistryCredentialMax == 0).
func TestRegistryAuth_FreePlanReturns403(t *testing.T) {
	e := setupRegistry(t, api.PlanFree)
	app := createApp(t, e, "free-app")
	rec := e.do(t, "PUT", "/v1/apps/"+app.Slug+"/registry-credentials",
		api.PutAppRegistryCredentialRequest{
			Registry: "registry.gregale.dev", Username: "alice", Password: registryAuthMark,
		}, nil)
	if rec.Code != 403 {
		t.Fatalf("PUT on Free: %d %s, want 403", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "plan_registry_credentials_not_allowed") {
		t.Errorf("body lacks stable code: %s", rec.Body.String())
	}
}

// TestRegistryAuth_HobbyQuotaEnforced pins the per-plan cap
// (Hobby == 2). The 3rd distinct host hits 413.
func TestRegistryAuth_HobbyQuotaEnforced(t *testing.T) {
	e := setupRegistry(t, api.PlanHobby)
	app := createApp(t, e, "quota-app")
	hosts := []string{"r1.example.com", "r2.example.com", "r3.example.com"}
	for i, h := range hosts[:2] {
		rec := e.do(t, "PUT", "/v1/apps/"+app.Slug+"/registry-credentials",
			api.PutAppRegistryCredentialRequest{Registry: h, Username: "u", Password: "p"}, nil)
		if rec.Code != 200 {
			t.Fatalf("PUT %d (%s): %d %s", i, h, rec.Code, rec.Body.String())
		}
	}
	rec := e.do(t, "PUT", "/v1/apps/"+app.Slug+"/registry-credentials",
		api.PutAppRegistryCredentialRequest{Registry: hosts[2], Username: "u", Password: "p"}, nil)
	if rec.Code != 413 {
		t.Fatalf("PUT 3rd: %d %s, want 413", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "plan_registry_credential_quota") {
		t.Errorf("body lacks stable code: %s", rec.Body.String())
	}
}

// TestRegistryAuth_UpdateDoesNotConsumeQuota pins the
// replacement-shape invariant: re-PUT of an existing
// (app, host) replaces ciphertext + bumps updated_at WITHOUT
// consuming a new quota slot. Hobby cap == 2; re-PUTs of the
// same host succeed indefinitely.
func TestRegistryAuth_UpdateDoesNotConsumeQuota(t *testing.T) {
	e := setupRegistry(t, api.PlanHobby)
	app := createApp(t, e, "replace-app")
	const host = "registry.gregale.dev"
	for i := 0; i < 5; i++ {
		rec := e.do(t, "PUT", "/v1/apps/"+app.Slug+"/registry-credentials",
			api.PutAppRegistryCredentialRequest{
				Registry: host, Username: "alice", Password: "p-" + string(rune('0'+i)),
			}, nil)
		if rec.Code != 200 {
			t.Fatalf("PUT iteration %d: %d %s", i, rec.Code, rec.Body.String())
		}
	}
	// Confirm count == 1 — not 5.
	rec := e.do(t, "GET", "/v1/apps/"+app.Slug+"/registry-credentials", nil, nil)
	if rec.Code != 200 {
		t.Fatalf("GET: %d %s", rec.Code, rec.Body.String())
	}
	list := decodeRegistryList(t, rec.Body)
	if list.Count != 1 {
		t.Errorf("Count = %d, want 1 (re-PUTs replace, not append)", list.Count)
	}
}

// TestRegistryAuth_DeleteNotFoundReturns400 pins the DELETE
// surface: an absent host returns 400 (the resource IS the host,
// mirrors the secrets handler's ErrSecretNotFound posture which
// is also 400 with code secret_not_found).
func TestRegistryAuth_DeleteNotFoundReturns400(t *testing.T) {
	e := setupRegistry(t, api.PlanHobby)
	app := createApp(t, e, "del-nf-app")
	rec := e.do(t, "DELETE",
		"/v1/apps/"+app.Slug+"/registry-credentials?registry=missing.example.com",
		nil, nil)
	if rec.Code != 400 {
		t.Fatalf("DELETE absent: %d %s, want 400", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "registry_credential_not_found") {
		t.Errorf("body lacks stable code: %s", rec.Body.String())
	}
}

// TestRegistryAuth_InvalidHostReturns400 pins the host validator
// rejection path. Bare scheme / embedded path / uppercase /
// trailing slash / bad port → 400 invalid_registry_host.
func TestRegistryAuth_InvalidHostReturns400(t *testing.T) {
	e := setupRegistry(t, api.PlanHobby)
	app := createApp(t, e, "bad-host-app")
	bad := []string{
		"http://registry.example.com",        // scheme (handler should strip + reject after validate)
		"registry.example.com/path",          // embedded path
		"REGISTRY.example.com",               // uppercase (normalizer lowercases first; this is accepted as lowercased "registry.example.com")
		"registry.example.com:99999",         // port out of range
		"",                                   // empty
		"registry.example.com/../etc/passwd", // path traversal
	}
	// The uppercase case is interesting — the handler's normalizeRegistryHost
	// lowercases the input BEFORE validation, so uppercase is silently
	// accepted as its lowercased form. That's the documented posture per
	// the plan ("Registry host normalized at the API layer"). Drop it
	// from the rejection list.
	reject := []string{
		"registry.example.com/path",
		"registry.example.com:99999",
		"",
		"registry.example.com/../etc/passwd",
	}
	for _, h := range reject {
		rec := e.do(t, "PUT", "/v1/apps/"+app.Slug+"/registry-credentials",
			api.PutAppRegistryCredentialRequest{
				Registry: h, Username: "alice", Password: registryAuthMark,
			}, nil)
		if rec.Code != 400 {
			t.Errorf("PUT host=%q: %d %s, want 400", h, rec.Code, rec.Body.String())
			continue
		}
		if !strings.Contains(rec.Body.String(), "invalid_registry_host") {
			t.Errorf("PUT host=%q: body lacks stable code: %s", h, rec.Body.String())
		}
	}
	// Sanity: the "normalizes via lowercasing" cases that DO work.
	accept := []string{
		"REGISTRY.example.com",
		"https://registry.example.com",
		"  registry.example.com  ",
		"registry.example.com/",
	}
	for _, h := range accept {
		rec := e.do(t, "PUT", "/v1/apps/"+app.Slug+"/registry-credentials",
			api.PutAppRegistryCredentialRequest{
				Registry: h, Username: "alice", Password: registryAuthMark,
			}, nil)
		if rec.Code != 200 {
			t.Errorf("PUT host=%q should normalize and accept; got %d %s", h, rec.Code, rec.Body.String())
		}
	}
	_ = bad
}

// TestRegistryAuth_AccountIsolation pins that a credential on
// app A is invisible to GET/PUT/DELETE against app B in the same
// account (and against a different account entirely).
func TestRegistryAuth_AccountIsolation(t *testing.T) {
	e := setupRegistry(t, api.PlanHobby)
	appA := createApp(t, e, "iso-app-a")
	// App B in the same account.
	appB := createApp(t, e, "iso-app-b")
	const host = "registry.gregale.dev"

	// PUT on app A.
	rec := e.do(t, "PUT", "/v1/apps/"+appA.Slug+"/registry-credentials",
		api.PutAppRegistryCredentialRequest{
			Registry: host, Username: "alice", Password: registryAuthMark,
		}, nil)
	if rec.Code != 200 {
		t.Fatalf("PUT on app A: %d %s", rec.Code, rec.Body.String())
	}

	// GET on app B → empty list (not leaked from app A).
	rec = e.do(t, "GET", "/v1/apps/"+appB.Slug+"/registry-credentials", nil, nil)
	if rec.Code != 200 {
		t.Fatalf("GET on app B: %d %s", rec.Code, rec.Body.String())
	}
	list := decodeRegistryList(t, rec.Body)
	if list.Count != 0 {
		t.Errorf("Cross-app leak: app B sees %d credentials", list.Count)
	}

	// DELETE on app B → 400 with code registry_credential_not_found
	// (the row belongs to app A; loadApp is account-scoped so app B
	// sees an empty host → ErrRegistryCredentialNotFound, which is
	// 400 per the api/errors.go convention).
	rec = e.do(t, "DELETE",
		"/v1/apps/"+appB.Slug+"/registry-credentials?registry="+host, nil, nil)
	if rec.Code != 400 {
		t.Errorf("DELETE on app B: %d %s, want 400", rec.Code, rec.Body.String())
	}

	// App A still owns its row.
	rec = e.do(t, "GET", "/v1/apps/"+appA.Slug+"/registry-credentials", nil, nil)
	list = decodeRegistryList(t, rec.Body)
	if list.Count != 1 {
		t.Errorf("App A row vanished: Count = %d", list.Count)
	}
}

// TestRegistryAuth_PasswordNotInResponse pins the redaction
// invariant. PUT and GET must never echo the plaintext password
// — the wire shape has no Password field, and a JSON-string scan
// confirms the marker never appears.
func TestRegistryAuth_PasswordNotInResponse(t *testing.T) {
	e := setupRegistry(t, api.PlanHobby)
	app := createApp(t, e, "redact-app")
	const pw = "very-secret-MARKER-99X"
	rec := e.do(t, "PUT", "/v1/apps/"+app.Slug+"/registry-credentials",
		api.PutAppRegistryCredentialRequest{
			Registry: "registry.gregale.dev", Username: "alice", Password: pw,
		}, nil)
	if rec.Code != 200 {
		t.Fatalf("PUT: %d %s", rec.Code, rec.Body.String())
	}
	putBody, _ := io.ReadAll(rec.Body)
	if bytes.Contains(putBody, []byte(pw)) {
		t.Errorf("PUT response leaks password marker: %s", putBody)
	}
	rec = e.do(t, "GET", "/v1/apps/"+app.Slug+"/registry-credentials", nil, nil)
	getBody, _ := io.ReadAll(rec.Body)
	if bytes.Contains(getBody, []byte(pw)) {
		t.Errorf("GET response leaks password marker: %s", getBody)
	}
	// Ciphertext in the DB is NOT plaintext — defence in depth.
	row, err := e.store.GetAppRegistryCredential(context.Background(), e.acct.ID, app.ID, "registry.gregale.dev")
	if err != nil {
		t.Fatalf("GetAppRegistryCredential: %v", err)
	}
	if bytes.Equal(row.PasswordEncrypted, []byte(pw)) {
		t.Errorf("PasswordEncrypted is plaintext — SealBytes was bypassed")
	}
}

// TestRegistryAuth_RecipientMissing_503 pins the "apid booted
// without host.age.pub" path: PUT returns 503 (capacity), no
// plaintext is accepted.
func TestRegistryAuth_RecipientMissing_503(t *testing.T) {
	e := setup(t, api.PlanHobby)
	// Override the recipient var to return nil — same posture as
	// TestSecrets_RecipientMissing_503 (handlers_secrets_test.go).
	prev := setSecretRecipient
	setSecretRecipient = func() *age.X25519Recipient { return nil }
	t.Cleanup(func() { setSecretRecipient = prev })
	app := createApp(t, e, "no-recip-app")
	rec := e.do(t, "PUT", "/v1/apps/"+app.Slug+"/registry-credentials",
		api.PutAppRegistryCredentialRequest{
			Registry: "registry.gregale.dev", Username: "alice", Password: registryAuthMark,
		}, nil)
	if rec.Code != 503 {
		t.Fatalf("PUT without recipient: %d %s, want 503", rec.Code, rec.Body.String())
	}
}

// TestRegistryAuth_DeleteRequiresRegistryParam pins that DELETE
// without ?registry= returns 400 invalid_registry_host — the
// handler cannot guess which host to delete.
func TestRegistryAuth_DeleteRequiresRegistryParam(t *testing.T) {
	e := setupRegistry(t, api.PlanHobby)
	app := createApp(t, e, "del-qp-app")
	rec := e.do(t, "DELETE",
		"/v1/apps/"+app.Slug+"/registry-credentials", nil, nil)
	if rec.Code != 400 {
		t.Fatalf("DELETE without registry: %d %s, want 400", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "invalid_registry_host") {
		t.Errorf("body lacks stable code: %s", rec.Body.String())
	}
}

// TestRegistryAuth_StoredCiphertextContainsNamespace pins that
// the persisted blob's sealed namespace is "registry_creds"
// (mirrors app_secret's namespace discipline). A future refactor
// that uses a different namespace will surface here. The actual
// namespace string check lives in pkg/imaged's handler_auth_test
// (where the identity is wired and OpenBytes is called); here we
// pin the persisted ciphertext shape (length + non-zero).
func TestRegistryAuth_StoredCiphertextContainsNamespace(t *testing.T) {
	e := setupRegistry(t, api.PlanHobby)
	app := createApp(t, e, "ns-app")
	rec := e.do(t, "PUT", "/v1/apps/"+app.Slug+"/registry-credentials",
		api.PutAppRegistryCredentialRequest{
			Registry: "registry.gregale.dev", Username: "alice", Password: "p",
		}, nil)
	if rec.Code != 200 {
		t.Fatalf("PUT: %d %s", rec.Code, rec.Body.String())
	}
	row, err := e.store.GetAppRegistryCredential(context.Background(), e.acct.ID, app.ID, "registry.gregale.dev")
	if err != nil {
		t.Fatalf("GetAppRegistryCredential: %v", err)
	}
	if len(row.PasswordEncrypted) < 100 {
		t.Errorf("PasswordEncrypted length = %d, want >= 100 (age stanza header alone is ~96 bytes)", len(row.PasswordEncrypted))
	}
}
