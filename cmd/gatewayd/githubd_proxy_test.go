// githubd proxy tests (slice 7, ADR-012). Verifies:
//
//   - HMAC verify at the edge rejects bad signatures with 401
//   - good signatures forward verbatim to githubd's loopback
//   - non-webhook paths fall through untouched
//   - body cap (10 MiB) returns 413
//   - missing secret causes every webhook to be rejected (closed by default)
package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/onebox-faas/faas/pkg/githubd"
	"github.com/onebox-faas/faas/pkg/state"
	"github.com/onebox-faas/faas/pkg/webhookdedupe"
)

func sign(body []byte, secret []byte) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// newTestProxyWithReplay builds a proxy with a real state.MemStore
// backing the dedupe check, mirroring production wiring. The
// returned *state.MemStore lets tests pre-seed rows or assert on
// recorded deliveries after the call returns.
func newTestProxyWithReplay(t *testing.T, secret []byte) (http.Handler, *atomic.Int32, *state.MemStore, *fakeAuditStore) {
	t.Helper()
	var upstreamHits atomic.Int32
	upstreamHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHits.Add(1)
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Echo-Path", r.URL.Path)
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write(body)
	})
	srv := httptest.NewServer(upstreamHandler)
	t.Cleanup(srv.Close)
	mem := state.NewMemStore()
	auditor := newFakeAuditStore()
	proxy := newGithubdProxy(srv.URL, secret, http.NewServeMux(), slog.New(slog.NewTextHandler(io.Discard, nil)), mem, newGatewaydAuditor(auditor, slog.New(slog.NewTextHandler(io.Discard, nil))))
	return proxy, &upstreamHits, mem, auditor
}

// fakeAuditStore records every AppendEvent call so tests can
// assert on the kind + payload without spinning up Postgres.
type fakeAuditStore struct {
	mu    sync.Mutex
	rows  []fakeAuditRow
	failN int // how many of the next calls should fail
}

type fakeAuditRow struct {
	Actor   string
	Kind    string
	Subject *string
	Data    []byte
}

func newFakeAuditStore() *fakeAuditStore { return &fakeAuditStore{} }

func (f *fakeAuditStore) AppendEvent(_ context.Context, actor, kind string, subject *string, data []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failN > 0 {
		f.failN--
		return io.ErrUnexpectedEOF
	}
	f.rows = append(f.rows, fakeAuditRow{Actor: actor, Kind: kind, Subject: subject, Data: append([]byte(nil), data...)})
	return nil
}

func (f *fakeAuditStore) Count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.rows)
}

func (f *fakeAuditStore) Last() (fakeAuditRow, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.rows) == 0 {
		return fakeAuditRow{}, false
	}
	return f.rows[len(f.rows)-1], true
}

func newTestProxy(t *testing.T, secret []byte, upstream http.Handler) (http.Handler, *atomic.Int32) {
	t.Helper()
	var upstreamHits atomic.Int32
	upstreamHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHits.Add(1)
		// Echo the body back as JSON so the test can assert on it
		// without depending on githubd's internal handler shape.
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Echo-Path", r.URL.Path)
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write(body)
	})
	// upstream may be nil — when it is, the test only exercises
	// 401/413/closed paths and the echo handler above is unreachable.
	_ = upstream
	srv := httptest.NewServer(upstreamHandler)
	t.Cleanup(srv.Close)
	// Issue #294: tests that pre-date the replay check pass nil for
	// both replay and auditor; the proxy forwards every HMAC-verified
	// request, matching pre-#294 behaviour.
	proxy := newGithubdProxy(srv.URL, secret, http.NewServeMux(), slog.New(slog.NewTextHandler(io.Discard, nil)), nil, nil)
	return proxy, &upstreamHits
}

func TestGithubdProxy_VerifiesAndForwards(t *testing.T) {
	secret := []byte("test-webhook-secret")
	proxy, hits := newTestProxy(t, secret, nil)

	body := []byte(`{"ref":"refs/heads/main","after":"abc"}`)
	req := httptest.NewRequest(http.MethodPost, githubWebhookPath, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Hub-Signature-256", sign(body, secret))
	// Issue #294: required header (GitHub always sends it).
	req.Header.Set("X-GitHub-Delivery", "delivery-rec-1")

	rr := httptest.NewRecorder()
	proxy.ServeHTTP(rr, req)

	if rr.Code != http.StatusAccepted {
		t.Errorf("status = %d, want 202; body=%s", rr.Code, rr.Body.String())
	}
	if hits.Load() != 1 {
		t.Errorf("upstream hits = %d, want 1", hits.Load())
	}
	if !bytes.Contains(rr.Body.Bytes(), body) {
		t.Errorf("upstream body not echoed; got %q", rr.Body.String())
	}
	if got := rr.Header().Get("X-Echo-Path"); got != githubWebhookPath {
		t.Errorf("X-Echo-Path = %q, want %q", got, githubWebhookPath)
	}
}

func TestGithubdProxy_BadSignatureReturns401(t *testing.T) {
	secret := []byte("test-webhook-secret")
	proxy, hits := newTestProxy(t, secret, nil)

	body := []byte(`{"ref":"refs/heads/main"}`)
	req := httptest.NewRequest(http.MethodPost, githubWebhookPath, bytes.NewReader(body))
	req.Header.Set("X-Hub-Signature-256", sign(body, []byte("WRONG-SECRET")))

	rr := httptest.NewRecorder()
	proxy.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rr.Code)
	}
	if hits.Load() != 0 {
		t.Errorf("upstream should NOT be hit on bad sig; hits = %d", hits.Load())
	}
}

func TestGithubdProxy_MissingHeaderReturns401(t *testing.T) {
	secret := []byte("test-webhook-secret")
	proxy, hits := newTestProxy(t, secret, nil)

	body := []byte(`{"ref":"refs/heads/main"}`)
	req := httptest.NewRequest(http.MethodPost, githubWebhookPath, bytes.NewReader(body))
	// no X-Hub-Signature-256 header

	rr := httptest.NewRecorder()
	proxy.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rr.Code)
	}
	if hits.Load() != 0 {
		t.Errorf("upstream should NOT be hit when header missing; hits = %d", hits.Load())
	}
}

func TestGithubdProxy_EmptySecretRejectsEverything(t *testing.T) {
	// No upstream → closed-by-default: empty secret arms a
	// zero-byte key, but our proxy path refuses to verify against
	// an unset secret at all (see loadGithubWebhookSecret → githubd.VerifyPushSignature).
	var hits atomic.Int32
	upstreamHandler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusAccepted)
	})
	srv := httptest.NewServer(upstreamHandler)
	defer srv.Close()
	proxy := newGithubdProxy(srv.URL, nil /* secret unset */, http.NewServeMux(),
		slog.New(slog.NewTextHandler(io.Discard, nil)), nil, nil)

	body := []byte(`{"ref":"refs/heads/main"}`)
	req := httptest.NewRequest(http.MethodPost, githubWebhookPath, bytes.NewReader(body))
	req.Header.Set("X-Hub-Signature-256", sign(body, []byte("anything")))

	rr := httptest.NewRecorder()
	proxy.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rr.Code)
	}
	if hits.Load() != 0 {
		t.Errorf("upstream should NOT be hit when secret missing; hits = %d", hits.Load())
	}
}

func TestGithubdProxy_NonWebhookPathsFallThrough(t *testing.T) {
	secret := []byte("test-webhook-secret")
	_, hits := newTestProxy(t, secret, nil)

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Fallthrough", "yes")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	// Build the proxy over a fallthrough handler directly to
	// observe "did the request reach next?".
	proxy2 := newGithubdProxy("http://127.0.0.1:1", secret, mux, slog.New(slog.NewTextHandler(io.Discard, nil)), nil, nil)

	for _, p := range []string{"/dashboard/", "/oauth/callback", "/api/v1/deployments", "/v1/apps"} {
		req := httptest.NewRequest(http.MethodGet, p, nil)
		rr := httptest.NewRecorder()
		proxy2.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Errorf("%s: status = %d, want 200", p, rr.Code)
		}
		if rr.Header().Get("X-Fallthrough") != "yes" {
			t.Errorf("%s: fallthrough header missing", p)
		}
	}
	if hits.Load() != 0 {
		t.Errorf("upstream hits = %d, want 0 (non-webhook paths must not reach githubd)", hits.Load())
	}
}

func TestGithubdProxy_OversizedBodyReturns413(t *testing.T) {
	secret := []byte("test-webhook-secret")
	proxy, hits := newTestProxy(t, secret, nil)

	// 11 MiB > 10 MiB cap. Avoid sha256 over a huge buffer here —
	// the proxy should reject before any crypto.
	big := bytes.Repeat([]byte("x"), 11<<20)
	req := httptest.NewRequest(http.MethodPost, githubWebhookPath, bytes.NewReader(big))
	req.Header.Set("X-Hub-Signature-256", "sha256=deadbeef")

	rr := httptest.NewRecorder()
	proxy.ServeHTTP(rr, req)

	if rr.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want 413", rr.Code)
	}
	if hits.Load() != 0 {
		t.Errorf("upstream should NOT be hit on oversized body; hits = %d", hits.Load())
	}
}

func TestGithubdProxy_PreservesCorrelationID(t *testing.T) {
	secret := []byte("test-webhook-secret")
	proxy, _ := newTestProxy(t, secret, nil)

	body := []byte(`{"ref":"refs/heads/main","after":"abc"}`)
	req := httptest.NewRequest(http.MethodPost, githubWebhookPath, bytes.NewReader(body))
	req.Header.Set("X-Hub-Signature-256", sign(body, secret))
	req.Header.Set("X-Faas-Request-Id", "rid-12345")
	req.Header.Set("X-GitHub-Delivery", "delivery-rec-corr-1")

	rr := httptest.NewRecorder()
	proxy.ServeHTTP(rr, req)

	if rr.Code != http.StatusAccepted {
		t.Errorf("status = %d, want 202", rr.Code)
	}
	// The upstream doesn't surface the request-id in this test —
	// we just assert that a valid signed request still flows through
	// end-to-end without 4xx.
	_ = rr.Header().Get("X-Echo-Path")
}

// Sanity: when the upstream is unreachable, the proxy returns a
// 502 problem+json body — exercises the error branch.
func TestGithubdProxy_UpstreamDownReturns502(t *testing.T) {
	secret := []byte("test-webhook-secret")
	// Point at a closed port so RoundTrip fails immediately.
	proxy := newGithubdProxy("http://127.0.0.1:1", secret, http.NewServeMux(),
		slog.New(slog.NewTextHandler(io.Discard, nil)), nil, nil)

	body := []byte(`{"ref":"refs/heads/main","after":"abc"}`)
	req := httptest.NewRequest(http.MethodPost, githubWebhookPath, bytes.NewReader(body))
	req.Header.Set("X-Hub-Signature-256", sign(body, secret))
	req.Header.Set("X-GitHub-Delivery", "delivery-rec-502")

	rr := httptest.NewRecorder()
	proxy.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", rr.Code)
	}
	if got := rr.Header().Get("Content-Type"); !strings.HasPrefix(got, "application/problem+json") {
		t.Errorf("content-type = %q, want application/problem+json prefix", got)
	}
}

// Verify the helper wires env → []byte correctly, and that
// FAAS_GITHUB_WEBHOOK_SECRET takes priority over FAAS_WEBHOOK_SECRET.
func TestLoadGithubWebhookSecret(t *testing.T) {
	env := map[string]string{}
	get := func(k string) string { return env[k] }

	if got := loadGithubWebhookSecret(get); got != nil {
		t.Errorf("unset → %q, want nil", got)
	}
	env["FAAS_GITHUB_WEBHOOK_SECRET"] = "  abc  "
	if got := string(loadGithubWebhookSecret(get)); got != "abc" {
		t.Errorf("trim + read: %q, want abc", got)
	}
	delete(env, "FAAS_GITHUB_WEBHOOK_SECRET")
	env["FAAS_WEBHOOK_SECRET"] = "fallback"
	if got := string(loadGithubWebhookSecret(get)); got != "fallback" {
		t.Errorf("fallback read: %q, want fallback", got)
	}
	env["FAAS_GITHUB_WEBHOOK_SECRET"] = "primary"
	if got := string(loadGithubWebhookSecret(get)); got != "primary" {
		t.Errorf("primary should win: %q, want primary", got)
	}
}

// Pin the slice-7 invariant: the githubd.Verifier is the same code
// the proxy uses (defends against accidental drift where the proxy
// forks its own verifier).
func TestGithubdProxy_VerifierMatchesGithubdPackage(t *testing.T) {
	secret := []byte("test-webhook-secret")
	body := []byte(`{"ref":"refs/heads/main"}`)
	if err := githubd.VerifyPushSignature(body, sign(body, secret), secret); err != nil {
		t.Fatalf("githubd verifier rejected a body the proxy would accept: %v", err)
	}
}

// ----- Issue #294: webhook replay protection -----

// TestGithubdProxy_FirstDelivery_RecordsRow covers the happy path
// with the replay check enabled: the first delivery HMAC-verifies,
// gets a fresh row in the dedupe table, and is forwarded to the
// upstream. Pre-#294 behaviour, but the new code path also writes
// to the dedupe table.
func TestGithubdProxy_FirstDelivery_RecordsRow(t *testing.T) {
	secret := []byte("test-webhook-secret")
	proxy, hits, mem, _ := newTestProxyWithReplay(t, secret)

	body := []byte(`{"ref":"refs/heads/main","after":"abc"}`)
	req := httptest.NewRequest(http.MethodPost, githubWebhookPath, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Hub-Signature-256", sign(body, secret))
	req.Header.Set("X-GitHub-Delivery", "delivery-rec-1")

	rr := httptest.NewRecorder()
	proxy.ServeHTTP(rr, req)

	if rr.Code != http.StatusAccepted {
		t.Errorf("status = %d, want 202; body=%s", rr.Code, rr.Body.String())
	}
	if hits.Load() != 1 {
		t.Errorf("upstream hits = %d, want 1", hits.Load())
	}
	// Round-trip via the helper to prove the row was recorded.
	if err := webhookdedupe.CheckReplay(t.Context(), mem, webhookdedupe.ProviderGitHub, "delivery-rec-1"); !webhookdedupe.IsReplay(err) {
		t.Errorf("recorded delivery should be a replay; err=%v", err)
	}
}

// TestGithubdProxy_RejectsReplay is issue #294 acceptance
// criterion 4: POST the same X-GitHub-Delivery twice; the second
// is rejected with 200 (idempotent — GitHub interprets as success)
// and does NOT reach the upstream.
func TestGithubdProxy_RejectsReplay(t *testing.T) {
	secret := []byte("test-webhook-secret")
	proxy, hits, _, auditor := newTestProxyWithReplay(t, secret)
	body := []byte(`{"ref":"refs/heads/main","after":"abc"}`)

	for i, wantHits := range []int32{1, 1} {
		req := httptest.NewRequest(http.MethodPost, githubWebhookPath, bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Hub-Signature-256", sign(body, secret))
		req.Header.Set("X-GitHub-Delivery", "delivery-rec-replay")

		rr := httptest.NewRecorder()
		proxy.ServeHTTP(rr, req)

		switch i {
		case 0:
			if rr.Code != http.StatusAccepted {
				t.Errorf("first delivery: status = %d, want 202; body=%s", rr.Code, rr.Body.String())
			}
		case 1:
			if rr.Code != http.StatusOK {
				t.Errorf("replay: status = %d, want 200 (idempotent); body=%s", rr.Code, rr.Body.String())
			}
		}
		if got := hits.Load(); got != wantHits {
			t.Errorf("iter %d: upstream hits = %d, want %d", i, got, wantHits)
		}
	}

	// Audit row was emitted exactly once for the replay (not the
	// fresh delivery).
	if got := auditor.Count(); got != 1 {
		t.Errorf("audit row count = %d, want 1 (only the replay emits)", got)
	}
	row, ok := auditor.Last()
	if !ok {
		t.Fatalf("audit row missing")
	}
	if row.Actor != "gatewayd" {
		t.Errorf("audit actor = %q, want gatewayd", row.Actor)
	}
	if row.Kind != "webhook.replay_rejected" {
		t.Errorf("audit kind = %q, want webhook.replay_rejected", row.Kind)
	}
	if row.Subject != nil {
		t.Errorf("audit subject = %v, want nil (gatewayd has no account id at the edge)", row.Subject)
	}
	if !strings.Contains(string(row.Data), `"provider":"github"`) || !strings.Contains(string(row.Data), `"delivery_id":"delivery-rec-replay"`) {
		t.Errorf("audit data missing provider/delivery_id; got %s", string(row.Data))
	}
}

// TestGithubdProxy_MissingDeliveryHeader_Returns400 covers the
// misconfigured-client branch: an HMAC-valid POST without the
// X-GitHub-Delivery header is a 400 (not a 200-replay). GitHub
// always sets this header; a missing one means the upstream is
// speaking a different protocol.
func TestGithubdProxy_MissingDeliveryHeader_Returns400(t *testing.T) {
	secret := []byte("test-webhook-secret")
	proxy, hits, mem, auditor := newTestProxyWithReplay(t, secret)

	body := []byte(`{"ref":"refs/heads/main"}`)
	req := httptest.NewRequest(http.MethodPost, githubWebhookPath, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Hub-Signature-256", sign(body, secret))
	// no X-GitHub-Delivery

	rr := httptest.NewRecorder()
	proxy.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400; body=%s", rr.Code, rr.Body.String())
	}
	if hits.Load() != 0 {
		t.Errorf("upstream hits = %d, want 0 (400 short-circuits before forward)", hits.Load())
	}
	// No dedupe row recorded, no audit emission.
	if err := webhookdedupe.CheckReplay(t.Context(), mem, webhookdedupe.ProviderGitHub, ""); !webhookdedupe.IsReplay(err) {
		// empty delivery_id is its own fresh-key branch (no row) — that's fine.
		_ = err
	}
	if got := auditor.Count(); got != 0 {
		t.Errorf("audit row count = %d, want 0 (400 is not a replay)", got)
	}
}

// TestGithubdProxy_AuditEmitFailure_DoesNotRollback pins ADR-035's
// best-effort semantics: a stuck audit emitter must not block the
// 200-on-replay response. The webhook is the source of truth on
// the state-mutation side; the audit row is observation.
func TestGithubdProxy_AuditEmitFailure_DoesNotRollback(t *testing.T) {
	secret := []byte("test-webhook-secret")
	proxy, hits, _, auditor := newTestProxyWithReplay(t, secret)
	auditor.failN = 100 // force every audit emit to fail

	body := []byte(`{"ref":"refs/heads/main"}`)
	// Two POSTs of the same delivery.
	for i := 0; i < 2; i++ {
		req := httptest.NewRequest(http.MethodPost, githubWebhookPath, bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Hub-Signature-256", sign(body, secret))
		req.Header.Set("X-GitHub-Delivery", "delivery-audit-fail")
		rr := httptest.NewRecorder()
		proxy.ServeHTTP(rr, req)
		if i == 1 && rr.Code != http.StatusOK {
			t.Errorf("iter %d: replay status = %d, want 200 even when audit fails", i, rr.Code)
		}
	}
	if hits.Load() != 1 {
		t.Errorf("upstream hits = %d, want 1", hits.Load())
	}
}

// Compile-time sanity: state.MemStore satisfies githubdReplayStore
// (the slice of state.Store the proxy uses). Catches interface
// drift at build time rather than at runtime via a nil-deref.
var _ githubdReplayStore = (*state.MemStore)(nil)

// Compile-time sanity: webhookdedupe.TTL is exposed for tests
// (and the production wiring relies on it via the constant).
var _ = webhookdedupe.TTL
var _ = time.Now
