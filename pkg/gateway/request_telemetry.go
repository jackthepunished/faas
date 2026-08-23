// request_telemetry.go — the request-hot-path component of the
// production debugger (ADR-127).
//
// Sits alongside the gateway's Handler.observe exit funnel
// (pkg/gateway/handler.go:5456) and enqueues one row per
// gateway-served request. Unlike app_errors_recorder.go (which only
// fires on 4xx/5xx), every request lands here — the data plane that
// backs "did v81 make it slow?".
//
// Cardinality discipline is NOT the recorder's job in PR-A — the
// publisher (request_telemetry_publisher.go) is where the
// (app_id, deployment_id, route, status, minute) dedupe collapses
// burst traffic to a representative row + count. Doing it in the
// publisher keeps the recorder's hot path to O(1) under one mutex.
//
// The recorder NEVER opens a Postgres connection (CLAUDE.md
// ownership: apid is the sole writer). It hands rows to the
// publisher (request_telemetry_publisher.go) which dials apid via
// a unix-socket gRPC IncrementRequestTelemetry streaming RPC —
// same shape as app_errors_publisher.go.
//
// Concurrency: every method is safe under concurrent calls from
// many request goroutines. ringMu guards ring + head + len. The
// lock is held for O(1) work per row on the hot path (enqueue) and
// O(max) on the publisher drain path (DrainBatch).

package gateway

import (
	"context"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/google/uuid"
)

// RequestTelemetryRow is the unit of work shared between recorder +
// publisher + apid receiver. Field names match the
// request_telemetry table columns in migrations/00387_ verbatim so
// the apid gRPC handler can pass them straight to sqlc params.
type RequestTelemetryRow struct {
	AccountID    uuid.UUID
	AppID        uuid.UUID
	DeploymentID uuid.UUID
	Route        string // route template (e.g. "GET /v1/users/{id}"), NOT expanded URL
	Method       string // closed enum: GET/POST/PUT/PATCH/DELETE/HEAD/OPTIONS
	Status       int    // 100-599 (CHECK constraint)
	LatencyMS    int    // wall-clock from Handler.ServeHTTP entry to observe exit
	ColdBoot     bool   // true when this request woke a fresh instance
	TraceID      string // W3C trace-id hex (32 chars); "" when unset
	ReceivedAt   time.Time
}

// requestTelemetryConfig bundles the knobs the recorder reads at
// boot. Defaults set via setRequestTelemetryDefaults.
type requestTelemetryConfig struct {
	// Enabled is the kill-switch (FAAS_REQUEST_TELEMETRY_ENABLED).
	// When false, the middleware is a no-op pass-through and the
	// publisher goroutine does not start.
	Enabled bool

	// RingSize caps the in-process ringbuffer. Past the cap,
	// the oldest row is overwritten (next publisher tick drops
	// further rows if the channel stays full). Sized to absorb
	// a 1k-RPS app at 1s flush cadence (4096 = 4× headroom).
	RingSize int

	// Now is injectable for tests. nil ⇒ time.Now.
	Now func() time.Time
}

func (c *requestTelemetryConfig) setDefaults() {
	if c.RingSize == 0 {
		c.RingSize = 4096
	}
	if c.Now == nil {
		c.Now = time.Now
	}
}

// requestTelemetryRecorder is the in-process hot-path component.
// Construct via NewRequestTelemetryRecorder, install as middleware
// via Middleware, and have the publisher call DrainBatch on its
// tick (5s default per app_errors_publisher.go:50).
type requestTelemetryRecorder struct {
	cfg requestTelemetryConfig
	log *slog.Logger

	// ringMu guards ring + head + len. The recorder holds the
	// lock for O(1) work per row; the publisher holds it for the
	// duration of DrainBatch (typically < 1ms for a 256-row
	// batch).
	ringMu sync.Mutex
	ring   []RequestTelemetryRow
	head   int
	len    int
}

// NewRequestTelemetryRecorder wires the recorder. The publisher
// obtains a reference to this struct and calls DrainBatch on its
// tick (see request_telemetry_publisher.go).
func NewRequestTelemetryRecorder(cfg requestTelemetryConfig, log *slog.Logger) *requestTelemetryRecorder {
	cfg.setDefaults()
	return &requestTelemetryRecorder{
		cfg:  cfg,
		log:  log,
		ring: make([]RequestTelemetryRow, cfg.RingSize),
	}
}

// Middleware returns the http.Handler middleware the gateway
// installs in front of (or alongside) the request handler. When
// cfg.Enabled is false, the middleware is a pass-through (the
// kill-switch path).
//
// Reads the (account_id, app_id, deployment_id, route_template)
// context keys populated by the gateway's auth middleware — same
// keys app_errors_recorder.go:475-478 uses. When any of the four
// are absent (e.g. a request that failed before the picker
// resolved the app), the recorder silently drops the row.
func (r *requestTelemetryRecorder) Middleware(next http.Handler) http.Handler {
	if !r.cfg.Enabled {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		next.ServeHTTP(w, req)
		// After the handler returns, enqueue the row from the
		// resolved context. The Handler.observe funnel (line
		// 5456) is the canonical exit — but the observe funnel
		// already captures status + elapsed + app + deployment;
		// the middleware path is the fallback for handlers
		// that don't go through observe (e.g. control-plane
		// /healthz).
		r.enqueueFromContext(req.Context(), req)
	})
}

// enqueueFromContext builds a row from the resolved request
// context and appends to the ringbuffer. Errors are silent
// (logged at debug) — the request hot path cannot fail.
func (r *requestTelemetryRecorder) enqueueFromContext(ctx context.Context, req *http.Request) {
	accountID, _ := reqContextUUID(ctx, accountIDContextKey{})
	appID, _ := reqContextUUID(ctx, appIDContextKey{})
	deploymentID, _ := reqContextUUID(ctx, deploymentIDContextKey{})
	route, _ := ctx.Value(routeTemplateKey{}).(string)
	if accountID == uuid.Nil || appID == uuid.Nil || route == "" {
		// Pre-picker path (auth failure, healthz, 404 before
		// app resolution). Drop silently — there's no app to
		// attribute the row to.
		return
	}
	// deploymentID stays uuid.Nil when not resolved; the column
	// is NOT NULL but allows the zero UUID (no SQL constraint
	// beyond that).
	status, _ := ctx.Value(statusCodeContextKey{}).(int)
	latencyMS, _ := ctx.Value(latencyMSContextKey{}).(int)
	coldBoot, _ := ctx.Value(coldBootContextKey{}).(bool)
	traceID, _ := ctx.Value(traceIDContextKey{}).(string)

	row := RequestTelemetryRow{
		AccountID:    accountID,
		AppID:        appID,
		DeploymentID: deploymentID,
		Route:        route,
		Method:       req.Method,
		Status:       status,
		LatencyMS:    latencyMS,
		ColdBoot:     coldBoot,
		TraceID:      traceID,
		ReceivedAt:   r.cfg.Now(),
	}
	r.enqueue(row)
}

// enqueue is the single entry point for the ringbuffer append.
// O(1) under ringMu. Burst traffic overwrites the oldest row
// past the cap — the publisher's drain is the back-pressure
// signal (it should drain faster than the ring fills).
func (r *requestTelemetryRecorder) enqueue(row RequestTelemetryRow) {
	r.ringMu.Lock()
	if r.len < len(r.ring) {
		// ring not full — append at tail
		tail := (r.head + r.len) % len(r.ring)
		r.ring[tail] = row
		r.len++
	} else {
		// ring full — overwrite head, advance head by 1
		r.ring[r.head] = row
		r.head = (r.head + 1) % len(r.ring)
	}
	r.ringMu.Unlock()
}

// DrainBatch returns up to max rows from the head of the ringbuffer
// and removes them. Returns an empty slice when the ring is empty.
// The publisher calls this on its tick (5s default) and ships the
// batch to apid via gRPC.
func (r *requestTelemetryRecorder) DrainBatch(max int) []RequestTelemetryRow {
	if max <= 0 {
		return nil
	}
	r.ringMu.Lock()
	defer r.ringMu.Unlock()
	if r.len == 0 {
		return nil
	}
	n := r.len
	if n > max {
		n = max
	}
	out := make([]RequestTelemetryRow, n)
	for i := 0; i < n; i++ {
		out[i] = r.ring[(r.head+i)%len(r.ring)]
	}
	// Advance head by n, shrink len by n. The ring slot positions
	// stay allocated (no realloc) — the publisher will overwrite
	// them on the next enqueue.
	r.head = (r.head + n) % len(r.ring)
	r.len -= n
	return out
}

// PendingCount returns the number of rows currently in the
// ringbuffer waiting for the publisher. Read-only; useful for
// /metrics + tests.
func (r *requestTelemetryRecorder) PendingCount() int {
	r.ringMu.Lock()
	defer r.ringMu.Unlock()
	return r.len
}

// RingCapacity returns the configured ringbuffer size. Read-only;
// useful for /metrics + tests.
func (r *requestTelemetryRecorder) RingCapacity() int {
	return len(r.ring)
}

// RecordFromObserve is the seam Handler.observe uses to enqueue
// a row at the gateway's single exit funnel
// (pkg/gateway/handler.go:5456). It is the explicit-row variant of
// the Middleware path: the caller (observe) has already resolved
// the status + elapsed + cold + target from its arguments, so it
// passes the row in pre-built rather than letting enqueueFromContext
// re-read context keys.
//
// Safe under concurrent calls — goes through the same enqueue() as
// the middleware path.
func (r *requestTelemetryRecorder) RecordFromObserve(row RequestTelemetryRow) {
	r.enqueue(row)
}

// --- context-key helpers for the ServeHTTP-side stamping ---

// withAppAndAccount stamps account_id + app_id onto ctx. Called
// once per request from Handler.ServeHTTP at the `haveApp:`
// label (handler.go:4601) so observe can read both via the
// accountIDContextKey / appIDContextKey keys below. Mirrors
// withRouteLabel's pattern in observability.go.
//
// Returns the original ctx when both ids are empty — pre-picker
// paths (auth failure, no host) do not stamp anything, so observe
// sees an absent key and skips the row.
func withAppAndAccount(r *http.Request, accountID, appID uuid.UUID) *http.Request {
	if r == nil {
		return r
	}
	if accountID == uuid.Nil || appID == uuid.Nil {
		return r
	}
	ctx := context.WithValue(r.Context(), accountIDContextKey{}, accountID)
	ctx = context.WithValue(ctx, appIDContextKey{}, appID)
	return r.WithContext(ctx)
}

// withRouteTemplate stamps the route template onto ctx so observe
// can stamp a closed-enum route label into the row. Mirrors
// withAppAndAccount. Used by the post-pick rule that derives the
// per-request route label (handler.go:4613-4617).
func withRouteTemplate(r *http.Request, template string) *http.Request {
	if r == nil || template == "" {
		return r
	}
	return r.WithContext(context.WithValue(r.Context(), routeTemplateKey{}, template))
}

// accountIDFromContext reads the account_id stamped on ctx.
// Returns uuid.Nil when absent.
func accountIDFromContext(ctx context.Context) uuid.UUID {
	v, _ := reqContextUUID(ctx, accountIDContextKey{})
	return v
}

// appIDFromContext reads the app_id stamped on ctx.
// Returns uuid.Nil when absent.
func appIDFromContext(ctx context.Context) uuid.UUID {
	v, _ := reqContextUUID(ctx, appIDContextKey{})
	return v
}

// routeTemplateFromContext reads the route template stamped on ctx.
// Returns "" when absent.
func routeTemplateFromContext(ctx context.Context) string {
	v, _ := ctx.Value(routeTemplateKey{}).(string)
	return v
}

// --- context keys for the request-side metadata ---

// The gateway's existing auth middleware populates these context
// keys for the request_telemetry hot path. Mirrors the keys
// populated for app_errors_recorder.go (cmd/gatewayd-internal/
// app_errors_recorder.go:475-478).
//
// accountIDContextKey / appIDContextKey / deploymentIDContextKey
// are uuid.UUID; routeTemplateKey is string; statusCodeContextKey /
// latencyMSContextKey are int; coldBootContextKey is bool;
// traceIDContextKey is string.
//
// The Handler.observe exit funnel (pkg/gateway/handler.go:5456) is
// the canonical site that populates statusCodeContextKey +
// latencyMSContextKey + coldBootContextKey. PR-A's
// handler-observe wiring lands the stamping.
type (
	accountIDContextKey    struct{}
	appIDContextKey        struct{}
	deploymentIDContextKey struct{}
	routeTemplateKey       struct{}
	statusCodeContextKey   struct{}
	latencyMSContextKey    struct{}
	coldBootContextKey     struct{}
	traceIDContextKey      struct{}
)

// reqContextUUID reads a uuid.UUID from req.Context() by the given
// key. Returns uuid.Nil + false when absent.
func reqContextUUID(ctx context.Context, key any) (uuid.UUID, bool) {
	v, ok := ctx.Value(key).(uuid.UUID)
	return v, ok
}
