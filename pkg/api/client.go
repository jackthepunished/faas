// Package api is the one-box FaaS platform's wire contract. It holds:
//   - DTOs for every v1 REST request/response (this file + dto.go)
//   - RFC 7807 Problem envelope and error constructors (errors.go)
//   - The typed Go SDK clients use against apid (Client below)
//
// Client is the public SDK surface. New customers should:
//
//	c := api.NewClient("https://api.example.com", os.Getenv("FAAS_TOKEN"))
//	app, err := c.GetApp(ctx, "hello-world")
//	apps, err := c.ListApps(ctx)
//
// All methods are safe for concurrent use; the underlying HTTP
// transport is shared and the only mutable state is via the per-call
// context. Conventions:
//
//   - Auth — every method sends Authorization: Bearer <token> when the
//     Client was constructed with a non-empty token. Tokenless clients
//     are useful for the anonymous device-code flow only (MintCliAuthCode,
//     ExchangeCliAuthCode).
//
//   - Idempotency — non-GET/HEAD calls auto-mint an Idempotency-Key
//     header (UUIDv4) on the way out when the caller didn't supply one.
//     The server's replay middleware (apid/server.go::idempotent) keeps
//     responses for 24h; SDK callers who want deterministic retry
//     semantics should pass their own key. DeleteAccount accepts an
//     explicit key argument for this reason.
//
//   - Errors — every 4xx/5xx with a Problem-shaped body returns an
//     *APIError wrapping the canonical Problem. Bodies that fail JSON
//     decoding fall through to errors.New("API error: <status>") so
//     non-problem responses (e.g. the authlimiter's plain-text 429)
//     still surface.
//
//   - Timeouts — the default HTTP client has a 30s timeout. SSE
//     streams and tarball uploads use dedicated transports; see
//     NewClientWithDeployTimeout and the *SSE methods (added in
//     commit 2).
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// Client is a typed wrapper over the v1 REST API. Construct with
// NewClient (30s default timeout) or NewClientWithDeployTimeout
// (longer upload timeout). Pass-through to net/http for SSE streams is
// configured internally; see logs.go.
//
// Path and query parameters are passed verbatim to net/http. The
// OpenAPI spec constrains every path param to a regex that excludes
// URL-unsafe characters (slug = ^[a-z0-9-]+$, id = ^[a-f0-9]{32}$,
// key = ^[A-Z][A-Z0-9_]*$, domain = ^[a-z0-9.\-]+$); apid validates
// input with these patterns, so malformed input surfaces as a 4xx
// Problem rather than a URL-mangled 404. SDK callers that compose
// slugs from user input should validate against the spec pattern
// before calling.
type Client struct {
	baseURL string
	token   string

	http       *http.Client // 30s default — used for every JSON call
	deployHTTP *http.Client // optional, used by DeployMultipart
}

// NewClient builds a client for baseURL with the given bearer token.
// An empty token disables Authorization (useful for the anonymous
// device-code endpoints).
func NewClient(baseURL, token string) *Client {
	return &Client{
		baseURL: baseURL,
		token:   token,
		http:    &http.Client{Timeout: 30 * time.Second},
	}
}

// NewClientWithDeployTimeout is like NewClient but configures a
// longer upload HTTP client. A non-positive duration falls back to
// the 30s default. Used by SDK consumers uploading multi-MB tarballs
// where the 30s default would otherwise trip.
func NewClientWithDeployTimeout(baseURL, token string, deployTimeout time.Duration) *Client {
	c := NewClient(baseURL, token)
	if deployTimeout > 0 {
		c.deployHTTP = &http.Client{Timeout: deployTimeout}
	}
	return c
}

// HTTPClient returns the underlying JSON HTTP client. Exposed so SDK
// callers can swap transport-level knobs (TLS, retries) without
// depending on a private field.
func (c *Client) HTTPClient() *http.Client { return c.http }

// BaseURL returns the URL prefix the client was constructed with.
func (c *Client) BaseURL() string { return c.baseURL }

// Token returns the bearer token the client was constructed with
// (empty for anonymous clients). The returned value is the raw
// secret; do NOT log it, surface it in errors, or persist it. SDK
// callers that need to forward the token to other surfaces should
// copy it into a local variable scoped to the request.
func (c *Client) Token() string { return c.token }

// uploadHTTP returns the upload client or falls back to the default.
func (c *Client) uploadHTTP() *http.Client {
	if c.deployHTTP != nil {
		return c.deployHTTP
	}
	return c.http
}

// do executes an HTTP request against c.baseURL+path with the SDK's
// standard auth + idempotency conventions. It marshals body as JSON
// when body != nil, decodes non-2xx as Problem, and unmarshals a
// successful response into out when out != nil.
func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("marshal request: %w", err)
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, r)
	if err != nil {
		return err
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	// UX §3.2 / impl §4.2: every mutating call carries Idempotency-Key
	// so a retried deploy/park/wake/rollback/etc. never double-charges
	// or double-creates. We never override an explicit key the caller
	// already set.
	if method != http.MethodGet && method != http.MethodHead && req.Header.Get("Idempotency-Key") == "" {
		req.Header.Set("Idempotency-Key", newUUIDv4())
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return c.doReq(c.http, req, out)
}

// doReq executes a prepared request against the given *http.Client
// (default c.http or uploadHTTP for tarball uploads) and applies the
// SDK's standard response handling: 4 MiB body cap, non-2xx → Problem,
// 2xx → unmarshal into out when out != nil. The caller is responsible
// for auth + Idempotency-Key + Content-Type — see do for the standard
// recipe; methods that need a custom header set it on req before
// calling doReq.
func (c *Client) doReq(cli *http.Client, req *http.Request, out any) error {
	resp, err := cli.Do(req)
	if err != nil {
		return fmt.Errorf("could not reach the API: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	data, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode >= 300 {
		var p Problem
		if json.Unmarshal(data, &p) == nil && p.Code != "" {
			return &APIError{Problem: p}
		}
		return fmt.Errorf("API error: %s", resp.Status)
	}
	if out != nil && len(data) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("decode response: %w", err)
		}
	}
	return nil
}

// doBytes executes an HTTP request and returns the raw response body
// verbatim (issue #299, used by GetBuildsIdSbom for the CycloneDX
// JSON document the server streams back). Mirrors do for auth +
// idempotency conventions but skips the JSON unmarshal — the
// caller passes a *[]byte and receives the body untouched. Returns
// (nil, *APIError) on a non-2xx, same shape as do.
func (c *Client) doBytes(ctx context.Context, method, path string, body, out any) error {
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("marshal request: %w", err)
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, r)
	if err != nil {
		return err
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	if method != http.MethodGet && method != http.MethodHead && req.Header.Get("Idempotency-Key") == "" {
		req.Header.Set("Idempotency-Key", newUUIDv4())
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("could not reach the API: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode >= 300 {
		var p Problem
		if json.Unmarshal(data, &p) == nil && p.Code != "" {
			return &APIError{Problem: p}
		}
		return fmt.Errorf("API error: %s", resp.Status)
	}
	if out != nil {
		if bp, ok := out.(*[]byte); ok {
			*bp = data
			return nil
		}
		// Fall through: caller wants JSON-decoded, do the same
		// unmarshal as doReq. Untyped callers will get a decode
		// error if they pass anything other than *[]byte.
		if len(data) > 0 {
			if err := json.Unmarshal(data, out); err != nil {
				return fmt.Errorf("decode response: %w", err)
			}
		}
	}
	return nil
}

// ErrNoBody is returned by helpers that expected a body but got none.
// Errors.Is/As users can match it directly; it's also wrapped inside
// *APIError.Problem paths so callers don't need to import errors.
var ErrNoBody = errors.New("api: response body was empty")

// Whoami returns the authenticated account.
func (c *Client) Whoami(ctx context.Context) (AccountResponse, error) {
	var out AccountResponse
	return out, c.do(ctx, "GET", "/v1/account", nil, &out)
}

// ExportAccount downloads the GDPR export bundle (spec §17 G6) into
// the provided writer. includeSecrets=false drops the ciphertext
// slice. The streamed body is decoded as a single JSON document for
// the SDK caller to inspect, so memory usage scales with bundle size.
func (c *Client) ExportAccount(ctx context.Context, includeSecrets bool) (AccountExportResponse, error) {
	path := "/v1/account/export"
	if !includeSecrets {
		path += "?include_secrets=false"
	}
	var out AccountExportResponse
	return out, c.do(ctx, "GET", path, nil, &out)
}

// DeleteAccount schedules the account for deletion. The server is
// idempotent under Idempotency-Key; callers may pass an explicit
// stable key (CI retries) or "" to auto-mint a UUIDv4 per call.
func (c *Client) DeleteAccount(ctx context.Context, idempotencyKey string) (AccountDeletionResponse, error) {
	if idempotencyKey == "" {
		idempotencyKey = newUUIDv4()
	}
	req, err := http.NewRequestWithContext(ctx, "DELETE", c.baseURL+"/v1/account", nil)
	if err != nil {
		return AccountDeletionResponse{}, err
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	req.Header.Set("Idempotency-Key", idempotencyKey)
	var out AccountDeletionResponse
	return out, c.doReq(c.http, req, &out)
}

// RestoreAccount cancels a pending deletion (spec §17 G6).
func (c *Client) RestoreAccount(ctx context.Context) (AccountResponse, error) {
	var out AccountResponse
	return out, c.do(ctx, "POST", "/v1/account/restore", nil, &out)
}

// MFA (IAM-2, issue #186) — TOTP second-factor on the dashboard.
// The server returns the otpauth URL + scratch codes ONCE on
// EnrollMFA; the customer must complete ConfirmMFA before the server
// stamps mfa_enrolled_at. VerifyMFA is the step-up route for an
// already-enrolled customer whose session cookie is mfa_pending.
// RecoverMFA burns a recovery code; DisableMFA clears MFA state
// (re-auth via password or recovery_code). All five require the
// session cookie — API keys bypass MFA per the IAM-2 decision.

// EnrollMFA starts enrollment. The plaintext TOTP secret + QR +
// 10 recovery codes are returned exactly once. The caller is
// responsible for rendering the QR + showing the codes to the
// customer — the server-side blob is sealed at rest under the
// host age key, and subsequent /enroll calls overwrite the
// secret without re-surfacing the plaintexts.
func (c *Client) PostAccountMfaEnroll(ctx context.Context) (MFAEnrollResponse, error) {
	var out MFAEnrollResponse
	return out, c.do(ctx, "POST", "/v1/account/mfa/enroll", MFAEnrollRequest{}, &out)
}

// ConfirmMFA finishes enrollment with the customer's first 6-digit
// TOTP code. On success the server stamps mfa_enrolled_at, clears
// mfa_required, and re-issues the session cookie without
// mfa_pending. Idempotent on retry.
func (c *Client) PostAccountMfaConfirm(ctx context.Context, req MFAConfirmRequest) (MFAConfirmResponse, error) {
	var out MFAConfirmResponse
	return out, c.do(ctx, "POST", "/v1/account/mfa/confirm", req, &out)
}

// VerifyMFA steps up an mfa_pending session for an already-enrolled
// customer. Does NOT re-stamp mfa_enrolled_at — only re-issues the
// session cookie without mfa_pending.
func (c *Client) PostAccountMfaVerify(ctx context.Context, req MFAVerifyRequest) (MFAVerifyResponse, error) {
	var out MFAVerifyResponse
	return out, c.do(ctx, "POST", "/v1/account/mfa/verify", req, &out)
}

// RecoverMFA burns a recovery code to regain access when the
// customer's TOTP device is lost. The matching hash is removed
// from the stored set; subsequent calls with the same code return
// 401. If the burn would consume the last code, the handler refuses
// and the caller should fall back to DisableMFA via password.
func (c *Client) PostAccountMfaRecover(ctx context.Context, req MFARecoverRequest) (MFARecoverResponse, error) {
	var out MFARecoverResponse
	return out, c.do(ctx, "POST", "/v1/account/mfa/recover", req, &out)
}

// DisableMFA opts out of MFA. The request body must include exactly
// one of Password or RecoveryCode — both empty and both set return
// 400 CodeValidation. On success the server clears
// mfa_secret_encrypted + mfa_recovery_codes_hash + mfa_enrolled_at;
// mfa_required is left as-is so the plan-upgrade / 2nd-deploy
// chokepoints can re-arm on the next trigger.
func (c *Client) PostAccountMfaDisable(ctx context.Context, req MFADisableRequest) (MFADisableResponse, error) {
	var out MFADisableResponse
	return out, c.do(ctx, "POST", "/v1/account/mfa/disable", req, &out)
}

// IAM-3 server-side session revocation (ADR-039, issue #187 + #244
// merged). The dashboard's "Active sessions" panel is driven by
// these four endpoints. All four require the session cookie —
// API keys bypass session tracking per the IAM-3 design decision
// (bearer keys never create or query the sessions table).
//
// Each method ignores 204 No Content; the SDK surfaces either a
// structured response (for List + RevokeAll) or just the error
// (for Logout + RevokeSession). The CLI's `faas logout` and
// `faas sessions` subcommands wrap these.

func (c *Client) PostAccountLogout(ctx context.Context) error {
	return c.do(ctx, "POST", "/v1/auth/logout", struct{}{}, nil)
}

func (c *Client) GetAccountSessions(ctx context.Context) (SessionListResponse, error) {
	var out SessionListResponse
	return out, c.do(ctx, "GET", "/v1/auth/sessions", nil, &out)
}

func (c *Client) DeleteAccountSession(ctx context.Context, id string) error {
	return c.do(ctx, "DELETE", "/v1/auth/sessions/"+id, SessionsRevokeRequest{}, nil)
}

func (c *Client) PostAccountSessionsRevokeAll(ctx context.Context) (SessionsRevokeAllResponse, error) {
	var out SessionsRevokeAllResponse
	return out, c.do(ctx, "POST", "/v1/auth/sessions/revoke_all", struct{}{}, &out)
}

// ListApps returns the account's apps.
func (c *Client) ListApps(ctx context.Context) ([]AppResponse, error) {
	var out []AppResponse
	return out, c.do(ctx, "GET", "/v1/apps", nil, &out)
}

// CreateApp creates an app.
func (c *Client) CreateApp(ctx context.Context, req CreateAppRequest) (AppResponse, error) {
	var out AppResponse
	return out, c.do(ctx, "POST", "/v1/apps", req, &out)
}

// Deploy creates a deployment for an app slug (JSON variant).
// For tarball / dockerfile deploys use DeployMultipart.
func (c *Client) Deploy(ctx context.Context, slug string, req CreateDeploymentRequest) (DeploymentResponse, error) {
	var out DeploymentResponse
	return out, c.do(ctx, "POST", "/v1/apps/"+slug+"/deployments", req, &out)
}

// GetDeployment returns a deployment by ID.
func (c *Client) GetDeployment(ctx context.Context, id string) (DeploymentResponse, error) {
	var out DeploymentResponse
	return out, c.do(ctx, "GET", "/v1/deployments/"+id, nil, &out)
}

// GetBuildsIdProvenance returns the ADR-038 build_provenance row for
// a build id. Backs the `faas build provenance <id>` CLI command.
// The backend surfaces a missing row as a 404 with code
// build_provenance_not_found, which the SDK propagates as a
// *APIError — callers should check against apierr.Code() when the
// distinction matters (vs. a hard 404 "no such build").
//
// Method name: the sdk-coverage drift gate
// (cmd/sdk-coverage/main.go::deriveMethodName) auto-derives
// "Get<PathSegments>" from the route; for `GET /v1/builds/{id}/provenance`
// the natural form is `GetBuildsIdProvenance`. Renaming here is
// cheaper than pinning a methodRouteMap row that would diverge from
// every other /v1/{resource}/{id} SDK shape.
func (c *Client) GetBuildsIdProvenance(ctx context.Context, id string) (BuildProvenanceResponse, error) {
	var out BuildProvenanceResponse
	return out, c.do(ctx, "GET", "/v1/builds/"+id+"/provenance", nil, &out)
}

// GetBuildsIdSbom returns the CycloneDX SBOM for a build id
// (issue #299, ADR-038 Phase 3). Backs the `faas build sbom <id>`
// CLI command. The body is the raw CycloneDX 1.5 JSON the imaged
// populator wrote into storage at build-completion time; the SDK
// returns it as []byte (not a typed struct) so callers can hand it
// straight to `cyclonedx-cli validate` or to a dashboard renderer
// without an intermediate decode.
//
// Returns (nil, *APIError) when no SBOM exists for the build id —
// Phase-3 populator hasn't landed yet, the build predates the
// schema column, or the storage backend lost the artifact. The
// caller (CLI) surfaces this as a "no SBOM" hint.
func (c *Client) GetBuildsIdSbom(ctx context.Context, id string) ([]byte, error) {
	var out []byte
	return out, c.doBytes(ctx, "GET", "/v1/builds/"+id+"/sbom", nil, &out)
}

// DeployMultipart ships a source tarball (with optional runtime +
// handler) to the multipart deploy endpoint. sourceName is the form
// filename apid sees in the multipart "source" part; pass the
// basename of the customer's file. source must implement io.Reader
// (e.g. *os.File, *bytes.Buffer). The caller is responsible for any
// pre-open security validation the surface requires — the SDK makes
// no assumptions about the file backend.
//
// For zero-knowledge of a customer file's provenance (the CLI's
// `faas deploy --tarball` refuses symlinks via openCustomerFile),
// wrap openCustomerFile before calling DeployMultipart.
func (c *Client) DeployMultipart(ctx context.Context, slug string, source io.Reader, sourceName, runtime, handler string, dockerfile bool) (DeploymentResponse, error) {
	var b bytes.Buffer
	w := newMultipartWriter(&b, slug, dockerfile, runtime, handler)
	fw, err := w.CreateFormFile("source", sourceName)
	if err != nil {
		return DeploymentResponse{}, fmt.Errorf("create form file: %w", err)
	}
	if _, err := io.Copy(fw, source); err != nil {
		return DeploymentResponse{}, fmt.Errorf("copy source: %w", err)
	}
	if err := w.Close(); err != nil {
		return DeploymentResponse{}, fmt.Errorf("close multipart writer: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", c.baseURL+"/v1/apps/"+slug+"/deployments", &b)
	if err != nil {
		return DeploymentResponse{}, err
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	req.Header.Set("Content-Type", w.FormDataContentType())
	// DeployMultipart bypasses Client.do (multipart Content-Type wins
	// over the JSON default) and routes through the longer-timeout
	// upload client. Auto-mint Idempotency-Key here so retry-safe
	// semantics still hold; the file-open guard (if any) runs at the
	// caller before this mint, so a rejected path never produces an
	// Idempotency-Key on the wire.
	req.Header.Set("Idempotency-Key", newUUIDv4())
	var out DeploymentResponse
	return out, c.doReq(c.uploadHTTP(), req, &out)
}

// GetApp returns the app metadata for a slug.
func (c *Client) GetApp(ctx context.Context, slug string) (AppResponse, error) {
	var out AppResponse
	return out, c.do(ctx, "GET", "/v1/apps/"+slug, nil, &out)
}

// UpdateApp applies a partial update to an app.
func (c *Client) UpdateApp(ctx context.Context, slug string, req UpdateAppRequest) (AppResponse, error) {
	var out AppResponse
	return out, c.do(ctx, "PATCH", "/v1/apps/"+slug, req, &out)
}

// RenameApp swaps an app's slug atomically (issue #63).
func (c *Client) RenameApp(ctx context.Context, oldSlug, newSlug string) (AppResponse, error) {
	var out AppResponse
	return out, c.do(ctx, "POST", "/v1/apps/"+oldSlug+"/rename",
		RenameAppRequest{NewSlug: newSlug}, &out)
}

// DeleteApp removes an app.
func (c *Client) DeleteApp(ctx context.Context, slug string) error {
	return c.do(ctx, "DELETE", "/v1/apps/"+slug, nil, nil)
}

// ChangePlan changes the account's subscription tier.
func (c *Client) ChangePlan(ctx context.Context, plan string) (AccountResponse, error) {
	var out AccountResponse
	return out, c.do(ctx, "PATCH", "/v1/account/plan",
		map[string]string{"plan": plan}, &out)
}

// GetStatusSLO fetches the public SLO snapshot.
func (c *Client) GetStatusSLO(ctx context.Context) (StatusPage, error) {
	var out StatusPage
	return out, c.do(ctx, "GET", "/status/slo.json", nil, &out)
}

// Rollback re-promotes the most recent superseded deployment.
func (c *Client) Rollback(ctx context.Context, slug string) (DeploymentResponse, error) {
	var out DeploymentResponse
	return out, c.do(ctx, "POST", "/v1/apps/"+slug+"/rollback", nil, &out)
}

// Park and Wake toggle the app between cold-parked and live.
func (c *Client) Park(ctx context.Context, slug string) error {
	return c.do(ctx, "POST", "/v1/apps/"+slug+"/park", nil, nil)
}
func (c *Client) Wake(ctx context.Context, slug string) error {
	return c.do(ctx, "POST", "/v1/apps/"+slug+"/wake", nil, nil)
}
func (c *Client) ListInstances(ctx context.Context, slug string) ([]InstanceResponse, error) {
	var out []InstanceResponse
	return out, c.do(ctx, "GET", "/v1/apps/"+slug+"/instances", nil, &out)
}

// GetInstances returns every live instance across the caller's account
// (issue #393). One call replaces N per-app ListInstances calls. The
// cursor / limit semantics mirror /v1/invoices: before is the last
// instance.id from a previous page, limit clamps to 1..100 (default 25).
// Cross-account isolation is a property of the SQL — the SDK doesn't
// need to scope the call. See ADR-045.
func (c *Client) GetInstances(ctx context.Context, before string, limit int) (ListInstancesResponse, error) {
	var out ListInstancesResponse
	path := "/v1/instances"
	if before != "" || limit > 0 {
		path += "?"
		if before != "" {
			path += "before=" + before
		}
		if limit > 0 {
			if before != "" {
				path += "&"
			}
			path += "limit=" + strconv.Itoa(limit)
		}
	}
	return out, c.do(ctx, "GET", path, nil, &out)
}

// Domains.
func (c *Client) ListDomains(ctx context.Context) ([]CustomDomainResponse, error) {
	var out []CustomDomainResponse
	return out, c.do(ctx, "GET", "/v1/domains", nil, &out)
}
func (c *Client) CreateDomain(ctx context.Context, req CreateCustomDomainRequest) (CustomDomainResponse, error) {
	var out CustomDomainResponse
	return out, c.do(ctx, "POST", "/v1/domains", req, &out)
}
func (c *Client) DeleteDomain(ctx context.Context, domain string) error {
	return c.do(ctx, "DELETE", "/v1/domains/"+domain, nil, nil)
}

// ListCrons returns every cron on the account when slug is empty,
// or every cron for the given app when slug is non-empty. The slug
// filter is added to the wire only when non-empty so the request
// matches the spec (zero documented parameters) and the server-side
// listCrons handler returns 200 with the full account-scoped list.
func (c *Client) ListCrons(ctx context.Context, slug string) ([]CronResponse, error) {
	path := "/v1/crons"
	if slug != "" {
		path += "?slug=" + slug
	}
	var out []CronResponse
	return out, c.do(ctx, "GET", path, nil, &out)
}
func (c *Client) CreateCron(ctx context.Context, slug string, req CreateCronRequest) (CronResponse, error) {
	var out CronResponse
	return out, c.do(ctx, "POST", "/v1/crons", req, &out)
}

// UpdateCron edits a cron's schedule/path/enabled. Pointer-based
// fields let the caller distinguish "unset" from "explicit zero" —
// matches the partial-update shape of Client.UpdateApp. The wire
// method is PATCH; the idempotency-key auto-mint covers this call
// (TestDo_MutatingCallsCarryIdempotencyKey in client_test.go).
func (c *Client) UpdateCron(ctx context.Context, id string, req UpdateCronRequest) (CronResponse, error) {
	var out CronResponse
	return out, c.do(ctx, "PATCH", "/v1/crons/"+id, req, &out)
}
func (c *Client) DeleteCron(ctx context.Context, id string) error {
	return c.do(ctx, "DELETE", "/v1/crons/"+id, nil, nil)
}

// --- Event-driven surface (Move 2) -----------------------------------------
//
// The 10 routes exposed under /v1/apps/{slug}/invoke[/async],
// /v1/apps/{slug}/queues/{send,receive,{id}/ack},
// /v1/apps/{slug}/delayed-tasks, /v1/delayed-tasks/{id}, and
// /v1/invocations[/{id}]. Names follow the spec's natural verb, not
// the route path — see cmd/sdk-coverage/main.go::methodRouteMap for
// the explicit rename table.

// InvokeApp synchronously invokes an app and long-polls for the
// result. Timeout is bounded by the server (5s on Free, 30s on paid
// plans); the call returns 504 long_poll_timeout if the cap elapses
// before the row reaches a terminal state.
func (c *Client) InvokeApp(ctx context.Context, slug string, req InvokeRequest) (InvokeResponse, error) {
	var out InvokeResponse
	return out, c.do(ctx, "POST", "/v1/apps/"+slug+"/invoke", req, &out)
}

// InvokeAppAsync enqueues the invocation and returns 202 + id + the
// status URL. The drain picks the row up on the next 1s tick.
func (c *Client) InvokeAppAsync(ctx context.Context, slug string, req InvokeRequest) (AsyncInvokeResponse, error) {
	var out AsyncInvokeResponse
	return out, c.do(ctx, "POST", "/v1/apps/"+slug+"/invoke/async", req, &out)
}

// QueueSend enqueues a payload on the per-app FIFO queue. Cap-checked
// against the plan's MaxQueueDepth at the handler.
func (c *Client) QueueSend(ctx context.Context, slug string, req QueueSendRequest) (QueueSendResponse, error) {
	var out QueueSendResponse
	return out, c.do(ctx, "POST", "/v1/apps/"+slug+"/queues/send", req, &out)
}

// QueueReceive long-polls for the next dispatched row on the queue.
// 30s server-side cap; on timeout returns (zero, ErrLongPollTimeout)
// — caller is expected to retry. Stays open across the app's
// dispatched rows until one lands or the cap elapses.
func (c *Client) QueueReceive(ctx context.Context, slug string) (QueueReceiveResponse, error) {
	var out QueueReceiveResponse
	return out, c.do(ctx, "POST", "/v1/apps/"+slug+"/queues/receive", nil, &out)
}

// AckQueueRow is a no-op state change (the row is already completed
// when invocation_done fires) — idempotent; a re-ack returns 204.
func (c *Client) AckQueueRow(ctx context.Context, slug, id string) error {
	return c.do(ctx, "POST", "/v1/apps/"+slug+"/queues/"+id+"/ack", nil, nil)
}

// CreateDelayedTask schedules a delayed-task row to fire at the
// given future timestamp. Cap-checked against MaxDelayedTasksPerApp.
func (c *Client) CreateDelayedTask(ctx context.Context, slug string, req DelayedTaskRequest) (DelayedTaskResponse, error) {
	var out DelayedTaskResponse
	return out, c.do(ctx, "POST", "/v1/apps/"+slug+"/delayed-tasks", req, &out)
}

// GetDelayedTask returns a single delayed-task by id. Account-scoped
// — cross-account reads surface 404, not 200 with a foreign row.
func (c *Client) GetDelayedTask(ctx context.Context, id string) (DelayedTaskResponse, error) {
	var out DelayedTaskResponse
	return out, c.do(ctx, "GET", "/v1/delayed-tasks/"+id, nil, &out)
}

// CancelDelayedTask cancels a pending delayed-task. Idempotent — a
// re-cancel on a terminal row returns 404 invocation_not_found.
func (c *Client) CancelDelayedTask(ctx context.Context, id string) error {
	return c.do(ctx, "DELETE", "/v1/delayed-tasks/"+id, nil, nil)
}

// ListInvocations paginates the account's invocations by `?before=<id>`
// (the LAST id of the returned slice). Defaults to 20 per page.
func (c *Client) ListInvocations(ctx context.Context, before string, limit int) (ListInvocationsResponse, error) {
	var out ListInvocationsResponse
	q := url.Values{}
	if before != "" {
		q.Set("before", before)
	}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	path := "/v1/invocations"
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	return out, c.do(ctx, "GET", path, nil, &out)
}

// GetInvocation returns a single invocation by id. Account-scoped.
func (c *Client) GetInvocation(ctx context.Context, id string) (Invocation, error) {
	var out Invocation
	return out, c.do(ctx, "GET", "/v1/invocations/"+id, nil, &out)
}

// API keys.
//
// CreateKey accepts an explicit scopes slice. Pass nil to preserve the
// historical "full access" behavior (the server defaults nil to
// ["admin"]). See ADR-034 for the scope vocabulary.
func (c *Client) ListKeys(ctx context.Context) ([]APIKeyResponse, error) {
	var out []APIKeyResponse
	return out, c.do(ctx, "GET", "/v1/keys", nil, &out)
}
func (c *Client) CreateKey(ctx context.Context, label string, scopes []string) (APIKeyResponse, error) {
	var out APIKeyResponse
	return out, c.do(ctx, "POST", "/v1/keys", CreateKeyRequest{Label: label, Scopes: scopes}, &out)
}
func (c *Client) DeleteKey(ctx context.Context, id string) error {
	return c.do(ctx, "DELETE", "/v1/keys/"+id, nil, nil)
}

// Audit events (IAM-4, ADR-035). The events table is append-only
// (spec §5), so this surface is read-only by design. since and
// kindPrefix are optional — pass empty strings to read the full
// 50-row default window. limit is bounded server-side at 100; values
// larger are silently capped per the same convention as ListSecrets.

// ListAuditEvents returns the caller's auth audit events newest-first.
func (c *Client) ListAuditEvents(ctx context.Context, since, kindPrefix string, limit int) (ListAuditEventsResponse, error) {
	var out ListAuditEventsResponse
	q := url.Values{}
	if since != "" {
		q.Set("since", since)
	}
	if kindPrefix != "" {
		q.Set("kind_prefix", kindPrefix)
	}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	path := "/v1/audit-events"
	if encoded := q.Encode(); encoded != "" {
		path += "?" + encoded
	}
	return out, c.do(ctx, "GET", path, nil, &out)
}

// GetAuditEvent fetches a single auth audit event by id. Cross-account
// lookups 404 the same way unknown ids do, so a caller cannot enumerate
// other accounts' row counts by id-probing.
func (c *Client) GetAuditEvent(ctx context.Context, id string) (AuditEventResponse, error) {
	var out AuditEventResponse
	return out, c.do(ctx, "GET", "/v1/audit-events/"+id, nil, &out)
}

// CLI auth device-code flow (spec §2.2).

// MintCliAuthCode anonymously mints a fresh device code.
func (c *Client) MintCliAuthCode(ctx context.Context) (CliAuthCodeResponse, error) {
	var out CliAuthCodeResponse
	return out, c.do(ctx, "POST", "/v1/cli-auth/code", struct{}{}, &out)
}

// ExchangeCliAuthCode polls the server for the user's approval.
func (c *Client) ExchangeCliAuthCode(ctx context.Context, code string) (CliAuthExchangeResponse, error) {
	var out CliAuthExchangeResponse
	return out, c.do(ctx, "POST", "/v1/cli-auth/exchange",
		CliAuthExchangeRequest{Code: code}, &out)
}

// Dashboard auth (issue #165, ADR-032 PR #2). The SDK uses these
// against a tokenless Client (NewClient returns one with token="");
// the auth flows issue a session cookie but the SDK does not consume
// it — the dashboard cookie is the only auth artifact on the browser
// side. Programmatic auth stays on the device-code flow above, where
// the customer can mint a real api_key via the dashboard after
// signing in.
//
// PasswordSignup creates an account (if the email is unbound) and
// signs the caller in. The same response shape as PasswordLogin:
// {account_id, plan}, no api_key. Anti-enumeration: a colliding
// signup attempt returns 401 invalid_credentials, not 409 — the
// SDK and the CLI render the same generic "sign in failed" copy.
func (c *Client) PasswordSignup(ctx context.Context, email, password string) (PasswordLoginResponse, error) {
	var out PasswordLoginResponse
	return out, c.do(ctx, "POST", "/signup",
		PasswordSignupRequest{Email: email, Password: password}, &out)
}

// PasswordLogin signs the caller in with email + password. The
// success response does NOT carry an API key — the session cookie is
// the only auth artifact. The SDK does not consume the cookie; the
// caller is expected to follow the 302 redirect or to exchange the
// session via the device-code flow for API access.
func (c *Client) PasswordLogin(ctx context.Context, email, password string) (PasswordLoginResponse, error) {
	var out PasswordLoginResponse
	return out, c.do(ctx, "POST", "/login",
		PasswordLoginRequest{Email: email, Password: password}, &out)
}

// RequestPasswordReset mints a password-reset email. The server
// always returns 200 with an identical body regardless of whether the
// email is bound to an account, so the surface does not leak account
// presence. The full reset URL is sent via the platform's mailer
// (recorded in mail_wiring_test.go); the SDK caller never sees the
// token.
func (c *Client) RequestPasswordReset(ctx context.Context, email string) error {
	return c.do(ctx, "POST", "/login/forgot",
		PasswordResetRequest{Email: email}, nil)
}

// ConfirmPasswordReset consumes a one-shot reset token and sets the
// new password. The token is the base64url-encoded value from the
// email link (NOT the SHA-256 hash the server stored). A replay
// (already-consumed token) returns 410 reset_token_invalid.
func (c *Client) ConfirmPasswordReset(ctx context.Context, token, newPassword string) error {
	return c.do(ctx, "POST", "/auth/reset",
		PasswordResetConfirm{Token: token, NewPassword: newPassword}, nil)
}

// SetPassword updates the password on the currently authenticated
// account. Reachable only after Bearer auth (the dashboard session
// cookie is interchangeable with the bearer token via
// sessionAuthFor). Used by OAuth-only customers to opt into password
// login.
func (c *Client) SetPassword(ctx context.Context, password string) error {
	return c.do(ctx, "POST", "/dashboard/account/set-password",
		SetPasswordRequest{Password: password}, nil)
}

// Logout clears the dashboard session. Idempotent — clearing a
// non-existent session is a no-op.
func (c *Client) Logout(ctx context.Context) error {
	return c.do(ctx, "POST", "/logout", nil, nil)
}

// Secrets (spec §11/G2). Plaintext VALUE never leaves the caller
// except via SetSecret's body.
func (c *Client) ListSecrets(ctx context.Context, slug string) (AppSecretListResponse, error) {
	var out AppSecretListResponse
	return out, c.do(ctx, "GET", "/v1/apps/"+slug+"/secrets", nil, &out)
}

// GetSecrets returns every sealed secret across the caller's account
// (issue #393). One call replaces N per-app ListSecrets calls. Each
// row carries app_id and app_slug so the dashboard renders
// "foo-app / DATABASE_URL" without a parallel /v1/apps lookup.
// Ciphertext is the age-sealed envelope (base64); plaintext VALUE
// is never on the wire (same invariant as ListSecrets). Cursor is
// the (app_slug, key) pair, encoded as "<slug>|<key>" — see
// ADR-045.
func (c *Client) GetSecrets(ctx context.Context, before string, limit int) (ListSecretsForAccountResponse, error) {
	var out ListSecretsForAccountResponse
	path := "/v1/secrets"
	if before != "" || limit > 0 {
		path += "?"
		if before != "" {
			path += "before=" + url.QueryEscape(before)
		}
		if limit > 0 {
			if before != "" {
				path += "&"
			}
			path += "limit=" + strconv.Itoa(limit)
		}
	}
	return out, c.do(ctx, "GET", path, nil, &out)
}
func (c *Client) SetSecret(ctx context.Context, slug, key, value string) error {
	return c.do(ctx, "PUT", "/v1/apps/"+slug+"/secrets/"+key,
		PutAppSecretRequest{Value: value}, nil)
}
func (c *Client) UnsetSecret(ctx context.Context, slug, key string) error {
	return c.do(ctx, "DELETE", "/v1/apps/"+slug+"/secrets/"+key, nil, nil)
}

// Env vars (issue #395 / ADR-045). Plaintext by contract — values are
// non-sensitive runtime config. Value never appears in the GET
// response; only the key set + timestamps do (AppEnvResponse shape).
// PutAppsSlugEnvKey's body is the value path; DeleteAppsSlugEnvKey is
// identity-only. Method names match the sdk-coverage gate's
// MethodResource convention so every spec route ships with a Go SDK
// method (the older secrets surface used helper-style names like
// ListSecrets/SetSecret/UnsetSecret which the gate tolerates as
// pre-existing helpers — new surfaces follow MethodResource).
func (c *Client) GetAppsSlugEnv(ctx context.Context, slug string) (AppEnvListResponse, error) {
	var out AppEnvListResponse
	return out, c.do(ctx, "GET", "/v1/apps/"+slug+"/env", nil, &out)
}
func (c *Client) PutAppsSlugEnvKey(ctx context.Context, slug, key, value string) error {
	return c.do(ctx, "PUT", "/v1/apps/"+slug+"/env/"+key,
		PutAppEnvRequest{Value: value}, nil)
}
func (c *Client) DeleteAppsSlugEnvKey(ctx context.Context, slug, key string) error {
	return c.do(ctx, "DELETE", "/v1/apps/"+slug+"/env/"+key, nil, nil)
}

// Usage.
func (c *Client) GetUsage(ctx context.Context, month string) (UsageResponse, error) {
	var out UsageResponse
	return out, c.do(ctx, "GET", "/v1/usage?month="+month, nil, &out)
}

// GetAppMetrics returns the per-app metrics snapshot for slug over
// the named range window. rng is one of "5m", "15m", "1h", "6h",
// "24h", "7d", "15d" — empty falls back to the server's default
// (5m). Issue #273 / ADR-042.
func (c *Client) GetAppMetrics(ctx context.Context, slug, rng string) (AppMetricsResponse, error) {
	var out AppMetricsResponse
	path := "/v1/apps/" + slug + "/metrics"
	if rng != "" {
		path += "?range=" + rng
	}
	return out, c.do(ctx, "GET", path, nil, &out)
}

// GetAppsMetrics returns the account-wide per-app metrics rollup
// (issue #393). One call replaces N per-app GetAppMetrics calls;
// the response is keyed by app_slug so the dashboard renders rows
// without a parallel /v1/apps lookup. rng follows the same closed
// vocabulary as the per-app endpoint. First Prometheus failure
// short-circuits the entire response with source="degraded: …"
// and zeroed apps — see ADR-045.
func (c *Client) GetAppsMetrics(ctx context.Context, rng string) (AppsMetricsResponse, error) {
	var out AppsMetricsResponse
	path := "/v1/apps/metrics"
	if rng != "" {
		path += "?range=" + rng
	}
	return out, c.do(ctx, "GET", path, nil, &out)
}

// UsageSummary returns the account-wide monthly roll-up
// (used_gb_hours, included_gb_hours, overage_gb_hours, overage_cents).
// Distinct from GetUsage which returns per-app rows; empty month falls
// back to the server's default (current month).
func (c *Client) UsageSummary(ctx context.Context, month string) (UsageSummaryResponse, error) {
	var out UsageSummaryResponse
	path := "/v1/usage/summary"
	if month != "" {
		path += "?month=" + month
	}
	return out, c.do(ctx, "GET", path, nil, &out)
}

// ListDeployments returns a single page of deployments with a
// "next_before" cursor (RFC3339Nano). Use ListDeploymentsAll (added in
// commit 2) to walk every page automatically.
func (c *Client) ListDeployments(ctx context.Context, before string, limit int) (DeploymentListResponse, error) {
	var out DeploymentListResponse
	path := "/v1/deployments?"
	if before != "" {
		path += "before=" + before + "&"
	}
	if limit > 0 {
		path += "limit=" + fmt.Sprintf("%d", limit)
	}
	return out, c.do(ctx, "GET", path, nil, &out)
}

// ListInvoices returns a single page of the authenticated account's
// invoices (issue #259). month is "YYYY-MM" or "" for all months;
// before is the RFC3339Nano cursor for the next page ("" for first
// page). limit is clamped server-side at 100. Empty history returns
// a response with Items=nil (or empty) and no error.
func (c *Client) ListInvoices(ctx context.Context, month, before string, limit int) (InvoiceListResponse, error) {
	var out InvoiceListResponse
	v := url.Values{}
	if month != "" {
		v.Set("month", month)
	}
	if before != "" {
		v.Set("before", before)
	}
	if limit > 0 {
		v.Set("limit", fmt.Sprintf("%d", limit))
	}
	path := "/v1/invoices"
	if encoded := v.Encode(); encoded != "" {
		path += "?" + encoded
	}
	return out, c.do(ctx, "GET", path, nil, &out)
}

// IssueAccountCredit issues a positive-cents credit to the named
// account via POST /v1/admin/accounts/{id}/credits (issue #279).
// accountID is the target account's UUID. idemKey is the
// Idempotency-Key header value; pass an empty string to let the SDK
// auto-UUIDv4 (the typical path for the dashboard) or a stable
// string (the CLI's `cli-admin-credit-…` path) so a flaky-network
// retry returns the same credit_id. reason is operator-supplied
// (3..500 chars; the handler validates client-side).
//
// Auth: requires an admin-scoped API key in c.Token (admin-only
// endpoint, two-layer auth: requireScope(ScopesAdminOnly) +
// adminAllows email allowlist).
func (c *Client) IssueAccountCredit(ctx context.Context, accountID, idemKey string, cents int64, reason string) (AccountCreditResponse, error) {
	if idemKey == "" {
		idemKey = newUUIDv4()
	}
	body, err := json.Marshal(map[string]any{"cents": cents, "reason": reason})
	if err != nil {
		return AccountCreditResponse{}, err
	}
	req, err := http.NewRequestWithContext(ctx, "POST",
		c.baseURL+"/v1/admin/accounts/"+accountID+"/credits", bytes.NewReader(body))
	if err != nil {
		return AccountCreditResponse{}, err
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	req.Header.Set("Idempotency-Key", idemKey)
	req.Header.Set("Content-Type", "application/json")
	var out AccountCreditResponse
	return out, c.doReq(c.http, req, &out)
}

// ConsumeInvoiceCredits drains the account's active credits FIFO
// against an invoice's overage (issue #279 PR-C). Triggered by the
// operator at month-rollover today; the same reducer will be called
// from the PR-B UpsertInvoice webhook Tx and a future meterd cron —
// the HTTP endpoint is the contract the SDK exposes.
//
// invoiceID is the row ID (UUID) from GET /v1/invoices; the reducer
// re-resolves account + period + provider_invoice_id internally.
// idemKey is the Idempotency-Key header value — auto-UUIDv4 by
// default; pass a stable string for retryable ops.
//
// Auth: admin-scoped API key (admin-only endpoint, two-layer auth:
// requireScope(ScopesAdminOnly) + adminAllows email allowlist +
// requireMFA).
func (c *Client) ConsumeInvoiceCredits(ctx context.Context, invoiceID, idemKey string) (ConsumeInvoiceResponse, error) {
	if idemKey == "" {
		idemKey = newUUIDv4()
	}
	body, err := json.Marshal(map[string]any{})
	if err != nil {
		return ConsumeInvoiceResponse{}, err
	}
	req, err := http.NewRequestWithContext(ctx, "POST",
		c.baseURL+"/v1/invoices/"+invoiceID+"/consume-credits", bytes.NewReader(body))
	if err != nil {
		return ConsumeInvoiceResponse{}, err
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	req.Header.Set("Idempotency-Key", idemKey)
	req.Header.Set("Content-Type", "application/json")
	var out ConsumeInvoiceResponse
	return out, c.doReq(c.http, req, &out)
}
