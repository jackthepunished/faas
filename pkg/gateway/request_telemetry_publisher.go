// request_telemetry_publisher.go — the out-of-process shipping loop
// of the production debugger (ADR-127).
//
// Runs in a single goroutine in gatewayd-internal (NOT in the
// request hot path). Every FlushInterval it calls
// recorder.DrainBatch(FlushBatchSize) and ships the rows to apid
// via the ShipFn. The ShipFn is wired at daemon boot; in production
// it's the unix-socket gRPC IncrementRequestTelemetry streaming
// client (added in PR-B alongside the apid receiver); in unit
// tests it's a recorder into a slice.
//
// Back-pressure shape (mirrors app_errors_publisher.go:147-278):
// - If the gRPC stream is blocked, the publisher drops rows with a
//   warning log + a dropped_total counter. The recorder's ring
//   buffer protects the hot path either way.
// - On error from apid (transient), retry with exponential backoff
//   up to MaxRetries, then drop the batch.
//
// Cardinality discipline lands HERE, not in the recorder. Before
// shipping, the publisher collapses burst traffic by
// (app_id, deployment_id, route, status, minute_bucket) to one
// representative row + count — so a 1k-RPS endpoint at 100%
// sampling lands as ~1 row/minute to Postgres instead of ~60k.

package gateway

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

// requestTelemetryPublisherConfig bundles the knobs the publisher
// reads at boot. Defaults set via setPublisherDefaults.
type requestTelemetryPublisherConfig struct {
	// Enabled is the kill-switch. Mirrors the recorder's Enabled
	// (they're tied — both flip on FAAS_REQUEST_TELEMETRY_ENABLED).
	// When false, the publisher goroutine does not start.
	Enabled bool

	// FlushInterval is how often the goroutine drains the
	// recorder. 5s matches app_errors_publisher.go default.
	FlushInterval time.Duration

	// FlushBatchSize caps how many rows the publisher pulls
	// per tick. 256 matches app_errors_publisher.go default.
	FlushBatchSize int

	// MaxRetries caps per-tick retries on transient apid
	// errors before dropping the batch. 3 matches
	// app_errors_publisher.go default.
	MaxRetries int

	// Now is injectable for tests. nil ⇒ time.Now.
	Now func() time.Time
}

func (c *requestTelemetryPublisherConfig) setDefaults() {
	if c.FlushInterval == 0 {
		c.FlushInterval = 5 * time.Second
	}
	if c.FlushBatchSize == 0 {
		c.FlushBatchSize = 256
	}
	if c.MaxRetries == 0 {
		c.MaxRetries = 3
	}
	if c.Now == nil {
		c.Now = time.Now
	}
}

// ShipFn is the contract the daemon wires up at boot. Production
// implementation: a gRPC streaming client that opens an
// IncrementRequestTelemetry RPC against apid's unix socket and
// streams the rows. Test implementation: appends the rows to a
// slice for assertion.
//
// Returning a non-nil error tells the publisher to retry with
// exponential backoff. After MaxRetries retries, the batch is
// dropped with a warn log + droppedTotal counter increment.
type ShipFn func(ctx context.Context, rows []RequestTelemetryRow) error

// requestTelemetryPublisher is the goroutine-owning publisher.
// Construct via NewRequestTelemetryPublisher, then call Start
// to launch the goroutine and Stop to halt it (Stop drains any
// pending rows synchronously).
type requestTelemetryPublisher struct {
	cfg      requestTelemetryPublisherConfig
	recorder *requestTelemetryRecorder
	ship     ShipFn
	log      *slog.Logger

	// droppedTotal counts rows dropped due to ship errors after
	// exhausting MaxRetries. Surfaced via /metrics + tests.
	// atomic.Int64 — read from /metrics goroutine + write from
	// publisher goroutine.
	droppedTotal atomic.Int64

	// shippedTotal counts rows successfully shipped. Surfaced
	// via /metrics + tests.
	shippedTotal atomic.Int64

	// wakeCh is a buffered channel (cap 1) used to wake the
	// publisher immediately when the recorder is in danger of
	// overflowing. Producers call non-blocking send; the loop
	// wakes early.
	wakeCh chan struct{}

	// startOnce / stopOnce / stopCh guard lifecycle. Start must
	// be called exactly once; Stop must be called exactly once.
	startOnce sync.Once
	stopOnce  sync.Once
	stopCh    chan struct{}
	doneCh    chan struct{}
}

// NewRequestTelemetryPublisher wires a publisher. The recorder +
// ship callback must already be constructed; the publisher takes
// references and starts the goroutine in Start.
func NewRequestTelemetryPublisher(cfg requestTelemetryPublisherConfig, recorder *requestTelemetryRecorder, ship ShipFn, log *slog.Logger) *requestTelemetryPublisher {
	cfg.setDefaults()
	return &requestTelemetryPublisher{
		cfg:      cfg,
		recorder: recorder,
		ship:     ship,
		log:      log,
		wakeCh:   make(chan struct{}, 1),
		stopCh:   make(chan struct{}),
		doneCh:   make(chan struct{}, 1),
	}
}

// Start launches the publisher goroutine. Safe to call once;
// subsequent calls are no-ops.
func (p *requestTelemetryPublisher) Start(ctx context.Context) {
	p.startOnce.Do(func() {
		go p.run(ctx)
	})
}

// Stop signals the publisher goroutine to halt, drains the
// recorder one final time, and waits for the goroutine to exit.
// Safe to call once; subsequent calls are no-ops.
func (p *requestTelemetryPublisher) Stop() {
	p.stopOnce.Do(func() {
		close(p.stopCh)
		<-p.doneCh
	})
}

// Wake nudges the publisher to drain immediately (non-blocking).
// Producers (recorder-side) call Wake when PendingCount crosses
// the half-full threshold. The send on wakeCh is non-blocking —
// if the channel is already full, the existing wake is sufficient.
func (p *requestTelemetryPublisher) Wake() {
	select {
	case p.wakeCh <- struct{}{}:
	default:
	}
}

// DroppedTotal returns rows dropped due to ship errors after
// exhausting retries. Read-only.
func (p *requestTelemetryPublisher) DroppedTotal() int64 {
	return p.droppedTotal.Load()
}

// ShippedTotal returns rows successfully shipped. Read-only.
func (p *requestTelemetryPublisher) ShippedTotal() int64 {
	return p.shippedTotal.Load()
}

// run is the goroutine loop. Drains on FlushInterval (or on Wake)
// until stopCh closes. Drains one final batch synchronously on
// the way out so Stop() returns "nothing left to ship".
func (p *requestTelemetryPublisher) run(ctx context.Context) {
	defer func() {
		// Final drain on the way out so Stop() blocks until the
		// last in-flight rows have been attempted.
		p.tick(ctx)
		p.doneCh <- struct{}{}
	}()

	ticker := time.NewTicker(p.cfg.FlushInterval)
	defer ticker.Stop()

	for {
		select {
		case <-p.stopCh:
			return
		case <-ticker.C:
			p.tick(ctx)
		case <-p.wakeCh:
			// Wake-on-near-full. Tick once, then go back to
			// sleeping on the FlushInterval ticker.
			p.tick(ctx)
		}
	}
}

// tick drains one batch from the recorder, collapses it by
// (app, deployment, route, status, minute), and ships via ShipFn.
// Errors are logged + retried with exponential backoff up to
// MaxRetries; final failure increments droppedTotal.
func (p *requestTelemetryPublisher) tick(ctx context.Context) {
	if p.ship == nil {
		// ship not wired (test-only or boot race); drop the
		// drained rows on the floor + log once.
		rows := p.recorder.DrainBatch(p.cfg.FlushBatchSize)
		if len(rows) > 0 {
			p.droppedTotal.Add(int64(len(rows)))
		}
		return
	}
	rows := p.recorder.DrainBatch(p.cfg.FlushBatchSize)
	if len(rows) == 0 {
		return
	}
	collapsed := collapseRequestTelemetry(rows)

	var lastErr error
	for attempt := 0; attempt < p.cfg.MaxRetries; attempt++ {
		if attempt > 0 {
			// Exponential backoff: 100ms, 200ms, 400ms...
			backoff := time.Duration(1<<attempt) * 100 * time.Millisecond
			select {
			case <-ctx.Done():
				p.droppedTotal.Add(int64(len(collapsed)))
				return
			case <-time.After(backoff):
			}
		}
		lastErr = p.ship(ctx, collapsed)
		if lastErr == nil {
			p.shippedTotal.Add(int64(len(collapsed)))
			return
		}
		// Transient — log + retry.
		p.log.Warn("request telemetry ship failed; retrying",
			slog.Int("attempt", attempt+1),
			slog.Int("batch_size", len(collapsed)),
			slog.Any("error", lastErr))
	}
	// Out of retries — drop the batch.
	p.droppedTotal.Add(int64(len(collapsed)))
	p.log.Warn("request telemetry ship exhausted retries; dropping batch",
		slog.Int("batch_size", len(collapsed)),
		slog.Any("last_error", lastErr))
}

// collapseRequestTelemetry collapses burst traffic into one row
// per (app_id, deployment_id, route, method, status, minute_bucket)
// with a Count field. PR-A's sqlc schema does not yet have a
// count column; this function is the staging point for PR-B's
// INSERT shape (PR-B will add `count INT` to the schema).
//
// For PR-A the collapse is a no-op — every row ships verbatim —
// because the apid INSERT statement expects one row per call.
// PR-B will replace this with a real aggregate + a new sqlc
// INSERT that takes (..., count).
func collapseRequestTelemetry(rows []RequestTelemetryRow) []RequestTelemetryRow {
	// PR-A: pass-through. PR-B will replace with aggregate.
	return rows
}