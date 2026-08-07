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

// GetDeploymentScan returns the per-deploy grype CVE scan
// payload for one deployment (issue #464 / ADR-055). The
// handler returns a 404 in three cases — the deployment
// row doesn't exist, the deployment belongs to a different
// account (IDOR-safe), or no scan has run yet — and the
// SDK surfaces all three via the same ErrorCode wrapping
// `errors.Is(err, api.ErrNotFound)` callers already branch
// on. The Status field is the closed enum
// (complete|failed|skipped); see pkg/api.ScanResult for the
// full wire shape.
func (c *Client) GetDeploymentScan(ctx context.Context, id string) (ScanResult, error) {
	var out ScanResult
	return out, c.do(ctx, "GET", "/v1/deployments/"+id+"/scan", nil, &out)
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

// RaiseOverageCap sets the account's monthly overage cap (issue #561).
// Pass a non-negative int64 to set the cap (0 = "no overage allowed");
// pass nil to clear the cap (NULL round-trip). The server returns the
// updated account state. Caps are enforced by schedd: once the
// current-month overage meets/exceeds the cap, new wakes are refused
// with CodeAdmissionRefused (HTTP 402).
func (c *Client) RaiseOverageCap(ctx context.Context, overageCapCents *int64) (AccountResponse, error) {
	body := map[string]any{"overage_cap_cents": overageCapCents}
	var out AccountResponse
	return out, c.do(ctx, "POST", "/v1/account/overage-cap", body, &out)
}

// GetEgressAllowlistExtra returns the per-account additive budget
// on top of the plan's apps.egress_allowlist cap (issue #679 /
// PR-B / ADR-082). The response carries the live value plus the
// plan cap and the global ceiling so the CLI can render the
// "Override: N / Plan cap: 16 / Max extra: 1024" trio without
// a second round-trip.
//
// Admin scope + MFA are required (the client passes the same
// auth as for RaiseOverageCap and ChangePlan).
func (c *Client) GetEgressAllowlistExtra(ctx context.Context) (AccountEgressAllowlistExtraResponse, error) {
	var out AccountEgressAllowlistExtraResponse
	return out, c.do(ctx, "GET", "/v1/account/egress_allowlist_extra", nil, &out)
}

// SetEgressAllowlistExtra sets the per-account additive budget
// (issue #679 / PR-B / ADR-082). Pass 0 to clear the override (the
// plan cap is authoritative again). Negative values or values
// above the global ceiling are rejected with
// CodeAccountEgressAllowlistExtraOutOfRange (HTTP 400).
//
// Admin scope + MFA are required.
func (c *Client) SetEgressAllowlistExtra(ctx context.Context, extra int) (AccountEgressAllowlistExtraResponse, error) {
	var out AccountEgressAllowlistExtraResponse
	return out, c.do(ctx, "PATCH", "/v1/account/egress_allowlist_extra",
		SetAccountEgressAllowlistExtraRequest{Extra: extra}, &out)
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

// ListWakeTimeline returns the typed wake-stage timeline for a given
// wake_id (issue #517 / PR-C / ADR-064). Oldest-first; the dashboard
// reads it as a forward narrative (queue_accepted → admitted →
// boot_started → boot_completed → readiness_200 → proxy_first_byte).
//
// since is an RFC 3339 timestamp; rows strictly older are skipped
// (the dashboard's "load older" infinite-scroll). limit is bounded
// server-side at 1000; values larger are silently capped per the
// same convention as ListSecrets / ListAuditEvents.
//
// Cross-account visibility is enforced server-side: the slug must
// resolve to the caller's account, and each row's data.app_id is
// forge-checked against the resolved app id. A row that mismatches
// is dropped silently so a malicious admin can't surface a
// foreign-tenant frame in this timeline.
func (c *Client) ListWakeTimeline(ctx context.Context, slug, wakeID, since string, limit int) (WakeTimelineResponse, error) {
	var out WakeTimelineResponse
	q := url.Values{}
	if since != "" {
		q.Set("since", since)
	}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	path := "/v1/apps/" + slug + "/wakes/" + wakeID + "/timeline"
	if encoded := q.Encode(); encoded != "" {
		path += "?" + encoded
	}
	return out, c.do(ctx, "GET", path, nil, &out)
}

// ListAuditEvents returns the caller's auth audit events newest-first.
// includeAnonymous (Wave 0 PR-C / ADR-047) toggles subject=NULL rows —
// the defensive case where the app row was deleted between wake and
// the stateless-advisory audit emit. appID filters the overscan
// window to events whose data.app_id matches (the dashboard's per-app
// drill-down).
func (c *Client) ListAuditEvents(ctx context.Context, since, kindPrefix, appID string, limit int, includeAnonymous bool) (ListAuditEventsResponse, error) {
	var out ListAuditEventsResponse
	q := url.Values{}
	if since != "" {
		q.Set("since", since)
	}
	if kindPrefix != "" {
		q.Set("kind_prefix", kindPrefix)
	}
	if appID != "" {
		q.Set("app_id", appID)
	}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	if includeAnonymous {
		q.Set("include_anonymous", "true")
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
func (c *Client) SetSecret(ctx context.Context, slug, key, value string) error {
	return c.do(ctx, "PUT", "/v1/apps/"+slug+"/secrets/"+key,
		PutAppSecretRequest{Value: value}, nil)
}

// SetAppEvictionPriority (issue #475) PATCHes the per-app eviction
// tier on the PATCH /v1/apps/{slug} endpoint. The priority argument
// is the closed enum 'best_effort' or 'reserved' (mirrors
// api.EvictionPriority in pkg/api/dto.go). The plan gate (Free +
// reserved) and the per-account cap (Plan.ReservedConcurrencyPerAccount)
// are enforced server-side; this helper is a thin one-liner so
// customer code never builds the UpdateAppRequest struct directly
// for the eviction-tier field. The response body is the updated
// AppResponse (matches SetSecret's no-return convention — the caller
// can GET the app if they need the post-PATCH projection).
func (c *Client) SetAppEvictionPriority(ctx context.Context, slug, priority string) error {
	return c.do(ctx, "PATCH", "/v1/apps/"+slug,
		UpdateAppRequest{EvictionPriority: &priority}, nil)
}
func (c *Client) UnsetSecret(ctx context.Context, slug, key string) error {
	return c.do(ctx, "DELETE", "/v1/apps/"+slug+"/secrets/"+key, nil, nil)
}

// Private-registry Basic Auth (issue #461 / ADR-062). Password is
// sealed at rest server-side; the SDK only carries plaintext in the
// PUT body and never sees the ciphertext. Hosts MUST be supplied with
// an explicit "https://" prefix; apid rejects schemeless / http://
// inputs with 400 invalid_registry_host.
func (c *Client) ListAppRegistryCredentials(ctx context.Context, slug string) (AppRegistryCredentialListResponse, error) {
	var out AppRegistryCredentialListResponse
	return out, c.do(ctx, "GET", "/v1/apps/"+slug+"/registry-credentials", nil, &out)
}
func (c *Client) SetAppRegistryCredential(ctx context.Context, slug, registry, username, password string) (AppRegistryCredentialResponse, error) {
	var out AppRegistryCredentialResponse
	return out, c.do(ctx, "PUT", "/v1/apps/"+slug+"/registry-credentials",
		PutAppRegistryCredentialRequest{Registry: registry, Username: username, Password: password}, &out)
}
func (c *Client) DeleteAppRegistryCredential(ctx context.Context, slug, registry string) error {
	return c.do(ctx, "DELETE", "/v1/apps/"+slug+"/registry-credentials?registry="+url.QueryEscape(registry), nil, nil)
}

// Usage.
//
// GetUsage returns per-app usage rows for the given month — the wire
// shape is an ARRAY of UsageResponse objects, not a single struct.
// Mirrors the canonical Go SDK in pkg/api/client.go. See memory:
// getusage-wire-shape-mismatch for the history of the array contract.
func (c *Client) GetUsage(ctx context.Context, month string) ([]UsageResponse, error) {
	var out []UsageResponse
	return out, c.do(ctx, "GET", "/v1/usage?month="+month, nil, &out)
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

// Org surface (issue #190 / IAM-6 / ADR-061, PR 5). The 11 methods
// below mirror the spec routes documented under api/openapi.yaml
// paths /v1/orgs*, /v1/invitations/{token}. Each maps 1:1 to a
// spec route so the sdk-coverage gate (cmd/sdk-coverage) doesn't
// false-positive on drift. Bearer-auth only; account-scoped routes
// (`ListOrgs`, `CreateOrg`) skip the X-Active-Org hint, path-scoped
// routes require it (the apid loadOrg middleware resolves the slug
// from the path and stamps the membership onto the principal).

// ListOrgs returns the orgs the caller has an active membership in
// (the personal org + every shared org the caller belongs to).
// Account-scoped — no X-Active-Org hint needed.
func (c *Client) ListOrgs(ctx context.Context) (OrgListResponse, error) {
	var out OrgListResponse
	return out, c.do(ctx, "GET", "/v1/orgs", nil, &out)
}

// CreateOrg creates a new shared (non-personal) org. The caller
// becomes the first owner. Personal orgs are minted by the PR 3
// backfill and cannot be re-created here.
func (c *Client) CreateOrg(ctx context.Context, req CreateOrgRequest) (OrgResponse, error) {
	var out OrgResponse
	return out, c.do(ctx, "POST", "/v1/orgs", req, &out)
}

// GetOrg returns the active org by slug. Authz: any active member
// (`org.view`); non-members see 403 `org_role_forbidden`. Unknown
// slugs are 404 `org_not_found`.
func (c *Client) GetOrg(ctx context.Context, slug string) (OrgResponse, error) {
	var out OrgResponse
	return out, c.do(ctx, "GET", "/v1/orgs/"+slug, nil, &out)
}

// PatchOrg applies a partial update to the org (name and/or plan).
// Authz routing:
//   - Name → org.manage_billing (owner + billing)
//   - Plan → org.change_plan     (owner only)
//
// Personal orgs are immutable (409 `org_personal_immutable`).
func (c *Client) PatchOrg(ctx context.Context, slug string, req PatchOrgRequest) (OrgResponse, error) {
	var out OrgResponse
	return out, c.do(ctx, "PATCH", "/v1/orgs/"+slug, req, &out)
}

// DeleteOrg soft-deletes the org (sets status=deleted_pending).
// Hard-delete + GDPR purge land in PR 8. Personal orgs are
// immutable.
func (c *Client) DeleteOrg(ctx context.Context, slug string) error {
	return c.do(ctx, "DELETE", "/v1/orgs/"+slug, nil, nil)
}

// ListOrgMembers returns the active member list. Removed rows are
// filtered at the API boundary; live-cap count drops on remove
// even though the row stays for audit.
func (c *Client) ListOrgMembers(ctx context.Context, slug string) (MemberListResponse, error) {
	var out MemberListResponse
	return out, c.do(ctx, "GET", "/v1/orgs/"+slug+"/members", nil, &out)
}

// InviteOrgMember mints a 32-byte plaintext token (returned ONCE
// in the response) and stores only the SHA-256 hash. Token expires
// after 14 days. Role cannot be `owner`.
func (c *Client) InviteOrgMember(ctx context.Context, slug string, req InviteMemberRequest) (InvitationWithTokenResponse, error) {
	var out InvitationWithTokenResponse
	return out, c.do(ctx, "POST", "/v1/orgs/"+slug+"/members", req, &out)
}

// ChangeOrgMemberRole changes a member's role. Owner-only
// (`org.change_role`). Role cannot be `owner`; transfer-ownership is
// the only path to owner.
func (c *Client) ChangeOrgMemberRole(ctx context.Context, slug, accountID string, req ChangeMemberRoleRequest) (OrgMemberResponse, error) {
	var out OrgMemberResponse
	return out, c.do(ctx, "PATCH", "/v1/orgs/"+slug+"/members/"+accountID, req, &out)
}

// RemoveOrgMember removes a member. Owner-only
// (`org.remove_members`). Stamps `removed_at` on the row (the row
// stays for audit; live-cap count drops). Self-removal is rejected
// at the boundary.
func (c *Client) RemoveOrgMember(ctx context.Context, slug, accountID string) error {
	return c.do(ctx, "DELETE", "/v1/orgs/"+slug+"/members/"+accountID, nil, nil)
}

// TransferOrgOwnership atomically promotes new_owner_account_id to
// owner and demotes the caller to admin. The new owner must already
// be an active member of the org.
func (c *Client) TransferOrgOwnership(ctx context.Context, slug string, req TransferOwnershipRequest) (OrgResponse, error) {
	var out OrgResponse
	return out, c.do(ctx, "POST", "/v1/orgs/"+slug+"/transfer_ownership", req, &out)
}

// PeekInvitation is a read-only lookup that returns the invitation
// metadata (email, role, org slug, expires_at) without consuming
// the token. Used by the dashboard to render "you've been invited
// to Acme Inc. as developer" before the invitee accepts. The accept
// flow lands in PR 8.
func (c *Client) PeekInvitation(ctx context.Context, token string) (OrgInvitationResponse, error) {
	var out OrgInvitationResponse
	return out, c.do(ctx, "GET", "/v1/invitations/"+token, nil, &out)
}

// AcceptInvitation consumes the token via Store.ConsumeOrgInvitation
// (the load-bearing cap-in-tx check lives there) and inserts the
// bearer as a new active member. Two audit rows fire post-mutation:
// `org.invitation.accepted` and `org.member.added`. Returns 410
// (`org_invitation_invalid`) on unknown / consumed / revoked /
// expired tokens; 409 (`org_already_member`) if the bearer is
// already a member; 403 (`org_member_cap_exceeded`) at the plan cap.
func (c *Client) AcceptInvitation(ctx context.Context, token string) (OrgMemberResponse, error) {
	var out OrgMemberResponse
	return out, c.do(ctx, "POST", "/v1/invitations/"+token+"/accept", nil, &out)
}

// RevokeInvitation stamps revoked_at on a still-pending invitation.
// Owner + admin only (org.invite_members, symmetric with
// InviteOrgMember). Emits `org.invitation.revoked` with an 8-char
// token-hash prefix (never the full hash).
func (c *Client) RevokeInvitation(ctx context.Context, slug, token string) error {
	return c.do(ctx, "DELETE", "/v1/orgs/"+slug+"/invitations/"+token, nil, nil)
}

// GetOrgSeatUsage returns {used, limit, plan} for the active org.
// `limit` is the plan cap (OrgMembersMax). Free / unknown plans
// return 0 (the fail-closed accessor). Visibility-only — PR 9 ships
// the per-seat pricing cut-over.
func (c *Client) GetOrgSeatUsage(ctx context.Context, slug string) (SeatUsageResponse, error) {
	var out SeatUsageResponse
	return out, c.do(ctx, "GET", "/v1/orgs/"+slug+"/seat_usage", nil, &out)
}

// --- Webhook delivery (issue #476 / ADR-076) -----------------------------
//
// The outbound webhook surface mirrors the apid routes
// under /v1/apps/{slug}/webhooks[/...]. Eight endpoints: list,
// create, get, update, delete, rotate-secret, list-deliveries,
// retry-delivery. The wire shape (Create/Update AppWebhookRequest,
// AppWebhookResponse, AppWebhookDeliveryResponse) is sourced from
// pkg/api/webhooks.go and is also embedded as DTOs via the sdk-gen
// aggregator; this file is hand-curated for the Go SDK. See
// memory `sdk-go-errors-hand-curated-subset` for the related
// ErrXxx mirror pattern.

// ListAppWebhooks returns the per-app webhook subscriptions.
func (c *Client) ListAppWebhooks(ctx context.Context, slug string) ([]AppWebhookResponse, error) {
	var out []AppWebhookResponse
	return out, c.do(ctx, "GET", "/v1/apps/"+slug+"/webhooks", nil, &out)
}

// CreateAppWebhook subscribes a target URL to events on the app.
// The plaintext WebhookSecret is sent over the wire and is NEVER
// logged client-side; the response carries only the masked
// constant `***` for WebhookSecretSealedMasked.
func (c *Client) CreateAppWebhook(ctx context.Context, slug string, req CreateAppWebhookRequest) (AppWebhookResponse, error) {
	var out AppWebhookResponse
	return out, c.do(ctx, "POST", "/v1/apps/"+slug+"/webhooks", req, &out)
}

// GetAppWebhook returns a single subscription by id.
func (c *Client) GetAppWebhook(ctx context.Context, slug, id string) (AppWebhookResponse, error) {
	var out AppWebhookResponse
	return out, c.do(ctx, "GET", "/v1/apps/"+slug+"/webhooks/"+id, nil, &out)
}

// UpdateAppWebhook PATCHes the target_url / event_filter /
// retry_policy / enabled triple. Pointer fields let callers
// distinguish "leave as-is" from "set to empty / nil".
func (c *Client) UpdateAppWebhook(ctx context.Context, slug, id string, req UpdateAppWebhookRequest) (AppWebhookResponse, error) {
	var out AppWebhookResponse
	return out, c.do(ctx, "PATCH", "/v1/apps/"+slug+"/webhooks/"+id, req, &out)
}

// DeleteAppWebhook removes the subscription. Pending deliveries
// remain in the ledger but no new ones will be enqueued after
// delete; existing rows drain per their next_attempt_at.
func (c *Client) DeleteAppWebhook(ctx context.Context, slug, id string) error {
	return c.do(ctx, "DELETE", "/v1/apps/"+slug+"/webhooks/"+id, nil, nil)
}

// RotateAppWebhookSecret asks the server to mint a fresh sealed
// secret. The plaintext is server-side and never crosses the wire;
// the response carries only the masked constant and the rotated_at
// timestamp. Subsequent reads of the row return the masked constant.
func (c *Client) RotateAppWebhookSecret(ctx context.Context, slug, id string) (RotateAppWebhookSecretResponse, error) {
	var out RotateAppWebhookSecretResponse
	return out, c.do(ctx, "POST", "/v1/apps/"+slug+"/webhooks/"+id+"/rotate-secret", nil, &out)
}

// ListAppWebhookDeliveries paginates the per-subscription delivery
// ledger. `status` is one of pending|in_flight|succeeded|failed|dead
// or empty (all statuses). pageSize caps the response; pageToken is
// the opaque cursor returned by the previous call.
func (c *Client) ListAppWebhookDeliveries(ctx context.Context, slug, id string, opts ListAppWebhookDeliveriesOptions) (AppWebhookDeliveryListResponse, error) {
	var out AppWebhookDeliveryListResponse
	path := "/v1/apps/" + slug + "/webhooks/" + id + "/deliveries"
	if opts.Status != "" || opts.PageSize > 0 || opts.PageToken != "" {
		q := url.Values{}
		if opts.Status != "" {
			q.Set("status", opts.Status)
		}
		if opts.PageSize > 0 {
			q.Set("page_size", strconv.Itoa(opts.PageSize))
		}
		if opts.PageToken != "" {
			q.Set("page_token", opts.PageToken)
		}
		path += "?" + q.Encode()
	}
	return out, c.do(ctx, "GET", path, nil, &out)
}

// RetryAppWebhookDelivery moves a `dead` row back to `pending` and
// resets next_attempt_at to now(). Returns the refreshed delivery
// row so callers can show "queued for attempt N+1 at HH:MM:SS".
func (c *Client) RetryAppWebhookDelivery(ctx context.Context, slug, id, deliveryID string) (AppWebhookRetryDeliveryResponse, error) {
	var out AppWebhookRetryDeliveryResponse
	return out, c.do(ctx, "POST", "/v1/apps/"+slug+"/webhooks/"+id+"/deliveries/"+deliveryID+"/retry", nil, &out)
}
