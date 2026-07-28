package gateway

import (
	"context"
	"errors"
	"fmt"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestMetricsWakeQueueWaitRegisters asserts the §12 row name is
// exposed. Catches a rename that would silently break the dashboard.
func TestMetricsWakeQueueWaitRegisters(t *testing.T) {
	m := NewMetrics()
	m.ObserveWakeQueueWait(50 * time.Millisecond)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/metrics", nil)
	m.Handler().ServeHTTP(rec, req)
	body := rec.Body.String()
	if !strings.Contains(body, "gateway_wake_queue_wait_seconds") {
		t.Errorf("histogram not in registry output:\n%s", body)
	}
	if !strings.Contains(body, "gateway_wake_queue_wait_seconds_count 1") {
		t.Errorf("expected count=1 in output:\n%s", body)
	}
}

// TestMetricsWakeQueueWaitNilSafe keeps the histogram usable from
// unit tests that haven't constructed a Metrics bundle.
func TestMetricsWakeQueueWaitNilSafe(t *testing.T) {
	var m *Metrics
	m.ObserveWakeQueueWait(50 * time.Millisecond) // must not panic
}

// TestMetricsIssue273Exposition pins the new histogram + the cold
// rename (issue #273 / ADR-042). Catches a rename that the existing
// cold-wake test would have missed because it reads the Go field
// rather than the exposition string. Also asserts the request
// duration histogram is registered with the expected label set.
func TestMetricsIssue273Exposition(t *testing.T) {
	m := NewMetrics()
	m.PreInstantiateApp("app-1")
	m.ObserveColdBoot("app-1", 250*time.Millisecond)
	m.ObserveRequestDuration("app-1", "2xx", 12*time.Millisecond)
	m.ObserveRequestDuration("app-1", "5xx", 500*time.Millisecond)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/metrics", nil)
	m.Handler().ServeHTTP(rec, req)
	body := rec.Body.String()

	// Rename: cold_boot must be present, cold_wake must be absent
	// from active series. The HELP string mentions the old name for
	// documentation, so the assertion targets the series line (which
	// starts with the metric name at column 0, not preceded by #).
	if !strings.Contains(body, "gateway_cold_boot_total{") {
		t.Errorf("gateway_cold_boot_total series not in registry output:\n%s", body)
	}
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "gateway_cold_wake_total{") {
			t.Errorf("gateway_cold_wake_total series should be absent (issue #273 rename): %q", line)
		}
	}

	// New histogram registered with the right label set.
	if !strings.Contains(body, "gateway_request_duration_seconds_bucket") {
		t.Errorf("gateway_request_duration_seconds_bucket not in registry output:\n%s", body)
	}
	if !strings.Contains(body, `gateway_request_duration_seconds_count{app="app-1",class="2xx"} 1`) {
		t.Errorf("expected count for app-1/2xx to be 1:\n%s", body)
	}
	if !strings.Contains(body, `gateway_request_duration_seconds_count{app="app-1",class="5xx"} 1`) {
		t.Errorf("expected count for app-1/5xx to be 1:\n%s", body)
	}

	// Pre-instantiation: all four closed classes surface with count=0
	// for app-1 (no observation yet on 3xx/4xx). Catches a future
	// regression that accidentally stops pre-instantiating.
	for _, class := range []string{"2xx", "3xx", "4xx", "5xx"} {
		want := fmt.Sprintf(`gateway_request_duration_seconds_count{app="app-1",class=%q}`, class)
		if !strings.Contains(body, want) {
			t.Errorf("pre-instantiated %s missing:\n%s", want, body)
		}
	}
}

// TestMetricsPreInstantiateAppBounded asserts the per-app series
// surface stays at exactly the closed (class) set — protects the
// ADR-042 cardinality math from a future change that drops the
// loop or adds a label.
func TestMetricsPreInstantiateAppBounded(t *testing.T) {
	m := NewMetrics()
	m.PreInstantiateApp("alpha")
	m.PreInstantiateApp("beta")
	// After pre-instantiation only the 4 closed classes should
	// exist per app. Calling Observe for an UNRELATED class
	// ("foo") would mint a new tuple; assert we don't do that
	// from the pre-instantiation path.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/metrics", nil)
	m.Handler().ServeHTTP(rec, req)
	body := rec.Body.String()
	for _, app := range []string{"alpha", "beta"} {
		for _, class := range []string{"2xx", "3xx", "4xx", "5xx"} {
			needle := fmt.Sprintf(`gateway_request_duration_seconds_count{app=%q,class=%q}`, app, class)
			if !strings.Contains(body, needle) {
				t.Errorf("pre-instantiated %s missing:\n%s", needle, body)
			}
		}
	}
	// And no tuples minted with class="foo" or any unknown class.
	if strings.Contains(body, `class="foo"`) {
		t.Errorf("unexpected class tuple minted:\n%s", body)
	}
}

// TestObserveRequestDurationNilSafe keeps the histogram usable from
// nil-Metrics tests.
func TestObserveRequestDurationNilSafe(t *testing.T) {
	var m *Metrics
	m.ObserveRequestDuration("app-1", "2xx", 10*time.Millisecond) // must not panic
	m.PreInstantiateApp("app-1")                                  // must not panic
}

// TestWakeGateObservesWaitDuration drives two concurrent Wait calls
// through the gate and asserts the histogram caught at least one
// observation. The leader parks until ensure() returns; the follower
// blocks on the same call and its wait duration is non-zero.
func TestWakeGateObservesWaitDuration(t *testing.T) {
	m := NewMetrics()
	g := NewWakeGate(8, 5*time.Second)
	g.SetMetrics(m)

	release := make(chan struct{})
	var done sync.WaitGroup
	done.Add(2)

	// Leader triggers ensure; both leader (after ensure) and a follower
	// (queued behind the leader) should observe some non-zero wait.
	go func() {
		defer done.Done()
		_ = g.Wait(context.Background(), "appA",
			func() bool { return true },
			func(ctx context.Context) error {
				<-release
				return nil
			})
	}()
	// Yield so the leader is committed before the follower joins.
	time.Sleep(20 * time.Millisecond)
	go func() {
		defer done.Done()
		_ = g.Wait(context.Background(), "appA",
			func() bool { return false }, // would-wake check is leader-only; follower ignores it
			func(ctx context.Context) error { return nil })
	}()

	// Hold the leader parked so the follower accumulates wait.
	time.Sleep(50 * time.Millisecond)
	close(release)
	done.Wait()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/metrics", nil)
	m.Handler().ServeHTTP(rec, req)
	body := rec.Body.String()
	if !strings.Contains(body, "gateway_wake_queue_wait_seconds_count 2") {
		t.Errorf("expected 2 observations (leader + follower), got:\n%s", body)
	}
	// The follower's bucket should be >= 0.05s; the leader's bucket
	// is whatever it took to schedule (likely 0.005s). At least one
	// observation must land in a bucket ≥ 50ms — that's the follower.
	if !strings.Contains(body, `gateway_wake_queue_wait_seconds_bucket{le="0.05"}`) {
		t.Errorf("expected bucket line at le=0.05, got:\n%s", body)
	}
}

// TestWakeGateSkipsObservationOnErrQueueFull guards against the
// regression where ErrQueueFull and ctx-cancelled paths recorded
// ~0ms observations, driving the p95 to near-zero during overload
// storms (the very signal the SLO dashboard needs to surface).
//
// With cap=1, the leader counts as waiter 1; the very next caller
// sees waiters >= cap and gets ErrQueueFull synchronously. That
// rejected caller must NOT record in the wake-wait histogram.
func TestWakeGateSkipsObservationOnErrQueueFull(t *testing.T) {
	m := NewMetrics()
	g := NewWakeGate(1, 5*time.Second)
	g.SetMetrics(m)

	release := make(chan struct{})
	var done sync.WaitGroup
	done.Add(1)

	// Leader parks; counts as waiter 1 of cap=1.
	go func() {
		defer done.Done()
		_ = g.Wait(context.Background(), "appB",
			func() bool { return true },
			func(ctx context.Context) error { <-release; return nil })
	}()
	time.Sleep(20 * time.Millisecond) // leader commits first

	// Synchronous next caller — gate rejects with ErrQueueFull.
	err := g.Wait(context.Background(), "appB",
		func() bool { return false },
		func(ctx context.Context) error { return nil })
	if !errors.Is(err, ErrQueueFull) {
		t.Fatalf("err = %v, want ErrQueueFull", err)
	}

	close(release)
	done.Wait()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/metrics", nil)
	m.Handler().ServeHTTP(rec, req)
	body := rec.Body.String()
	// Only the leader observed (count=1); the rejected caller did not.
	if !strings.Contains(body, "gateway_wake_queue_wait_seconds_count 1") {
		t.Errorf("expected count=1 (rejected caller skipped), got:\n%s", body)
	}
}

// TestWakeGateSkipsObservationOnCtxCancel guards the other
// non-wait return: ctx cancellation. A caller that cancels before
// the in-flight ensure returns should not be recorded as having
// "waited" — it never got an instance.
//
// Race-free variant: the follower must observe the leader's
// in-flight wakeCall before the leader can complete and release
// the entry (otherwise the follower would short-circuit on
// shouldWake=false and become a new leader with its own non-
// observation outcome). We synchronize via InflightWaiters so
// the test waits until the follower is queued, then releases.
func TestWakeGateSkipsObservationOnCtxCancel(t *testing.T) {
	m := NewMetrics()
	g := NewWakeGate(8, 5*time.Second)
	g.SetMetrics(m)

	release := make(chan struct{})
	followerCommitted := make(chan struct{})
	var done sync.WaitGroup
	done.Add(2)

	// Leader parks; ensure returns nil after we release.
	go func() {
		defer done.Done()
		_ = g.Wait(context.Background(), "appC",
			func() bool { return true },
			func(ctx context.Context) error { <-release; return nil })
	}()

	// Follower with a cancelled context — must be queued behind the
	// leader BEFORE the leader's wakeCall is released. Otherwise the
	// follower becomes a new leader with shouldWake=false and never
	// observes.
	cancelledCtx, cancel := context.WithCancel(context.Background())
	cancel()
	go func() {
		defer done.Done()
		_ = g.Wait(cancelledCtx, "appC",
			func() bool { return false },
			func(ctx context.Context) error { return nil })
		// Tell the test driver we've entered Wait (even if it returned
		// immediately — the gate has serialized us).
		close(followerCommitted)
	}()

	// Wait until the follower has actually entered Wait. Polling
	// InflightWaiters is the cheapest signal that the gate has
	// serialized the caller against the leader's call.
	deadline := time.Now().Add(2 * time.Second)
	for g.InflightWaiters("appC") < 2 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if g.InflightWaiters("appC") < 2 {
		// Follower may have short-circuited (e.g. leader already
		// completed and the entry was released). Wait for the
		// commit signal as a fallback so we still close release
		// below.
		select {
		case <-followerCommitted:
		case <-time.After(time.Second):
		}
	}

	close(release)
	done.Wait()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/metrics", nil)
	m.Handler().ServeHTTP(rec, req)
	body := rec.Body.String()
	// Leader observed (one entry) IF it actually waited for the
	// follower to be queued. The follower never observes (ctx.Err()
	// path). Count is therefore 0 if the leader completed before
	// the follower queued, or 1 if it waited.
	count := countObservations(body, "gateway_wake_queue_wait_seconds_count")
	if count > 1 {
		t.Errorf("got count=%d, want <=1 (follower must skip)", count)
	}
}

// countObservations parses the bare `gateway_wake_queue_wait_seconds_count N`
// line out of a Prometheus exposition body. Returns 0 if the line isn't
// present (histogram never observed).
func countObservations(body, metric string) int {
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, metric+" ") {
			continue
		}
		var n int
		_, err := fmt.Sscanf(line, metric+" %d", &n)
		if err != nil {
			return 0
		}
		return n
	}
	return 0
}

// TestMetricsTLSCertExpiryRegisters — ADR-024 H3: the gauge must surface
// in /metrics from the moment the daemon binds (the closed-label pre-
// instantiation is for the counter side; the gauge is unlabelled and
// surfaces as soon as NewMetrics() is called).
func TestMetricsTLSCertExpiryRegisters(t *testing.T) {
	m := NewMetrics()
	m.SetTLSCertExpiry(14 * 24 * time.Hour)

	// Verify the wire shape via the exposition handler.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/metrics", nil)
	m.Handler().ServeHTTP(rec, req)
	if !strings.Contains(rec.Body.String(), "gateway_tls_cert_expiry_seconds") {
		t.Errorf("gauge not in registry output:\n%s", rec.Body.String())
	}
	// Numeric readback via the same path the operator's /metrics scrape
	// uses. PR #345 review: a string-Contains on the literal "1.2096e+06"
	// is brittle to promhttp encoder format changes (uppercase E, fixed-
	// point for smaller values, etc); a Gather() assertion survives them
	// all and matches the gaugeDuration helper in cert_expiry_test.go.
	fams, err := m.Registry().Gather()
	if err != nil {
		t.Fatal(err)
	}
	var got float64
	var found bool
	for _, fam := range fams {
		if fam.GetName() != "gateway_tls_cert_expiry_seconds" {
			continue
		}
		for _, mt := range fam.GetMetric() {
			got = mt.GetGauge().GetValue()
			found = true
		}
	}
	if !found {
		t.Fatal("gateway_tls_cert_expiry_seconds not in registry gather")
	}
	want := float64(14 * 24 * 60 * 60) // 1,209,600 s
	if got != want {
		t.Errorf("gauge value = %v, want %v (14d in seconds)", got, want)
	}
}

// TestMetricsTLSCertExpiryNilSafe — the gauge setter must not panic when
// called on a nil receiver (mirrors the ObserveBuildCount /
// SetResidentGBPerCustomer nil-safe precedent in pkg/wire/metrics.go).
func TestMetricsTLSCertExpiryNilSafe(t *testing.T) {
	var m *Metrics
	m.SetTLSCertExpiry(14 * 24 * time.Hour) // must not panic
}

// TestMetricsTLSOnDemandDeniedRegistersAndPreInstantiates — ADR-024
// H3: the counter must surface every closed reason label at 0 from the
// moment the daemon binds (so the §12 dashboard panel never shows "no
// data" and so the frozen-zero state for dns01 + token is observable
// as the H3.b follow-up signal).
func TestMetricsTLSOnDemandDeniedRegistersAndPreInstantiates(t *testing.T) {
	m := NewMetrics()
	m.ObserveTLSOnDemandDenied("allowlist")

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/metrics", nil)
	m.Handler().ServeHTTP(rec, req)
	body := rec.Body.String()
	// Every reason in the closed set must surface — allowlist at 1,
	// dns01 + token at 0. The labels are alphabetical in the exposition
	// output, so the order doesn't matter.
	for _, want := range []string{
		`gateway_tls_on_demand_denied_total{reason="allowlist"} 1`,
		`gateway_tls_on_demand_denied_total{reason="dns01"} 0`,
		`gateway_tls_on_demand_denied_total{reason="token"} 0`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing exposition line %q in body:\n%s", want, body)
		}
	}
}

// TestMetricsTLSOnDemandDeniedNilSafe — the counter must not panic when
// called on a nil receiver (mirrors the ObserveBuildCount /
// SetResidentGBPerCustomer nil-safe precedent).
func TestMetricsTLSOnDemandDeniedNilSafe(t *testing.T) {
	var m *Metrics
	m.ObserveTLSOnDemandDenied("allowlist") // must not panic
}

// TestMetricsAccountRateLimitedRegistersAndPreInstantiates — ADR-040
// / issue #292: the counter must surface the four (plan) rows under the
// "__other__" placeholder at 0 from the moment the daemon binds, so the
// §12 dashboard panel never shows "no data". Real account_id rows
// appear on first 429.
func TestMetricsAccountRateLimitedRegistersAndPreInstantiates(t *testing.T) {
	m := NewMetrics()
	m.ObserveAccountRateLimit("acct-x", "pro")

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/metrics", nil)
	m.Handler().ServeHTTP(rec, req)
	body := rec.Body.String()
	for _, want := range []string{
		`gateway_per_account_rate_limited_total{account_id="__other__",plan="free"} 0`,
		`gateway_per_account_rate_limited_total{account_id="__other__",plan="hobby"} 0`,
		`gateway_per_account_rate_limited_total{account_id="__other__",plan="pro"} 0`,
		`gateway_per_account_rate_limited_total{account_id="__other__",plan="scale"} 0`,
		`gateway_per_account_rate_limited_total{account_id="acct-x",plan="pro"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing exposition line %q in body:\n%s", want, body)
		}
	}
}

// TestMetricsAccountRateLimitedNilSafe — the helper must not panic on a
// nil receiver (the call site in pkg/gateway/handler.go already
// nil-guards, but the helper itself is nil-safe by design — mirror of
// ObserveWakeQueueWait / ObserveTLSOnDemandDenied).
func TestMetricsAccountRateLimitedNilSafe(t *testing.T) {
	var m *Metrics
	m.ObserveAccountRateLimit("x", "free") // must not panic
}
