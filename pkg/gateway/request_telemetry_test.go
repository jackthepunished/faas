// request_telemetry_test.go — table-driven tests for the recorder +
// publisher (ADR-127).
//
// Covers:
//   - Middleware kill-switch pass-through when Enabled=false
//   - Ringbuffer FIFO order under non-overflowing load
//   - Ringbuffer overflow → oldest row overwritten
//   - Pre-picker (no account/app/route) requests silently dropped
//   - Context-stamped status, latency, cold_boot, trace_id flow through
//   - Publisher ship-success increments shippedTotal
//   - Publisher ship-error retries with backoff then drops, increments droppedTotal
//   - Publisher Wake() drains immediately (no flush-interval wait)
//   - Publisher Stop drains the final batch synchronously
//
// Tests live in package gateway so they can exercise the
// unexported ring buffer + drain plumbing directly.

package gateway

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
)

// nopLog silences the slog logger for tests so test output stays clean.
func nopLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// makeRow builds a RequestTelemetryRow with sensible defaults; tests
// override individual fields.
func makeRow() RequestTelemetryRow {
	return RequestTelemetryRow{
		AccountID:    uuid.New(),
		AppID:        uuid.New(),
		DeploymentID: uuid.New(),
		Route:        "GET /v1/checkout",
		Method:       "GET",
		Status:       200,
		LatencyMS:    42,
		ColdBoot:     false,
		ReceivedAt:   time.Now(),
	}
}

// requestContextWithRequestTelemetry stamps the request context with
// the keys the recorder reads in enqueueFromContext. Mirrors what
// the gateway's auth + observe middleware will stamp in production.
func requestContextWithRequestTelemetry(ctx context.Context, row RequestTelemetryRow) context.Context {
	ctx = context.WithValue(ctx, accountIDContextKey{}, row.AccountID)
	ctx = context.WithValue(ctx, appIDContextKey{}, row.AppID)
	ctx = context.WithValue(ctx, deploymentIDContextKey{}, row.DeploymentID)
	ctx = context.WithValue(ctx, routeTemplateKey{}, row.Route)
	ctx = context.WithValue(ctx, statusCodeContextKey{}, row.Status)
	ctx = context.WithValue(ctx, latencyMSContextKey{}, row.LatencyMS)
	ctx = context.WithValue(ctx, coldBootContextKey{}, row.ColdBoot)
	ctx = context.WithValue(ctx, traceIDContextKey{}, row.TraceID)
	return ctx
}

// --- recorder tests ---

func TestRequestTelemetryRecorder_MiddlewareDisabled_PassThrough(t *testing.T) {
	rec := NewRequestTelemetryRecorder(requestTelemetryConfig{Enabled: false}, nopLog())
	calls := 0
	h := rec.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.WriteHeader(http.StatusOK)
	}))
	for i := 0; i < 5; i++ {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	}
	if calls != 5 {
		t.Fatalf("expected downstream handler called 5 times, got %d", calls)
	}
	if rec.PendingCount() != 0 {
		t.Fatalf("expected zero enqueued rows when disabled, got %d", rec.PendingCount())
	}
}

func TestRequestTelemetryRecorder_RingFIFOOrder(t *testing.T) {
	rec := NewRequestTelemetryRecorder(requestTelemetryConfig{Enabled: true, RingSize: 8}, nopLog())
	for i := 0; i < 5; i++ {
		row := makeRow()
		row.LatencyMS = i // encode ordinal in latency for assertion
		rec.enqueue(row)
	}
	if rec.PendingCount() != 5 {
		t.Fatalf("expected 5 pending, got %d", rec.PendingCount())
	}
	batch := rec.DrainBatch(8)
	if len(batch) != 5 {
		t.Fatalf("expected batch of 5, got %d", len(batch))
	}
	for i, row := range batch {
		if row.LatencyMS != i {
			t.Errorf("batch[%d]: expected latency %d, got %d", i, i, row.LatencyMS)
		}
	}
	if rec.PendingCount() != 0 {
		t.Fatalf("expected 0 pending after drain, got %d", rec.PendingCount())
	}
}

func TestRequestTelemetryRecorder_RingOverflowOverwritesOldest(t *testing.T) {
	rec := NewRequestTelemetryRecorder(requestTelemetryConfig{Enabled: true, RingSize: 4}, nopLog())
	// Fill with latency 0..3 (4 rows == capacity).
	for i := 0; i < 4; i++ {
		row := makeRow()
		row.LatencyMS = i
		rec.enqueue(row)
	}
	// Overflow: 3 more rows (4..6) push out the oldest.
	for i := 4; i < 7; i++ {
		row := makeRow()
		row.LatencyMS = i
		rec.enqueue(row)
	}
	batch := rec.DrainBatch(8)
	if len(batch) != 4 {
		t.Fatalf("expected batch of 4 after overflow, got %d", len(batch))
	}
	// After overflow: latencies 3,4,5,6 (oldest 0,1,2 overwritten).
	want := []int{3, 4, 5, 6}
	for i, row := range batch {
		if row.LatencyMS != want[i] {
			t.Errorf("batch[%d]: expected latency %d, got %d", i, want[i], row.LatencyMS)
		}
	}
}

func TestRequestTelemetryRecorder_PrePickerDropsSilently(t *testing.T) {
	rec := NewRequestTelemetryRecorder(requestTelemetryConfig{Enabled: true, RingSize: 8}, nopLog())
	calls := 0
	h := rec.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.WriteHeader(http.StatusOK)
	}))
	// Three flavours of pre-picker requests:
	//   1. no context keys at all
	//   2. only account_id (no app_id)
	//   3. account_id + app_id but no route_template
	for i := 0; i < 3; i++ {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		switch i {
		case 1:
			req = req.WithContext(context.WithValue(req.Context(), accountIDContextKey{}, uuid.New()))
		case 2:
			ctx := context.WithValue(req.Context(), accountIDContextKey{}, uuid.New())
			ctx = context.WithValue(ctx, appIDContextKey{}, uuid.New())
			req = req.WithContext(ctx)
		}
		h.ServeHTTP(httptest.NewRecorder(), req)
	}
	if calls != 3 {
		t.Fatalf("expected downstream handler called 3 times, got %d", calls)
	}
	if rec.PendingCount() != 0 {
		t.Fatalf("expected zero pending (all pre-picker dropped), got %d", rec.PendingCount())
	}
}

func TestRequestTelemetryRecorder_StampsFlowThroughMiddleware(t *testing.T) {
	rec := NewRequestTelemetryRecorder(requestTelemetryConfig{Enabled: true, RingSize: 8}, nopLog())
	row := makeRow()
	row.Status = 503
	row.LatencyMS = 191
	row.ColdBoot = true
	row.TraceID = "4bf92f3577b34da6a3ce929d0e0e4736"

	h := rec.Middleware(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		// Stamp context as the gateway auth + observe would.
		stamped := requestContextWithRequestTelemetry(req.Context(), row)
		// Replace request context so the middleware sees the stamps.
		*req = *req.WithContext(stamped)
		w.WriteHeader(row.Status)
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))

	batch := rec.DrainBatch(8)
	if len(batch) != 1 {
		t.Fatalf("expected 1 row, got %d", len(batch))
	}
	got := batch[0]
	if got.Status != row.Status {
		t.Errorf("Status: got %d, want %d", got.Status, row.Status)
	}
	if got.LatencyMS != row.LatencyMS {
		t.Errorf("LatencyMS: got %d, want %d", got.LatencyMS, row.LatencyMS)
	}
	if got.ColdBoot != row.ColdBoot {
		t.Errorf("ColdBoot: got %v, want %v", got.ColdBoot, row.ColdBoot)
	}
	if got.TraceID != row.TraceID {
		t.Errorf("TraceID: got %q, want %q", got.TraceID, row.TraceID)
	}
	if got.Route != row.Route {
		t.Errorf("Route: got %q, want %q", got.Route, row.Route)
	}
	if got.Method != http.MethodGet {
		t.Errorf("Method: got %q, want GET", got.Method)
	}
	if got.AccountID != row.AccountID || got.AppID != row.AppID || got.DeploymentID != row.DeploymentID {
		t.Errorf("IDs mismatch: account=%v/%v app=%v/%v deployment=%v/%v",
			got.AccountID, row.AccountID, got.AppID, row.AppID, got.DeploymentID, row.DeploymentID)
	}
}

func TestRequestTelemetryRecorder_ConcurrentEnqueuePreservesAllRows(t *testing.T) {
	// Stress: 100 goroutines × 50 enqueues = 5000 rows. Ring is
	// sized larger than the workload so all rows should survive
	// (the drain only fires from the publisher, not here).
	rec := NewRequestTelemetryRecorder(requestTelemetryConfig{Enabled: true, RingSize: 8192}, nopLog())
	const goroutines = 100
	const perGoroutine = 50
	var wg sync.WaitGroup
	var enqueued atomic.Int64
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < perGoroutine; i++ {
				rec.enqueue(makeRow())
				enqueued.Add(1)
			}
		}()
	}
	wg.Wait()
	if enqueued.Load() != int64(goroutines*perGoroutine) {
		t.Fatalf("expected %d enqueues, got %d", goroutines*perGoroutine, enqueued.Load())
	}
	if got := rec.PendingCount(); got != goroutines*perGoroutine {
		t.Fatalf("expected pending %d, got %d", goroutines*perGoroutine, got)
	}
}

func TestRequestTelemetryRecorder_DrainBatchEmptyReturnsNil(t *testing.T) {
	rec := NewRequestTelemetryRecorder(requestTelemetryConfig{Enabled: true}, nopLog())
	if got := rec.DrainBatch(64); got != nil {
		t.Fatalf("expected nil batch from empty ring, got %v", got)
	}
}

func TestRequestTelemetryRecorder_DrainBatchZeroMaxReturnsNil(t *testing.T) {
	rec := NewRequestTelemetryRecorder(requestTelemetryConfig{Enabled: true}, nopLog())
	rec.enqueue(makeRow())
	if got := rec.DrainBatch(0); got != nil {
		t.Fatalf("expected nil batch when max=0, got %v", got)
	}
	// Row should still be in the ring (drain did not consume it).
	if rec.PendingCount() != 1 {
		t.Fatalf("expected 1 pending after zero-max drain, got %d", rec.PendingCount())
	}
}

// --- publisher tests ---

// fakeShip captures every batch the publisher ships and lets the
// test inject a per-call error.
type fakeShip struct {
	mu      sync.Mutex
	batches [][]RequestTelemetryRow
	err     error // returned by every Ship call
	calls   int
}

func (f *fakeShip) Ship(_ context.Context, rows []RequestTelemetryRow) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil {
		return f.err
	}
	// Copy the slice so later recorder mutations don't leak.
	cp := make([]RequestTelemetryRow, len(rows))
	copy(cp, rows)
	f.batches = append(f.batches, cp)
	return nil
}

func (f *fakeShip) TotalRows() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, b := range f.batches {
		n += len(b)
	}
	return n
}

func TestRequestTelemetryPublisher_ShipSuccessIncrementsCounter(t *testing.T) {
	rec := NewRequestTelemetryRecorder(requestTelemetryConfig{Enabled: true, RingSize: 16}, nopLog())
	ship := &fakeShip{}
	pub := NewRequestTelemetryPublisher(requestTelemetryPublisherConfig{
		Enabled:        true,
		FlushInterval:  10 * time.Millisecond,
		FlushBatchSize: 8,
		MaxRetries:     1,
	}, rec, ship.Ship, nopLog())
	pub.Start(context.Background())
	defer pub.Stop()

	for i := 0; i < 5; i++ {
		rec.enqueue(makeRow())
	}
	// Give the goroutine one tick to drain.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if ship.TotalRows() == 5 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := ship.TotalRows(); got != 5 {
		t.Fatalf("expected ship to receive 5 rows, got %d", got)
	}
	if got := pub.ShippedTotal(); got != 5 {
		t.Fatalf("expected ShippedTotal=5, got %d", got)
	}
	if got := pub.DroppedTotal(); got != 0 {
		t.Fatalf("expected DroppedTotal=0, got %d", got)
	}
	if rec.PendingCount() != 0 {
		t.Fatalf("expected ring drained, got %d pending", rec.PendingCount())
	}
}

func TestRequestTelemetryPublisher_RetriesOnTransientErrorThenSucceeds(t *testing.T) {
	rec := NewRequestTelemetryRecorder(requestTelemetryConfig{Enabled: true, RingSize: 16}, nopLog())
	ship := &flakyShip{failuresBeforeSuccess: 2}
	pub := NewRequestTelemetryPublisher(requestTelemetryPublisherConfig{
		Enabled:        true,
		FlushInterval:  10 * time.Millisecond,
		FlushBatchSize: 8,
		MaxRetries:     5, // tolerate 2 failures then succeed on attempt 3
	}, rec, ship.Ship, nopLog())
	pub.Start(context.Background())
	defer pub.Stop()

	rec.enqueue(makeRow())
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if pub.ShippedTotal() == 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := pub.ShippedTotal(); got != 1 {
		t.Fatalf("expected ShippedTotal=1 after retry-then-success, got %d (calls=%d)", got, ship.calls)
	}
	if got := pub.DroppedTotal(); got != 0 {
		t.Fatalf("expected DroppedTotal=0, got %d", got)
	}
}

func TestRequestTelemetryPublisher_ExhaustsRetriesThenDrops(t *testing.T) {
	rec := NewRequestTelemetryRecorder(requestTelemetryConfig{Enabled: true, RingSize: 16}, nopLog())
	ship := &alwaysFailShip{err: errors.New("apid unreachable")}
	pub := NewRequestTelemetryPublisher(requestTelemetryPublisherConfig{
		Enabled:        true,
		FlushInterval:  10 * time.Millisecond,
		FlushBatchSize: 8,
		MaxRetries:     2, // fail twice ⇒ drop
	}, rec, ship.Ship, nopLog())
	pub.Start(context.Background())
	defer pub.Stop()

	for i := 0; i < 3; i++ {
		rec.enqueue(makeRow())
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if pub.DroppedTotal() == 3 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := pub.DroppedTotal(); got != 3 {
		t.Fatalf("expected DroppedTotal=3, got %d (calls=%d)", got, ship.calls)
	}
	if got := pub.ShippedTotal(); got != 0 {
		t.Fatalf("expected ShippedTotal=0, got %d", got)
	}
}

func TestRequestTelemetryPublisher_WakeDrainsImmediately(t *testing.T) {
	// Set a deliberately long flush interval so the only way the
	// rows ship before the test deadline is via Wake().
	rec := NewRequestTelemetryRecorder(requestTelemetryConfig{Enabled: true, RingSize: 16}, nopLog())
	ship := &fakeShip{}
	pub := NewRequestTelemetryPublisher(requestTelemetryPublisherConfig{
		Enabled:        true,
		FlushInterval:  10 * time.Second, // 10s — Wake must shortcut
		FlushBatchSize: 8,
		MaxRetries:     1,
	}, rec, ship.Ship, nopLog())
	pub.Start(context.Background())
	defer pub.Stop()

	rec.enqueue(makeRow())
	pub.Wake()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if ship.TotalRows() == 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := ship.TotalRows(); got != 1 {
		t.Fatalf("expected ship to receive 1 row via Wake, got %d", got)
	}
}

func TestRequestTelemetryPublisher_StopDrainsFinalBatch(t *testing.T) {
	// Long flush interval + enqueue + Stop. The final drain in
	// run() must ship the rows synchronously before doneCh closes.
	rec := NewRequestTelemetryRecorder(requestTelemetryConfig{Enabled: true, RingSize: 16}, nopLog())
	ship := &fakeShip{}
	pub := NewRequestTelemetryPublisher(requestTelemetryPublisherConfig{
		Enabled:        true,
		FlushInterval:  10 * time.Second,
		FlushBatchSize: 8,
		MaxRetries:     1,
	}, rec, ship.Ship, nopLog())
	pub.Start(context.Background())

	for i := 0; i < 4; i++ {
		rec.enqueue(makeRow())
	}
	pub.Stop() // synchronous: must ship before returning

	if got := ship.TotalRows(); got != 4 {
		t.Fatalf("expected ship to receive 4 rows on Stop, got %d", got)
	}
	if got := pub.ShippedTotal(); got != 4 {
		t.Fatalf("expected ShippedTotal=4, got %d", got)
	}
}

func TestRequestTelemetryPublisher_NilShipDropsRows(t *testing.T) {
	rec := NewRequestTelemetryRecorder(requestTelemetryConfig{Enabled: true, RingSize: 16}, nopLog())
	pub := NewRequestTelemetryPublisher(requestTelemetryPublisherConfig{
		Enabled:        true,
		FlushInterval:  10 * time.Millisecond,
		FlushBatchSize: 8,
		MaxRetries:     1,
	}, rec, nil, nopLog())
	pub.Start(context.Background())
	defer pub.Stop()

	for i := 0; i < 3; i++ {
		rec.enqueue(makeRow())
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if pub.DroppedTotal() == 3 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := pub.DroppedTotal(); got != 3 {
		t.Fatalf("expected DroppedTotal=3 (nil ship drops rows), got %d", got)
	}
}

func TestCollapseRequestTelemetry_PassThroughForPRA(t *testing.T) {
	// PR-A: collapseRequestTelemetry is a no-op. PR-B will replace
	// it with an aggregate. Pin the behavior so future changes
	// don't silently regress PR-A's contract.
	rows := []RequestTelemetryRow{makeRow(), makeRow(), makeRow()}
	got := collapseRequestTelemetry(rows)
	if len(got) != len(rows) {
		t.Fatalf("expected pass-through len %d, got %d", len(rows), len(got))
	}
	for i := range rows {
		if got[i].Route != rows[i].Route {
			t.Errorf("row %d: route changed across collapse", i)
		}
	}
}

// --- helpers for flaky/always-fail ship ---

type flakyShip struct {
	mu                    sync.Mutex
	failuresBeforeSuccess int
	calls                 int
}

func (f *flakyShip) Ship(_ context.Context, rows []RequestTelemetryRow) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.calls <= f.failuresBeforeSuccess {
		return errors.New("transient")
	}
	return nil
}

type alwaysFailShip struct {
	mu    sync.Mutex
	err   error
	calls int
}

func (a *alwaysFailShip) Ship(_ context.Context, _ []RequestTelemetryRow) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.calls++
	return a.err
}