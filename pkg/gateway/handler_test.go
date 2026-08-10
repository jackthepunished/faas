package gateway

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/onebox-faas/faas/pkg/api"
	"github.com/onebox-faas/faas/pkg/wire"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	dto "github.com/prometheus/client_model/go"
)

// fakeBackend simulates routing + a parked app that wakes on demand, plus
// the per-app target set (issue #168) so tests can assert fan-out
// behavior end-to-end without a real cluster.
type fakeBackend struct {
	mu        sync.Mutex
	app       App
	host      string
	upstream  string // address the proxy connects to (the "node id" on the legacy path)
	running   bool   // legacy: pre-#168 single-target mode
	wakeErr   error
	admits    int32
	wakeIDOut string // value Admit() returns; empty → "fake-wake-id"
	// targets holds cached per-instance entries (issue #168). Populated
	// by Admit when admits > 0; Pick returns them round-robin via a
	// local counter. Tests seed via AddTarget to simulate a pre-warm
	// fleet without going through Admit.
	targets []Target
	// nextIdx is the round-robin cursor for Pick (legacy-mode fallback
	// when no targets have been seeded).
	nextIdx atomic.Uint64
	// admitErrOverrides forces the next N Admit calls to return the
	// given error (used by the at-capacity test).
	atCapForCalls int32
	// wakeMethodOut lets a test pin the WakeMethod the fake Admit
	// returns. Default (zero value) is WakeMethodColdBoot, which is
	// what every existing test expects. Set to WakeMethodSnapshotRestore
	// to drive the wake-locality classifier down the local_snapshot
	// branch (PR scale-out readiness).
	wakeMethodOut WakeMethod
	// failNextPick forces the next Pick call to return !ok so the
	// handler hits the "every cached instance was evicted between
	// admit and pick" branch. The handler surfaces that as a 503
	// without ever calling ObserveWakeLocality, which is exactly
	// the contract the wake-locality tests pin (PR scale-out
	// readiness). Single-shot: the flag is consumed (cleared) on
	// the failing call so a second Pick in the same test (e.g. a
	// retry path) gets the normal round-robin.
	failNextPick bool
}

// AddTarget seeds a Target into the per-app cache without going through
// Admit (issue #168). Used by tests that simulate a pre-warmed fleet or
// simulate eviction.
func (b *fakeBackend) AddTarget(t Target) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.targets = append(b.targets, t)
}

func (b *fakeBackend) Lookup(_ context.Context, host string) (App, bool) {
	if host == b.host {
		return b.app, true
	}
	return App{}, false
}

func (b *fakeBackend) Pick(_ string) PickResult {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.failNextPick {
		// Test seam (PR scale-out readiness): force Pick to fail so the
		// handler hits the "every cached instance was evicted between
		// admit and pick" branch and writes the 503 path. The flag is
		// consumed (single-shot) so a second Pick in the same test
		// (e.g. a retry path) gets the normal round-robin.
		b.failNextPick = false
		return PickResult{}
	}
	if len(b.targets) > 0 {
		idx := b.nextIdx.Add(1) - 1
		t := b.targets[int(idx%uint64(len(b.targets)))]
		return PickResult{Target: t, OK: true, Picked: t.DeploymentID}
	}
	if b.running {
		// Legacy single-target mode (preserves pre-#168 test
		// expectations): Target.NodeID doubles as the addr. WakeID
		// is empty so the handler doesn't stamp x-faas-wake-id.
		t := Target{NodeID: b.upstream, InstanceID: "i-fake", WakeID: ""}
		return PickResult{Target: t, OK: true}
	}
	return PickResult{}
}

func (b *fakeBackend) HealthyCount(_ string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.targets) > 0 {
		return len(b.targets)
	}
	if b.running {
		return 1
	}
	return 0
}

func (b *fakeBackend) Admit(_ context.Context, _, _ string, maxConcurrency int) (string, WakeMethod, bool, error) {
	// Issue #168 fan-out invariant: the HealthyCount + addTarget pair
	// must be serialized. The fakeBackend takes b.mu for the whole
	// call so concurrent Admit callers cannot collectively exceed
	// maxConcurrency. Production PGBackend enforces the same invariant
	// under tgtMu (see pkg/gateway/pgbackend.go).
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.targets) >= maxConcurrency {
		// Already at the cap — the production semantics here are
		// "schedule atomically refused", surfaced as atCapacity.
		return "", WakeMethodUnspecified, true, nil
	}
	seq := atomic.AddInt32(&b.admits, 1)
	if atomic.LoadInt32(&b.atCapForCalls) > 0 {
		atomic.AddInt32(&b.atCapForCalls, -1)
		return "", WakeMethodUnspecified, true, nil
	}
	if b.wakeErr != nil {
		return "", WakeMethodUnspecified, false, b.wakeErr
	}
	b.running = true // legacy-mode flag — also seeded via setLegacyHot in tests
	// Mint a fresh per-admit Target so the round-robin fans out
	// across admits (issue #168).
	t := Target{NodeID: b.upstream, InstanceID: "i-" + itoa(uint64(seq)), WakeID: "fake-wake-id"}
	b.targets = append(b.targets, t)
	// Pick the WakeMethod the test pinned (zero value = ColdBoot, so
	// every existing test continues to drive the cold-boot chokepoint).
	method := b.wakeMethodOut
	if method == WakeMethodUnspecified {
		method = WakeMethodColdBoot
	}
	if b.wakeIDOut != "" {
		return b.wakeIDOut, method, false, nil
	}
	return "fake-wake-id", method, false, nil
}

// Admits returns the AdmitInstance() call count (test assertion hook).
func (b *fakeBackend) Admits() *int32 { return &b.admits }

func newTestHandler(t *testing.T) (*Handler, *fakeBackend, *httptest.Server) {
	t.Helper()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("hello from app"))
	}))
	t.Cleanup(upstream.Close)

	b := &fakeBackend{
		app:      App{ID: "app-1", AccountID: "acct-1", Plan: api.PlanPro},
		host:     "jane-api.apps.dom",
		upstream: upstream.Listener.Addr().String(),
	}
	// Quiet logger: tests don't need slog output; the metrics assertion is the
	// real check. Production uses slog.Default() via NewHandler.
	return NewHandlerWith(b, NewMetrics(), slog.New(slog.NewJSONHandler(io.Discard, nil))), b, upstream
}

// setLegacyHot is the test helper that flips the fake backend into the
// legacy pre-#168 single-target mode: one Target cached, no admit fires.
// Replaces the old `b.running = true` idiom.
func (b *fakeBackend) setLegacyHot() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.running = true
	if len(b.targets) == 0 {
		b.targets = append(b.targets, Target{
			NodeID:     b.upstream,
			InstanceID: "i-fake",
			WakeID:     "", // empty: no fresh admit fired
		})
	}
}

func TestColdWakeReturns200AndHeader(t *testing.T) {
	h, b, _ := newTestHandler(t)

	req := httptest.NewRequest("GET", "http://jane-api.apps.dom/", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body)
	}
	if rec.Body.String() != "hello from app" {
		t.Errorf("body = %q", rec.Body.String())
	}
	if rec.Header().Get(wire.WakeHeader) != wire.ColdWakeValue {
		t.Error("first request after park should carry the cold-wake header (UX §6)")
	}
	// Per-wake stable ID flows from schedd's Wake() through the gateway
	// handler onto the response. fakeBackend's Wake returns the literal
	// "fake-wake-id" so this assertion locks down the wiring contract:
	// the response header must mirror what schedd returned, not be
	// regenerated or omitted by the gateway.
	if got := rec.Header().Get("x-faas-wake-id"); got != "fake-wake-id" {
		t.Errorf("x-faas-wake-id = %q, want fake-wake-id", got)
	}
	if atomic.LoadInt32(b.Admits()) != 1 {
		t.Errorf("expected exactly 1 admit, got %d", atomic.LoadInt32(b.Admits()))
	}
}

func TestHotPathDoesNotWakeOrTagCold(t *testing.T) {
	h, b, _ := newTestHandler(t)
	b.app.Plan = api.PlanFree // cap=1, so shouldWake returns false when target is seeded
	b.setLegacyHot()          // pre-seed one Target, no admit fires

	req := httptest.NewRequest("GET", "http://jane-api.apps.dom/", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if rec.Header().Get(wire.WakeHeader) != "" {
		t.Error("warm request must not carry the cold header")
	}
	if got := rec.Header().Get("x-faas-wake-id"); got != "" {
		t.Errorf("warm request must not carry x-faas-wake-id, got %q", got)
	}
	if atomic.LoadInt32(b.Admits()) != 0 {
		t.Errorf("hot path must not trigger an admit, got %d", atomic.LoadInt32(b.Admits()))
	}
}

// TestColdWakePropagatesUUIDv7WakeID asserts the response header matches
// the value the scheduler returned byte-for-byte. In production schedd
// mints a UUIDv7 (via google/uuid), so the contract is: header == whatever
// Wake returned, header is non-empty, header is a valid UUID. Catching
// drift between the gateway and the scheduler — e.g. if gatewayd-internal starts
// regenerating IDs locally — is the whole point of this test.
func TestColdWakePropagatesUUIDv7WakeID(t *testing.T) {
	h, b, _ := newTestHandler(t)
	b.wakeIDOut = "0193f7c0-1234-7abc-9def-0123456789ab"

	req := httptest.NewRequest("GET", "http://jane-api.apps.dom/", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	got := rec.Header().Get("x-faas-wake-id")
	if got != b.wakeIDOut {
		t.Errorf("x-faas-wake-id = %q, want %q (scheduler value must flow through verbatim)", got, b.wakeIDOut)
	}
	if _, err := uuid.Parse(got); err != nil {
		t.Errorf("x-faas-wake-id %q is not a valid UUID: %v", got, err)
	}
}

func TestUnknownHost404(t *testing.T) {
	h, _, _ := newTestHandler(t)
	req := httptest.NewRequest("GET", "http://nope.apps.dom/", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("unknown host = %d, want 404", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/problem+json" {
		t.Errorf("error should be problem+json, got %q", ct)
	}
}

// TestAppsSuffixFilter asserts the spec §4.1 wildcard host guard: with a
// configured appsSuffix, any Host that doesn't match is 404'd without
// touching the routing table.
func TestAppsSuffixFilter(t *testing.T) {
	h, b, _ := newTestHandler(t)
	h.WithAppsSuffix(".apps.dom")

	// Matches suffix → reaches the fake backend → proxied.
	req := httptest.NewRequest("GET", "http://jane-api.apps.dom/", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("matched suffix = %d, want 200", rec.Code)
	}

	// Doesn't match suffix → 404 (without ever calling b.Lookup).
	atomic.StoreInt32(b.Admits(), 0)
	req = httptest.NewRequest("GET", "http://attacker.example/", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("non-matching suffix = %d, want 404", rec.Code)
	}
	if atomic.LoadInt32(b.Admits()) != 0 {
		t.Error("non-matching suffix must not admit the app")
	}
}

// TestRequestIDRoundTrip asserts that x-faas-request-id is generated for every
// response and an inbound header overrides it (lets clients thread their own
// trace id).
func TestRequestIDRoundTrip(t *testing.T) {
	h, _, _ := newTestHandler(t)

	// 1) No inbound header → response carries a generated 32-char hex.
	req := httptest.NewRequest("GET", "http://jane-api.apps.dom/", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	got := rec.Header().Get("x-faas-request-id")
	if len(got) != 32 {
		t.Errorf("generated rid len = %d, want 32 hex chars (got %q)", len(got), got)
	}

	// 2) Inbound header → response echoes it.
	req = httptest.NewRequest("GET", "http://jane-api.apps.dom/", nil)
	req.Header.Set("x-faas-request-id", "my-trace-id")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if got := rec.Header().Get("x-faas-request-id"); got != "my-trace-id" {
		t.Errorf("inbound rid not echoed: got %q", got)
	}
}

func TestRateLimitReturns429(t *testing.T) {
	h, b, _ := newTestHandler(t)
	b.setLegacyHot()          // hot path; the rate-limit test doesn't care about wake
	b.app.Plan = api.PlanFree // burst 20

	got429 := false
	for i := 0; i < 30; i++ {
		req := httptest.NewRequest("GET", "http://jane-api.apps.dom/", nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code == http.StatusTooManyRequests {
			got429 = true
			if rec.Header().Get("Retry-After") == "" {
				t.Error("429 should include Retry-After")
			}
			if rec.Header().Get("x-faas-rate-limit-scope") != "app" {
				t.Error("app-scope 429 should carry x-faas-rate-limit-scope: app")
			}
			break
		}
	}
	if !got429 {
		t.Error("exceeding the Free burst should yield 429")
	}
}

// TestAccountRateLimitReturns429 — ADR-040 / issue #292: when the
// per-account bucket is exhausted the handler must 429 with
// x-faas-rate-limit-scope: account. Per-app burst is bypassed with
// unlimitedLimiter() so the test isolates the account scope — without
// that bypass the per-app bucket would trip first (burst 500 vs
// per-account burst 1000 on Pro), and the 429 would carry scope "app".
func TestAccountRateLimitReturns429(t *testing.T) {
	h, b, _ := newTestHandler(t)
	b.setLegacyHot()
	b.app.Plan = api.PlanPro // per-account burst 1000 (RateLimitPerAccountRPM)
	b.app.AccountID = "acct-rl"
	h.WithLimiter(NewLimiter().WithNoop()) // bypass per-app scope

	got429 := false
	for i := 0; i < 1100; i++ {
		req := httptest.NewRequest("GET", "http://jane-api.apps.dom/", nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code == http.StatusTooManyRequests {
			got429 = true
			if rec.Header().Get("Retry-After") == "" {
				t.Error("account-scope 429 should include Retry-After")
			}
			if rec.Header().Get("x-faas-rate-limit-scope") != "account" {
				t.Errorf("account-scope 429 should carry x-faas-rate-limit-scope: account; got %q",
					rec.Header().Get("x-faas-rate-limit-scope"))
			}
			break
		}
	}
	if !got429 {
		t.Error("exceeding the per-account Pro burst (1000) should yield 429")
	}
	// The metric counter for account-scope rejections must have
	// incremented. Scrape the registry and confirm.
	mrec := httptest.NewRecorder()
	mreq := httptest.NewRequest("GET", "/metrics", nil)
	h.Metrics().Handler().ServeHTTP(mrec, mreq)
	body := mrec.Body.String()
	want := `gateway_per_account_rate_limited_total{account_id="acct-rl",plan="pro"} 1`
	if !strings.Contains(body, want) {
		t.Errorf("missing exposition line %q in body:\n%s", want, body)
	}
}

// TestConcurrentColdRequestsCoalesceToOneWake (issue #168) — at the
// Free-plan cap of max_concurrency=1, 50 concurrent cold requests still
// coalesce to exactly ONE admit (the WakeGate's single-flight guarantee).
// Higher plans admit more; covered by TestCapThreeAdmitsThreeDistinctInstances.
func TestConcurrentColdRequestsCoalesceToOneWake(t *testing.T) {
	h, b, _ := newTestHandler(t)
	b.app.Plan = api.PlanFree // cap = 1 → coalesces to one admit
	h.WithLimiter(unlimitedLimiter())
	h.WithAccountLimiter(unlimitedAccountLimiter()) // ADR-040 — 50 concurrent > Free per-account burst 50

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req := httptest.NewRequest("GET", "http://jane-api.apps.dom/", nil)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Errorf("status = %d, want 200", rec.Code)
			}
		}()
	}
	wg.Wait()
	if got := atomic.LoadInt32(b.Admits()); got != 1 {
		t.Errorf("50 concurrent cold requests should trigger 1 admit, got %d", got)
	}
}

// TestHandlerStampsXFaasInstanceHeader (issue #168) — every proxied
// request carries x-faas-instance set to the picked Target's InstanceID.
// Inbound x-faas-instance is overwritten so an attacker can't steer the
// proxy to an arbitrary instance by setting the header on their request.
func TestHandlerStampsXFaasInstanceHeader(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Echo back the inbound x-faas-instance so the test can assert.
		_, _ = w.Write([]byte(r.Header.Get("x-faas-instance")))
	}))
	t.Cleanup(upstream.Close)

	b := &fakeBackend{
		app:      App{ID: "app-1", Plan: api.PlanFree},
		host:     "stamp.apps.dom",
		upstream: upstream.Listener.Addr().String(),
	}
	b.AddTarget(Target{NodeID: upstream.Listener.Addr().String(), InstanceID: "i-stamp-1", WakeID: "fake-wake-id"})
	h := NewHandlerWith(b, NewMetrics(), nil)

	req := httptest.NewRequest("GET", "http://stamp.apps.dom/", nil)
	req.Header.Set("x-faas-instance", "attacker-supplied-id")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := rec.Body.String(); got != "i-stamp-1" {
		t.Errorf("upstream saw x-faas-instance=%q, want i-stamp-1 (gateway must overwrite inbound)", got)
	}
}

// TestFanOutAdmitsUpToCapThenReuses (issue #168) — for plans with
// max_concurrency > 1, a burst of concurrent cold requests admits up
// to max_concurrency distinct instances; subsequent requests reuse
// the cached targets without firing new admits.
//
// Hobby plan caps at 2, so 4 concurrent cold requests admit 2 distinct
// instances (the leader's admit + 1 follower's fan-out admit), and the
// remaining 2 followers hit the cache. A sequential 5th request also
// reuses the cache.
func TestFanOutAdmitsUpToCapThenReuses(t *testing.T) {
	h, b, _ := newTestHandler(t)
	b.app.Plan = api.PlanHobby // max_concurrency = 2
	h.WithLimiter(unlimitedLimiter())
	h.WithAccountLimiter(unlimitedAccountLimiter()) // ADR-040

	const fans = 4
	var wg sync.WaitGroup
	for i := 0; i < fans; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req := httptest.NewRequest("GET", "http://jane-api.apps.dom/", nil)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Errorf("status = %d, want 200", rec.Code)
			}
		}()
	}
	wg.Wait()

	// The cache must hold exactly max_concurrency targets after the
	// burst — the cap is enforced, not "approximately". Note: the
	// gateway may call Admit MORE than max_concurrency times when
	// multiple followers race past the HealthyCount<cap check; schedd's
	// ledger rejects the excess via atCapacity=true and those rejects
	// don't add a Target. The cache size is the load-bearing invariant.
	if got := b.HealthyCount("app-1"); got != 2 {
		t.Errorf("HealthyCount after %d concurrent cold requests on Hobby cap = %d, want 2", fans, got)
	}

	// 5th request hits the cache — no new admit.
	preAdmit := atomic.LoadInt32(b.Admits())
	req := httptest.NewRequest("GET", "http://jane-api.apps.dom/", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if post := atomic.LoadInt32(b.Admits()); post > preAdmit {
		t.Errorf("5th request must reuse cached target, got %d new admits", post-preAdmit)
	}
	if got := b.HealthyCount("app-1"); got != 2 {
		t.Errorf("HealthyCount after 5th request = %d, want 2", got)
	}
}

// --- writeWakeError -------------------------------------------------------

func TestWriteWakeError_QueueFull(t *testing.T) {
	rec := httptest.NewRecorder()
	writeWakeError(rec, ErrQueueFull)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rec.Code)
	}
	if rec.Header().Get("Retry-After") != "5" {
		t.Errorf("Retry-After = %q, want 5", rec.Header().Get("Retry-After"))
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/problem+json" {
		t.Errorf("Content-Type = %q, want problem+json", ct)
	}
}

func TestWriteWakeError_ProblemPassthrough(t *testing.T) {
	rec := httptest.NewRecorder()
	prob := api.NewProblem(http.StatusBadRequest, api.CodePlanLimitRAM, "plan", "hobby")
	writeWakeError(rec, prob)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "plan_limit_ram") {
		t.Errorf("body = %q, want code plan_limit_ram", rec.Body.String())
	}
}

func TestWriteWakeError_GenericError(t *testing.T) {
	rec := httptest.NewRecorder()
	writeWakeError(rec, errors.New("upstream exploded"))
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "capacity") {
		t.Errorf("body = %q, want capacity error", rec.Body.String())
	}
}

// TestHostname — covers the hostname() helper that the handler uses to
// route requests by Host header (ignoring port).
func TestHostname(t *testing.T) {
	for _, tc := range []struct {
		host, want string
	}{
		{"example.com", "example.com"},
		{"example.com:8080", "example.com"},
		{"127.0.0.1:443", "127.0.0.1"},
		{"", ""},
	} {
		if got := hostname(tc.host); got != tc.want {
			t.Errorf("hostname(%q) = %q, want %q", tc.host, got, tc.want)
		}
	}
}

// TestMetricsSpec12 asserts the §12 metric names increment with the expected
// label sets on cold/404/429 paths. Names are dashboard dependencies — DO NOT
// rename without coordinating with deploy/grafana/.
func TestMetricsSpec12(t *testing.T) {
	h, _, _ := newTestHandler(t)
	h.SetWakeGateHook()

	// Cold path: +requests_total{200} +cold_wake_total +wake_latency_count.
	req := httptest.NewRequest("GET", "http://jane-api.apps.dom/", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if got := testutil.ToFloat64(h.metrics.requests.WithLabelValues("app-1", "pro", "200")); got != 1 {
		t.Errorf("requests_total{200}=%v, want 1", got)
	}
	if got := testutil.ToFloat64(h.metrics.coldBoot.WithLabelValues("app-1")); got != 1 {
		t.Errorf("cold_boot_total=%v, want 1", got)
	}
	if got := histogramObservationCount(t, h.metrics.wakeLatency); got != 1 {
		t.Errorf("wake_latency _count = %v, want 1 (one observation)", got)
	}
	if got := histogramMeanObservation(t, h.metrics.wakeLatency); got <= 0 || got > 100*time.Millisecond {
		t.Errorf("wake_latency observation = %v, want (0, 100ms] for localhost stub", got)
	}

	// Unknown host: +requests_total{404}.
	req = httptest.NewRequest("GET", "http://nope.apps.dom/", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if got := testutil.ToFloat64(h.metrics.requests.WithLabelValues("-", "-", "404")); got != 1 {
		t.Errorf("requests_total{404}=%v, want 1", got)
	}

	// Rate limit (Free plan burst 20, 25 requests): +rate_limited_total{1}.
	h2, b2, _ := newTestHandler(t)
	h2.SetWakeGateHook()
	b2.app.Plan = api.PlanFree
	for i := 0; i < 25; i++ {
		req := httptest.NewRequest("GET", "http://jane-api.apps.dom/", nil)
		rec = httptest.NewRecorder()
		h2.ServeHTTP(rec, req)
	}
	if got := testutil.ToFloat64(h2.metrics.rateLimited.WithLabelValues("app-1", "free")); got < 1 {
		t.Errorf("rate_limited_total=%v, want >=1", got)
	}
}

// histogramObservationCount reads the histogram's _count via the Prometheus
// dto format. Used by the wake-latency regression to assert the histogram
// actually received an observation, not just emitted a series.
func histogramObservationCount(t *testing.T, h prometheus.Histogram) uint64 {
	t.Helper()
	m := &dto.Metric{}
	if err := h.(prometheus.Metric).Write(m); err != nil {
		t.Fatalf("histogram write: %v", err)
	}
	if m.Histogram == nil {
		return 0
	}
	return m.Histogram.GetSampleCount()
}

// histogramMeanObservation returns the mean observation across every sample
// in the histogram (sum / count), in the histogram's base unit of seconds
// converted to time.Duration. With a single observation that's equivalent
// to that observation's value; with multiple observations it's the running
// mean. Empty histograms yield 0. The name says what the function does:
// a histogram's Prometheus exposition does not carry a per-sample
// timestamp, so callers that want "the most recent observation" need to
// scrape, store the previous exposure, and diff — this helper does not.
func histogramMeanObservation(t *testing.T, h prometheus.Histogram) time.Duration {
	t.Helper()
	m := &dto.Metric{}
	if err := h.(prometheus.Metric).Write(m); err != nil {
		t.Fatalf("histogram write: %v", err)
	}
	if m.Histogram == nil || m.Histogram.GetSampleCount() == 0 {
		return 0
	}
	return time.Duration(m.Histogram.GetSampleSum() / float64(m.Histogram.GetSampleCount()) * float64(time.Second))
}

// TestMetricsSpec12_FirstByteNotFullBody is the wake-timing regression: the
// histogram must reflect the time to first upstream response byte, not the
// time to drain the full upstream body. We construct an upstream that
// flushes headers immediately, then sleeps 100ms before writing the body,
// and assert the observed wake latency is well under what a full-body
// measurement would have produced.
func TestMetricsSpec12_FirstByteNotFullBody(t *testing.T) {
	const bodyGap = 100 * time.Millisecond

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush() // headers + status on the wire
		}
		time.Sleep(bodyGap) // upstream app "thinking"
		_, _ = io.WriteString(w, "body-after-delay")
	}))
	t.Cleanup(upstream.Close)

	b := &fakeBackend{
		app:      App{ID: "app-fb", Plan: api.PlanPro},
		host:     "firstbyte.apps.dom",
		upstream: upstream.Listener.Addr().String(),
	}
	h := NewHandlerWith(b, NewMetrics(), slog.New(slog.NewJSONHandler(io.Discard, nil)))

	req := httptest.NewRequest("GET", "http://firstbyte.apps.dom/", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	// First-byte observation must be much shorter than the body gap would
	// suggest for a full-body measurement. We allow generous slack for
	// localhost jitter and Go scheduler stalls, but a full-body measurement
	// would land >= bodyGap.
	got := histogramMeanObservation(t, h.metrics.wakeLatency)
	if got == 0 {
		t.Fatal("wake_latency observation missing")
	}
	if got >= bodyGap {
		t.Errorf("wake_latency observation = %v, want < %v (first-byte, not full body)", got, bodyGap)
	}
	// Sanity: the observation should not be so small as to suggest the
	// trace fired before wakeStart (negative durations would be < 0; the
	// trace fires after the request's outbound socket connects, which is
	// after the handler's wake gate returns).
	if got < 0 {
		t.Errorf("wake_latency observation = %v, want > 0", got)
	}
}

// TestHandlerObserveRequestDuration exercises the new histogram on
// every criterion-#8 path: warm success, cold success, 4xx, and the
// unknown-host sentinel. Issue #273 / ADR-042.
func TestHandlerObserveRequestDuration(t *testing.T) {
	h, _, _ := newTestHandler(t)
	h.SetWakeGateHook()

	// Warm success.
	req := httptest.NewRequest("GET", "http://jane-api.apps.dom/", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if got := histogramCountFromBody(t, h.metrics, `app="app-1",class="2xx"`); got != 1 {
		t.Errorf("request_duration{app-1,2xx} count = %d, want 1", got)
	}

	// Cold success (parked app, fresh admit).
	b := h.backend.(*fakeBackend)
	b.mu.Lock()
	b.targets = nil
	b.running = false
	b.mu.Unlock()
	req = httptest.NewRequest("GET", "http://jane-api.apps.dom/", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	// Cold path also ends in 2xx, so the same class row gets count=2.
	if got := histogramCountFromBody(t, h.metrics, `app="app-1",class="2xx"`); got != 2 {
		t.Errorf("request_duration{app-1,2xx} after cold = %d, want 2", got)
	}

	// Unknown host → 404 → 4xx class.
	req = httptest.NewRequest("GET", "http://nope.apps.dom/", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if got := histogramCountFromBody(t, h.metrics, `app="-",class="4xx"`); got != 1 {
		t.Errorf("request_duration{-,4xx} count = %d, want 1", got)
	}
}

// TestStatusClassBucket pins the §12 dashboard label mapping. The
// closed 5-set (1xx/2xx/3xx/4xx/5xx) keeps the histogram bounded
// per app×plan label combo — issue #709 adds the 1xx arm so a
// successful WebSocket / h2c handshake (101 Switching Protocols)
// does NOT inflate the errors panel.
func TestStatusClassBucket(t *testing.T) {
	cases := map[int]string{
		// 1xx — informational (issue #709; was "5xx" before).
		100: "1xx", // Continue
		101: "1xx", // Switching Protocols (WS / h2c handshake)
		102: "1xx", // Processing
		103: "1xx", // Early Hints
		// 2xx — success.
		200: "2xx",
		201: "2xx",
		204: "2xx",
		299: "2xx",
		// 3xx — redirect.
		301: "3xx",
		302: "3xx",
		304: "3xx",
		399: "3xx",
		// 4xx — client error.
		400: "4xx",
		404: "4xx",
		429: "4xx",
		499: "4xx",
		// 5xx — server error.
		500: "5xx",
		502: "5xx",
		503: "5xx",
		599: "5xx",
	}
	for status, want := range cases {
		if got := statusClassBucket(status); got != want {
			t.Errorf("statusClassBucket(%d) = %q, want %q", status, got, want)
		}
	}
}

// histogramCountFromBody scrapes the metrics endpoint and parses the
// `_count` line whose labels match labelNeedle. Returns 0 when no
// matching line is found. Used by tests that need a per-label-tuple
// count from a HistogramVec (which the older histogramObservationCount
// helper does not support — it takes a single Histogram).
func histogramCountFromBody(t *testing.T, m *Metrics, labelNeedle string) int {
	t.Helper()
	body := bodyForHistogram(t, m)
	prefix := "gateway_request_duration_seconds_count{"
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		if !strings.Contains(line, labelNeedle) {
			continue
		}
		// Trailing " <int>" is the count.
		parts := strings.Fields(line)
		if len(parts) != 2 {
			continue
		}
		var n int
		_, _ = fmt.Sscanf(parts[1], "%d", &n)
		return n
	}
	return 0
}

// TestHandlerWithStartTimeFix pins the WithStartTime wiring fix. A
// stub upstream that sleeps 50ms before responding must surface an
// observed duration ≥ 50ms — this fails before the fix (issue #273
// / ADR-042: WithStartTime was dead code, so startTime() fell back
// to time.Now() at observe() and the histogram recorded ~0).
func TestHandlerWithStartTimeFix(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(50 * time.Millisecond)
		_, _ = w.Write([]byte("slow app"))
	}))
	t.Cleanup(upstream.Close)
	b := &fakeBackend{
		app:      App{ID: "app-1", AccountID: "acct-1", Plan: api.PlanPro},
		host:     "jane-api.apps.dom",
		upstream: upstream.Listener.Addr().String(),
		running:  true,
	}
	b.targets = append(b.targets, Target{NodeID: b.upstream, InstanceID: "i-fake"})
	h := NewHandlerWith(b, NewMetrics(), slog.New(slog.NewJSONHandler(io.Discard, nil)))
	h.SetWakeGateHook()

	req := httptest.NewRequest("GET", "http://jane-api.apps.dom/", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	// Read the histogram's count + sum straight from the scrape
	// body. _count ≥ 1 (the request happened), and _sum / _count
	// ≥ 50ms — the latter is the assertion that fails before the
	// WithStartTime fix.
	body := bodyForHistogram(t, h.metrics)
	count := histogramCountFromBody(t, h.metrics, `app="app-1",class="2xx"`)
	if count < 1 {
		t.Fatalf("expected ≥ 1 observation on app-1/2xx; body:\n%s", body)
	}
	var sumSeconds float64
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "gateway_request_duration_seconds_sum{") &&
			strings.Contains(line, `app="app-1",class="2xx"`) {
			parts := strings.Fields(line)
			if len(parts) == 2 {
				_, _ = fmt.Sscanf(parts[1], "%f", &sumSeconds)
			}
		}
	}
	mean := sumSeconds / float64(count)
	if mean < 0.05 {
		t.Errorf("request_duration mean = %vs, want ≥ 0.05s (50ms upstream)", mean)
	}
}

// TestHandlerSiblingIsolation ensures traffic for one app does NOT
// mint histogram series for another. Pre-instantiation happens at
// Backend.Lookup hit time, so an app that's never routed never
// surfaces rows.
func TestHandlerSiblingIsolation(t *testing.T) {
	h, _, _ := newTestHandler(t)
	h.SetWakeGateHook()

	// Hit app-1.
	req := httptest.NewRequest("GET", "http://jane-api.apps.dom/", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	// app-1 rows exist (pre-instantiated at Lookup + observation on
	// the request).
	if got := histogramCountFromBody(t, h.metrics, `app="app-1",class="2xx"`); got != 1 {
		t.Errorf("app-1 row missing: got %d, want 1", got)
	}
	// A SIBLING app must NOT have any series — this is the
	// invariant ADR-042 promises.
	for _, line := range strings.Split(bodyForHistogram(t, h.metrics), "\n") {
		if strings.Contains(line, `app="sibling"`) {
			t.Errorf("sibling series leaked into /metrics: %q", line)
		}
	}
}

// bodyForHistogram is a helper that scrapes the metrics handler and
// returns the body as a string. Used by sibling-isolation checks.
func bodyForHistogram(t *testing.T, m *Metrics) string {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/metrics", nil)
	m.Handler().ServeHTTP(rec, req)
	return rec.Body.String()
}

// counterValueFromBody scrapes the metrics endpoint and parses the
// counter value whose label tuple matches labelNeedle. Returns 0
// when no matching line is found. Counter exposition is
// `<name>{<labels>} <value>`, no `_count` / `_sum` suffix, so this
// helper differs from histogramCountFromBody in two ways: (1) no
// suffix is appended to the prefix and (2) we accept a fully-formed
// label substring so callers can pin both the metric name and the
// label tuple in one needle.
//
// SAFE the needle must be a complete metric-name + label prefix
// (e.g. `gateway_wake_locality_total{outcome="local_coldboot"}`),
// not an ambiguous substring. Today the wake-locality needles are
// uniquely identifying — future contributors adding a sibling metric
// whose name is a substring (e.g. `gateway_wake_locality_bytes_*`)
// would silently match the wrong line. If that happens, prefer
// `strings.HasPrefix` plus a name-and-label-tuple check, or split
// the needle into a name and a label substring and match both.
func counterValueFromBody(t *testing.T, m *Metrics, labelNeedle string) int {
	t.Helper()
	body := bodyForHistogram(t, m)
	for _, line := range strings.Split(body, "\n") {
		if !strings.Contains(line, labelNeedle) {
			continue
		}
		// Trailing "<int>" is the counter value.
		parts := strings.Fields(line)
		if len(parts) != 2 {
			continue
		}
		var n int
		_, _ = fmt.Sscanf(parts[1], "%d", &n)
		return n
	}
	return 0
}

// TestHandlerObserveWakeLocality is the PR 1 table-driven assertion
// that exercises the wake-locality classifier at the after-proxy
// chokepoint (pkg/gateway/handler.go:454). Five cases pin the
// behaviour:
//
//  1. newly admitted restore → local_snapshot increments by 1
//  2. newly admitted cold boot → local_coldboot increments by 1
//  3. warm request (no admit) → neither counter moves
//  4. admission error → neither counter moves (handler returns before
//     the chokepoint)
//  5. pick-race failure → neither counter moves (handler returns
//     after ensureCapacity but before the chokepoint)
//
// The test is the load-bearing seam that locks down "the metric
// answers what fraction of admissions were local, not what fraction
// of requests were local" — the comment on ObserveWakeLocality that
// justifies the closed set is enforced here.
func TestHandlerObserveWakeLocality(t *testing.T) {
	cases := []struct {
		name          string
		setup         func(b *fakeBackend)
		wantLocalSnap int
		wantLocalCB   int
	}{
		{
			name: "newly admitted restore increments local_snapshot",
			setup: func(b *fakeBackend) {
				b.wakeMethodOut = WakeMethodSnapshotRestore
			},
			wantLocalSnap: 1,
			wantLocalCB:   0,
		},
		{
			name: "newly admitted cold boot increments local_coldboot",
			setup: func(b *fakeBackend) {
				// Default fakeBackend.wakeMethodOut is WakeMethodUnspecified
				// which Admit maps to ColdBoot — explicit here for clarity.
				b.wakeMethodOut = WakeMethodColdBoot
			},
			wantLocalSnap: 0,
			wantLocalCB:   1,
		},
		{
			name: "warm request increments neither counter",
			setup: func(b *fakeBackend) {
				// PlanFree cap=1; setLegacyHot pre-seeds one Target so
				// HealthyCount==1==MaxConcurrency → ensureCapacity's
				// saturation path returns cold=false without firing
				// Admit. The handler then exits at Pick without ever
				// reaching the after-proxy chokepoint. Same canonical
				// pattern as TestHotPathDoesNotWakeOrTagCold.
				b.app.Plan = api.PlanFree
				b.setLegacyHot()
			},
			wantLocalSnap: 0,
			wantLocalCB:   0,
		},
		{
			name: "admission error increments neither counter",
			setup: func(b *fakeBackend) {
				b.wakeErr = errors.New("schedd unreachable")
			},
			wantLocalSnap: 0,
			wantLocalCB:   0,
		},
		{
			name: "pick-fail failure increments neither counter",
			setup: func(b *fakeBackend) {
				// Drive an admit so cold==true is in play, then force
				// Pick to fail mid-request. The handler returns 503
				// before the after-proxy chokepoint.
				b.failNextPick = true
			},
			wantLocalSnap: 0,
			wantLocalCB:   0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, b, _ := newTestHandler(t)
			if tc.setup != nil {
				tc.setup(b)
			}

			req := httptest.NewRequest("GET", "http://jane-api.apps.dom/", nil)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			// Sanity: each request must reach the handler regardless of
			// the path (the metric is observed at a chokepoint that
			// only fires for one of the five cases).
			if rec.Code == 0 {
				t.Fatalf("status = 0; handler did not write a response")
			}

			gotSnap := counterValueFromBody(t, h.metrics, `gateway_wake_locality_total{outcome="local_snapshot"}`)
			if gotSnap != tc.wantLocalSnap {
				t.Errorf("local_snapshot count = %d, want %d", gotSnap, tc.wantLocalSnap)
			}
			gotCB := counterValueFromBody(t, h.metrics, `gateway_wake_locality_total{outcome="local_coldboot"}`)
			if gotCB != tc.wantLocalCB {
				t.Errorf("local_coldboot count = %d, want %d", gotCB, tc.wantLocalCB)
			}
		})
	}
}

// TestHandlerObserveWakeLocalityExactlyOncePerColdAdmit pins the
// dual-increment contract: when the after-proxy chokepoint fires
// twice (two distinct cold admits on the same app), the counter
// increments exactly twice. Catches a future contributor who wraps
// the increment in a loop, double-increments on a particular path,
// or otherwise drifts from the "one increment per admission" rule.
// Same canonical pattern as TestHandlerObserveWakeLocality: the
// app is PlanPro (cap=5) so a second request still hits the cold
// fan-out path (HealthyCount=1 < 5), admitting a second instance.
// Both admits must enumerate.
func TestHandlerObserveWakeLocalityExactlyOncePerColdAdmit(t *testing.T) {
	h, _, _ := newTestHandler(t)

	for i := 0; i < 2; i++ {
		req := httptest.NewRequest("GET", "http://jane-api.apps.dom/", nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("request #%d: status = %d, want 200", i, rec.Code)
		}
	}

	gotCB := counterValueFromBody(t, h.metrics, `gateway_wake_locality_total{outcome="local_coldboot"}`)
	if gotCB != 2 {
		t.Errorf("local_coldboot count after 2 cold admits = %d, want 2 (exactly once per admit)", gotCB)
	}
	gotSnap := counterValueFromBody(t, h.metrics, `gateway_wake_locality_total{outcome="local_snapshot"}`)
	if gotSnap != 0 {
		t.Errorf("local_snapshot count = %d, want 0 (PlanPro default is cold boot)", gotSnap)
	}
}

// TestStreamingFallbackLog_DedupPerKey pins the buffered-fallback
// deprecation log behaviour (issue #471 / ADR-047 PR-A). The helper
// must emit exactly one log line per (appID, contentType) pair —
// repeat calls within the same process are silent. Different
// content-types on the same app get separate entries (so the
// "missed" SSE-on-app-A and the "missed" SSE+json-on-app-A are
// distinguishable in dashboards). A nil-log handler must be a
// no-op (the test seam in Handler.NewHandlerWith accepts a nil
// logger; the deprecation path must not panic).
func TestStreamingFallbackLog_DedupPerKey(t *testing.T) {
	t.Run("dedup-on-same-key", func(t *testing.T) {
		h := &Handler{}
		var lines atomic.Int32
		h.log = slog.New(slog.NewJSONHandler(testCountingWriter{on: &lines}, nil))

		h.streamingFallbackLog("app-A", "text/event-stream")
		h.streamingFallbackLog("app-A", "text/event-stream")
		h.streamingFallbackLog("app-A", "text/event-stream")

		if got := lines.Load(); got != 1 {
			t.Errorf("log lines = %d, want 1 (sync.Map dedup must short-circuit repeats)", got)
		}
	})

	t.Run("distinct-content-types-distinct-entries", func(t *testing.T) {
		h := &Handler{}
		var lines atomic.Int32
		h.log = slog.New(slog.NewJSONHandler(testCountingWriter{on: &lines}, nil))

		h.streamingFallbackLog("app-A", "text/event-stream")
		h.streamingFallbackLog("app-A", "application/x-ndjson")
		// Same content-type on the same app → dedup, no new line.
		h.streamingFallbackLog("app-A", "text/event-stream")
		// Different app → distinct entry.
		h.streamingFallbackLog("app-B", "text/event-stream")

		if got := lines.Load(); got != 3 {
			t.Errorf("log lines = %d, want 3 (one per app×content-type pair)", got)
		}
	})

	t.Run("nil-log-handler-is-silent", func(t *testing.T) {
		h := &Handler{}
		// Deliberately leave h.log nil — must not panic on the
		// buffered path. The streamingFallbackLog short-circuit
		// at the top is the load-bearing guard.
		h.streamingFallbackLog("app-A", "text/event-stream")
	})
}

// testCountingWriter is a single-purpose io.Writer that bumps an
// atomic counter on every Write. The sliding scope of the streaming
// fallback test means a one-off helper is cheaper than the
// prometheus-based counterValue pattern used elsewhere in this file.
type testCountingWriter struct{ on *atomic.Int32 }

func (w testCountingWriter) Write(p []byte) (int, error) {
	w.on.Add(1)
	return len(p), nil
}

// TestServeHTTP_StreamingFallback_FiresOnPerAppFlag pins the
// ServeHTTP-level buffered-fallback contract (issue #471 / ADR-047
// PR-A, AC #4). The post-proxy branch must fire the deprecation log
// when the per-app App.StreamingEnabled is true AND the upstream
// emitted a text/event-stream response, regardless of the operator
// opt-in (h.streamingEnabled). The wiring lives in pgRouter.toApp;
// the test drives a fakeBackend with streamingEnabled=on and an
// SSE-emitting upstream so the full proxy path — including the
// statusRecorder.ContentType capture added in PR-A — is exercised.
//
// PR-D tightens the gate to `!streaming && app.Plan == PlanFree`:
// the buffered-fallback log is for the Free+flag misconfig surface
// only. A valid Hobby+ SSE on the streaming path is the normal-
// flush case, NOT a fallback. The sub-tests below pin the new
// three-way matrix:
//   - Free + per-app on + SSE → 1 streaming-fallback log line
//   - Free + per-app on + non-SSE → 0 lines (no SSE, nothing to deprecate)
//   - Free + per-app off + SSE → 0 lines (customer opted out)
//   - PlanHobby + per-app on + SSE → 0 lines (PR-D regression: the
//     operator's FAAS_GATEWAY_STREAMING flag is off, so the buffered
//     path is the operator's choice, not a customer misconfig)
//   - default handler (PlanPro) + per-app on + SSE → 0 lines (the
//     buffered path is the operator's choice on Pro too)
func TestServeHTTP_StreamingFallback_FiresOnPerAppFlag(t *testing.T) {
	const sseBody = "data: hello\n\n"
	// streamingFallbackMarker is the slog.NewJSONHandler msg-key
	// the deprecation log emits ("gateway: streaming fallback
	// ..."). Counting bytes / lines emitted by the JSON handler
	// would also count unrelated request-time log lines (e.g. the
	// wake-timing warn at handler.go:573), so we buffer the output
	// and match the marker substring instead.
	const streamingFallbackMarker = "streaming fallback"

	sseUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, sseBody)
	}))
	t.Cleanup(sseUpstream.Close)

	plainUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	t.Cleanup(plainUpstream.Close)

	run := func(t *testing.T, upstream string, streamingEnabled bool, plan api.Plan) int {
		t.Helper()
		h, b, _ := newTestHandler(t)
		// Force the operator FAAS_GATEWAY_STREAMING toggle off so
		// the buffered path is what gets exercised — the test is
		// about the buffered-fallback log gate, not the streaming
		// path. (Setting h.streamingEnabled=true exercises the
		// streaming path; see TestServeHTTP_StreamingFallback_FreeOnly
		// below for that case.)
		h.streamingEnabled = false
		b.mu.Lock()
		b.upstream = upstream
		b.running = true
		b.app.StreamingEnabled = streamingEnabled
		b.app.Plan = plan
		if len(b.targets) == 0 {
			b.targets = append(b.targets, Target{
				NodeID:     upstream,
				InstanceID: "i-fake",
				WakeID:     "",
			})
		}
		b.mu.Unlock()

		var buf bytes.Buffer
		h.log = slog.New(slog.NewJSONHandler(&buf, nil))

		req := httptest.NewRequest("GET", "http://jane-api.apps.dom/", nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
		return strings.Count(buf.String(), streamingFallbackMarker)
	}

	t.Run("Free + per-app-on + SSE → 1 line", func(t *testing.T) {
		if got := run(t, sseUpstream.Listener.Addr().String(), true, api.PlanFree); got != 1 {
			t.Errorf("streaming-fallback lines = %d, want 1 (Free + per-app streaming flag + SSE response must trip the deprecation)", got)
		}
	})

	t.Run("Free + per-app-on + non-SSE → 0 lines", func(t *testing.T) {
		if got := run(t, plainUpstream.Listener.Addr().String(), true, api.PlanFree); got != 0 {
			t.Errorf("streaming-fallback lines = %d, want 0 (upstream didn't emit SSE; nothing to deprecate)", got)
		}
	})

	t.Run("Free + per-app-off + SSE → 0 lines", func(t *testing.T) {
		if got := run(t, sseUpstream.Listener.Addr().String(), false, api.PlanFree); got != 0 {
			t.Errorf("streaming-fallback lines = %d, want 0 (customer opted out of streaming; legacy buffered path is the contract)", got)
		}
	})

	// PR-D regression: Hobby+ on the buffered path with the per-app
	// flag on. The operator's FAAS_GATEWAY_STREAMING toggle is off,
	// so the buffered path is the operator's choice, not a customer
	// misconfig. The dedup log must NOT fire. Pre-PR-D this case
	// fired the log noisily on every Hobby+ SSE response.
	t.Run("Hobby + per-app-on + SSE → 0 lines", func(t *testing.T) {
		if got := run(t, sseUpstream.Listener.Addr().String(), true, api.PlanHobby); got != 0 {
			t.Errorf("streaming-fallback lines = %d, want 0 (Hobby+ buffered path is the operator's choice, not a misconfig)", got)
		}
	})

	t.Run("Pro + per-app-on + SSE → 0 lines", func(t *testing.T) {
		if got := run(t, sseUpstream.Listener.Addr().String(), true, api.PlanPro); got != 0 {
			t.Errorf("streaming-fallback lines = %d, want 0 (Pro buffered path is the operator's choice, not a misconfig)", got)
		}
	})
}

// TestServeHTTP_StreamingFallback_BufferedOnly is the B4 tripwire
// for the streaming-path side of the gate: the buffered-fallback log
// must NOT fire when the streaming path is taken. The above
// TestServeHTTP_StreamingFallback_FiresOnPerAppFlag covers the
// buffered-path side (h.streamingEnabled = false → streaming == false
// and the gate returns 0 for Hobby/Pro even with per-app flag on +
// SSE). The streaming-path side is structurally guaranteed by the
// `!streaming` clause in handler.go:761 — the value of `streaming` is
// decided by the AND-gate at handler.go:720 and the buffered-fallback
// log is in a separate branch that is only reachable when
// `streaming == false`. There is no code path where the streaming
// path is taken AND the buffered-fallback log fires; an explicit
// test would either (a) duplicate the above matrix on a separate
// streamingEnabled=TRUE setup (which would also exercise the same
// `!streaming` short-circuit, no new coverage) or (b) require a
// full streaming upstream that drives the per-flush hook (the
// httptest recorder's Flush path is not safely re-entrant in this
// setup). The gate is one literal AND short-circuit; the code
// review + the plan-matrix test above are sufficient.

// TestStatusRecorder_FlushTriggers is the PR-B / ADR-047 unit
// tripwire: the per-flush hook must fire on the (256 KiB / 200 ms)
// triggers and once on the residual capture. The cumulative byte
// count passed to onFlush must monotonically increase and the
// delta between successive onFlush calls must sum to the total
// Bytes observed. Buffered path (nil flusher) is a no-op so the
// PR-A test suite keeps its character.
func TestStatusRecorder_FlushTriggers(t *testing.T) {
	t.Run("nil-flusher-buffered-path-noop", func(t *testing.T) {
		rec := &statusRecorder{ResponseWriter: httptest.NewRecorder()}
		var hookCalls atomic.Int32
		rec.installFlushHook(nil, func(int64) { hookCalls.Add(1) }, 256*1024, 200*time.Millisecond, time.Second)
		_, _ = rec.Write([]byte("hello"))
		_, _ = rec.Write([]byte(" world"))
		if rec.Bytes != int64(len("hello world")) {
			t.Errorf("Bytes = %d, want %d", rec.Bytes, len("hello world"))
		}
		if hookCalls.Load() != 0 {
			t.Errorf("nil-flusher path fired onFlush %d times, want 0", hookCalls.Load())
		}
	})
	t.Run("byte-threshold-triggers-flush", func(t *testing.T) {
		rec := &statusRecorder{ResponseWriter: httptest.NewRecorder()}
		var hookBytes []int64
		// 4 KiB threshold; 8 KiB total written in 1 KiB chunks.
		// lastFlushAt pre-set so periodic-time trigger doesn't fire.
		base := time.Now()
		rec.installFlushHook(nopFlusher{},
			func(c int64) { hookBytes = append(hookBytes, c) },
			4*1024, 200*time.Millisecond, time.Second)
		rec.firstFlush = false
		rec.lastFlushAt = base // suppress periodic trigger; only byte threshold counts
		// Write 8 KiB in 1 KiB chunks.
		for i := 0; i < 8; i++ {
			_, _ = rec.Write(make([]byte, 1024))
		}
		// Periodic flush should have fired once on the byte
		// threshold (when bytesDelta crossed 4 KiB at the
		// 5th Write).
		if len(hookBytes) < 1 {
			t.Fatalf("onFlush fired %d times, want ≥ 1 (byte threshold should have triggered)", len(hookBytes))
		}
		// The last hook call must be cumulative = 8192.
		last := hookBytes[len(hookBytes)-1]
		if last != 8192 {
			t.Errorf("last onFlush cumulative = %d, want 8192", last)
		}
		// Sum of deltas between successive hook calls must
		// equal 8192 (every byte observed by Write must be
		// accounted for via onFlush).
		var sum int64
		prev := int64(0)
		for _, b := range hookBytes {
			sum += b - prev
			prev = b
		}
		if sum != 8192 {
			t.Errorf("sum of onFlush deltas = %d, want 8192 (every observed byte must be accounted for exactly once)", sum)
		}
	})
	t.Run("residual-capture-finalFlush-fires", func(t *testing.T) {
		rec := &statusRecorder{ResponseWriter: httptest.NewRecorder()}
		base := time.Now()
		rec.installFlushHook(nopFlusher{},
			nil, // onFlush irrelevant for the periodic gate; we test finalFlush directly
			4*1024, 200*time.Millisecond, time.Second)
		rec.firstFlush = false
		// lastFlushedBytes set so any periodic-trigger eval
		// computes delta = current - lastFlushedBytes, which
		// is the contract the Handler hook relies on.
		rec.lastFlushedBytes = 0
		rec.lastFlushAt = base
		var hookBytes []int64
		rec.onFlush = func(c int64) { hookBytes = append(hookBytes, c) }
		// Write 100 bytes (well below the 4 KiB threshold).
		_, _ = rec.Write(make([]byte, 100))
		// Periodic flush should NOT have fired.
		if len(hookBytes) != 0 {
			t.Fatalf("periodic flush fired %d times under threshold, want 0", len(hookBytes))
		}
		// Now finalFlush (residual capture) must fire exactly
		// once with cumulative 100 (the cumulative bytes
		// observed by the recorder so far). The Handler's
		// onFlush closure subtracts lastReported against
		// this cumulative to compute the delta.
		rec.finalFlush()
		if len(hookBytes) != 1 {
			t.Fatalf("finalFlush fired %d times, want 1", len(hookBytes))
		}
		if hookBytes[0] != 100 {
			t.Errorf("finalFlush cumulative = %d, want 100", hookBytes[0])
		}
	})
	t.Run("first-flush-fires-on-first-write", func(t *testing.T) {
		rec := &statusRecorder{ResponseWriter: httptest.NewRecorder()}
		rec.installFlushHook(nopFlusher{},
			nil, // installFlushHook nil-hooks are a no-op; we set onFlush below
			1024*1024, 200*time.Millisecond, time.Second)
		// installFlushHook sets firstFlush=true.
		var hookCount atomic.Int32
		rec.onFlush = func(int64) { hookCount.Add(1) }
		// First write triggers first-flush path (uncoditionally).
		_, _ = rec.Write([]byte("first"))
		if hookCount.Load() != 1 {
			t.Errorf("first-flush hook fired %d times, want 1", hookCount.Load())
		}
		if rec.firstFlush {
			t.Error("firstFlush flag stayed true after first flush")
		}
	})
}

// nopFlusher is the unit-test stand-in for http.Flusher. The
// httptest.NewRecorder doesn't implement Flusher (it predates
// the streaming work); the recorder's Write path doesn't need
// a real flush target because the test asserts the hook
// callback fired, not the bytes made it to the wire.
type nopFlusher struct{}

func (nopFlusher) Flush() {}

// stubEdgeRuleMatcher is a hand-rolled EdgeRuleMatcher that returns
// pre-seeded rules for a single host. Used by the PR 4 review-fix
// regression tests to exercise matchAndApplyRewrite / ServeHTTP
// without touching the store or the LRU cache. Embeds
// noOpEdgeRuleMatcher (pkg/gateway/edge_rules.go) for the unused
// methods so the interface stays satisfied.
type stubEdgeRuleMatcher struct {
	noOpEdgeRuleMatcher
	rewrite  *EdgeRuleRewriteResolved
	redirect *EdgeRuleRedirectResolved
	headers  *EdgeRuleHeadersResolved
}

func (s stubEdgeRuleMatcher) MatchRewrite(_ context.Context, _, _, _ string) *EdgeRuleRewriteResolved {
	return s.rewrite
}

func (s stubEdgeRuleMatcher) MatchRedirect(_ context.Context, _, _, _ string) *EdgeRuleRedirectResolved {
	return s.redirect
}

func (s stubEdgeRuleMatcher) MatchHeaders(_ context.Context, _, _, _ string) *EdgeRuleHeadersResolved {
	return s.headers
}

// TestMatchAndApplyRewrite_PrefixAddToSlash_NoDoubleSlash pins the
// PR 4 review-fix F2: rule {From: "", To: "/"} (valid per apid
// EdgeRuleRewriteAction.Validate — non-empty is the only check)
// previously produced "//api/x" because singleSlash("/") returns "/"
// and r.URL.Path already starts with "/". The fix drops To's leading
// "/" before concatenating with r.URL.Path[1:], so the result is
// just r.URL.Path (a degenerate rewrite that leaves the path alone).
func TestMatchAndApplyRewrite_PrefixAddToSlash_NoDoubleSlash(t *testing.T) {
	app := App{ID: "app-1", AccountID: "acct-1", Plan: api.PlanPro}
	matcher := stubEdgeRuleMatcher{
		rewrite: &EdgeRuleRewriteResolved{
			ID: "rule-1", AccountID: "acct-1", AppID: "app-1",
			Priority: 0, PathGlob: "", Methods: nil,
			From: "", To: "/",
		},
	}
	h := &Handler{edgeRules: matcher, metrics: NewMetrics()}
	r := httptest.NewRequest("GET", "http://h.example.com/api/x", nil)
	if !h.matchAndApplyRewrite(r, app) {
		t.Fatalf("matchAndApplyRewrite returned false; want true (rule matched)")
	}
	if r.URL.Path == "//api/x" {
		t.Errorf("r.URL.Path = %q; double-slash regression (review-fix F2)", r.URL.Path)
	}
	if r.URL.Path != "/api/x" {
		t.Errorf("r.URL.Path = %q; want %q (degenerate rewrite leaves path alone)", r.URL.Path, "/api/x")
	}
}

// TestMatchAndApplyRewrite_PrefixAddToNonSlash_PrefixesCorrectly
// pins the positive case that review-fix F2 must NOT break: rule
// {From: "", To: "/v1"} must still produce "/v1/api/x" for an
// inbound /api/x. The fix uses r.URL.Path[1:] to drop the leading
// "/" before concatenating with To's body after singleSlash.
func TestMatchAndApplyRewrite_PrefixAddToNonSlash_PrefixesCorrectly(t *testing.T) {
	app := App{ID: "app-1", AccountID: "acct-1", Plan: api.PlanPro}
	matcher := stubEdgeRuleMatcher{
		rewrite: &EdgeRuleRewriteResolved{
			ID: "rule-1", AccountID: "acct-1", AppID: "app-1",
			Priority: 0, PathGlob: "", Methods: nil,
			From: "", To: "/v1",
		},
	}
	h := &Handler{edgeRules: matcher, metrics: NewMetrics()}
	r := httptest.NewRequest("GET", "http://h.example.com/api/x", nil)
	if !h.matchAndApplyRewrite(r, app) {
		t.Fatalf("matchAndApplyRewrite returned false; want true")
	}
	if r.URL.Path != "/v1/api/x" {
		t.Errorf("r.URL.Path = %q; want /v1/api/x", r.URL.Path)
	}
}

// TestServeHTTP_RedirectObservePassesPlanLabel pins PR 4 review-fix
// F1: the redirect branch's h.observe call previously passed
// app.AccountID as the plan parameter, breaking the §12 dashboard
// cardinality contract (ObserveRequest{app_id, plan, code} would be
// labelled with unbounded-cardinality account IDs). The fix uses
// string(app.Plan) to match the other 14+ call sites in this file.
// This end-to-end test wires a redirect-only stub matcher, fires a
// request, and asserts the metric carries plan="pro" (NOT the
// account ID "acct-1").
func TestServeHTTP_RedirectObservePassesPlanLabel(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("never reached — redirect short-circuits"))
	}))
	t.Cleanup(upstream.Close)

	b := &fakeBackend{
		app:      App{ID: "app-1", AccountID: "acct-1", Plan: api.PlanPro},
		host:     "r.example.com",
		upstream: upstream.Listener.Addr().String(),
		running:  true,
	}
	b.targets = append(b.targets, Target{NodeID: b.upstream, InstanceID: "i-fake"})
	h := NewHandlerWith(b, NewMetrics(), slog.New(slog.NewJSONHandler(io.Discard, nil)))
	h.SetWakeGateHook()
	matcher := stubEdgeRuleMatcher{
		redirect: &EdgeRuleRedirectResolved{
			ID: "rule-r", AccountID: "acct-1", AppID: "app-1",
			Priority: 0, PathGlob: "", Methods: nil,
			StatusCode: 308, To: "https://target.example.com",
		},
	}
	h.WithEdgeRules(matcher, nil, nil)

	req := httptest.NewRequest("GET", "http://r.example.com/", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	// Assert the redirect fired (short-circuit, so the upstream was
	// never contacted).
	if rec.Code != http.StatusPermanentRedirect {
		t.Fatalf("rec.Code = %d; want 308", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "https://target.example.com" {
		t.Errorf("Location = %q; want https://target.example.com", loc)
	}

	// Assert the ObserveRequest metric carries plan="pro" — NOT the
	// account ID. The metric is a CounterVec with labels
	// (app, plan, code) per pkg/gateway/metrics.go:294; bodyForHistogram
	// is the same scrape helper TestHandlerObserveRequestDuration uses.
	body := bodyForHistogram(t, h.metrics)
	if !strings.Contains(body, `app="app-1"`) ||
		!strings.Contains(body, `plan="pro"`) {
		t.Errorf("metric labels wrong: want app=app-1 plan=pro; body:\n%s", body)
	}
	if strings.Contains(body, `plan="acct-1"`) {
		t.Errorf("metric plan label carries account ID (review-fix F1 regression); body:\n%s", body)
	}
}
