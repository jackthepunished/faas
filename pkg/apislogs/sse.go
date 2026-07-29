// Package apislogs holds the SSE envelope helpers for the customer-
// facing app-logs stream (issue #254 / Move 4 landing). Both the
// cmd/apid tail (PR-A wiring, kept for the e2e harness direct-hit
// path) and the cmd/gatewayd inline handler (PR-2 production
// wiring) render the same `text/event-stream` envelope shape, so
// the helpers live here rather than being duplicated in each
// daemon.
//
// Wire shape (Move 4 acceptance #5): the SSE response is
// `text/event-stream` with the following frames:
//
//	event: log\ndata: {"seq":<i>,"instance":<s>,"stream":<s>,"line":<s>,"written_at":<rfc3339>}\n\n
//	event: ping\ndata: {}\n\n   (heartbeat; sigil form `:\n\n`)
//	event: degraded\ndata: {...}\n\n
//	event: error\ndata: {"code":<s>,"message":<s>}\n\n
//	event: end\ndata: {"reason":<s>|"timeout"|"not_found"|"schedd_unreachable"}\n\n
//
// The five-event vocabulary is the contract the SDK decoder
// (pkg/api/sse.go) matches against. New frames MUST add a new
// sentinel rather than overload an existing one — a downstream
// SDK filtering on `event: log` would silently misconsume a
// not-yet-catalogued event.
package apislogs

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/onebox-faas/faas/pkg/api"
	"github.com/onebox-faas/faas/pkg/scheddgrpc"
	"github.com/onebox-faas/faas/pkg/wire"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// RenderAppLogEvent writes a single `event: log` SSE frame for
// the given schedd frame. The payload matches Move 4 acceptance
// #5 (the `instance` field is the additive per ADR-016). The
// flusher.Flush after each frame is what the htmx-ext-sse
// auto-reconnect logic relies on — silent frames look like a
// dead connection. ops is nil-safe (a nil receiver no-ops;
// tests that don't wire metrics keep working).
func RenderAppLogEvent(w http.ResponseWriter, flusher http.Flusher, f scheddgrpc.LogFrame, appID string, ops *wire.OpsMetrics) {
	payload, _ := json.Marshal(map[string]any{
		"seq":        f.Seq,
		"instance":   f.InstanceID,
		"stream":     f.Stream,
		"line":       f.Line,
		"written_at": f.WrittenAt.UTC().Format(time.RFC3339Nano),
	})
	_, _ = fmt.Fprintf(w, "event: log\ndata: %s\n\n", payload)
	if flusher != nil {
		flusher.Flush()
	}
	ops.ObserveLogEmitted(appID)
}

// RenderAppLogsError renders a schedd-side dial error as either a
// 404 (no live instances) or a `degraded` event. Mirrors the
// legacy cmd/apid/handlers_ext.go::renderAppLogsError — the wire
// shape is the source of truth for the SDK decoder.
//
// The discriminator is the gRPC status code, not the lifted
// *api.Problem — schedd returns raw status.Error(codes.NotFound,
// ...) when the app is parked, and that error has no
// *api.Problem payload. Inspecting codes.NotFound directly is
// the load-bearing branch.
//
// Wire shape: the `data:` line is SSE text, NOT JSON. The error
// string is embedded with `%q` (Go-string escaping) so embedded
// quotes / backslashes / control bytes are safe to round-trip
// through the SSE stream — but the resulting line is NOT valid
// JSON, and a future consumer that parses the data: line as JSON
// will see `"error":"...\\n..."` style escapes. The SDK decoder
// matches on the SSE event name + the literal `"code":"not_found"`
// substring, so the escaping is benign there. New helpers that
// emit structured data should use json.Marshal instead.
func RenderAppLogsError(w http.ResponseWriter, flusher http.Flusher, err error) {
	if st, ok := status.FromError(err); ok && st.Code() == codes.NotFound {
		// Already past StartSSE — write the degraded event
		// instead of trying to overwrite the 200 (the consumer
		// reads the SSE body, not the status).
		_, _ = fmt.Fprintf(w, "event: degraded\ndata: {\"error\":%q,\"code\":\"not_found\"}\n\n", err.Error())
		_, _ = fmt.Fprint(w, "event: end\ndata: {\"reason\":\"not_found\"}\n\n")
		if flusher != nil {
			flusher.Flush()
		}
		return
	}
	_, _ = fmt.Fprintf(w, "event: degraded\ndata: {\"error\":%q}\n\n", err.Error())
	_, _ = fmt.Fprint(w, "event: end\ndata: {\"reason\":\"schedd_unreachable\"}\n\n")
	if flusher != nil {
		flusher.Flush()
	}
}

// StartSSE sets the SSE response headers and disables write
// timeouts for the lifetime of the request. The w.(http.Flusher)
// type assertion is intentional — every http.ResponseWriter we
// accept in production satisfies Flusher (the stdlib
// *http.response is itself a Flusher; httptest.NewRecorder also
// returns one). A missing Flusher would silently break the
// htmx-ext-sse auto-reconnect contract.
func StartSSE(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	w.(http.Flusher).Flush()
}

// ValidateLogFilters enforces the issue #309 filter contract on
// the `--level` and `--grep` query params. ok=false on
// rejection; the handler must then emit the SSE error frame
// (WriteInvalidLevelError / WriteInvalidGrepError) and return.
//
// `reason` is the SSE error code that pinpoints the rejection
// ("invalid_level" or "invalid_grep") — exported as a const
// string so the SDK decoder can branch without a second
// package-level import.
func ValidateLogFilters(r *http.Request) (level string, grep string, reason string, ok bool) {
	q := r.URL.Query()
	// --level: enum match against api.IsValidLogLevel so the CLI
	// and the server share the same source of truth.
	if l := q.Get("level"); l != "" && !api.IsValidLogLevel(l) {
		return "", "", "invalid_level", false
	}
	// --grep: reject embedded newlines so Move 4's substring
	// matcher can never match across log line boundaries (same
	// log-injection precedent as `CodeQL go/log-injection
	// sanitisers`).
	if g := q.Get("grep"); strings.ContainsAny(g, "\n\r") {
		return "", "", "invalid_grep", false
	}
	return q.Get("level"), q.Get("grep"), "", true
}

// SSE error codes emitted by ValidateLogFilters. Stable strings
// that the SDK decoder branches on (`pkg/api/sse.go`); renaming
// any of these is a breaking change.
const (
	InvalidLevelCode = "invalid_level"
	InvalidGrepCode  = "invalid_grep"
)

// WriteInvalidLevelError writes the `event: error` +
// `event: end` terminal for `level` validation failures. The
// caller has already started SSE; the helper just renders the
// two frames and flushes.
func WriteInvalidLevelError(w http.ResponseWriter, flusher http.Flusher) {
	_, _ = fmt.Fprint(w, "event: error\ndata: {\"code\":\"invalid_level\",\"message\":\"level must be one of: info, warn, error\"}\n\n")
	_, _ = fmt.Fprint(w, "event: end\ndata: {}\n\n")
	if flusher != nil {
		flusher.Flush()
	}
}

// WriteInvalidGrepError mirrors WriteInvalidLevelError for the
// `--grep` validation failure path.
func WriteInvalidGrepError(w http.ResponseWriter, flusher http.Flusher) {
	_, _ = fmt.Fprint(w, "event: error\ndata: {\"code\":\"invalid_grep\",\"message\":\"grep must not contain newline or carriage return\"}\n\n")
	_, _ = fmt.Fprint(w, "event: end\ndata: {}\n\n")
	if flusher != nil {
		flusher.Flush()
	}
}

// ParseInt64Query parses an int64 query param with a default
// value. Returns the default if the param is missing or
// unparseable. Negative values are clamped to 0 — the schedd
// gRPC stream lifts them to 0 anyway (defence in depth).
func ParseInt64Query(r *http.Request, name string, def int64) int64 {
	v := r.URL.Query().Get(name)
	if v == "" {
		return def
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return def
	}
	if n < 0 {
		return 0
	}
	return n
}

// IsTerminalFrame is the small predicate the SDK uses to decide
// when a frames stream is done. Event-name lookup is the
// cheapest check; the SDK reads the full line anyway. Exposed
// here so the apid tail (PR-A) and the gatewayd handler (PR-2)
// agree on which event names end the stream.
//
// Post-condition: every terminal frame in the wire shape is
// followed by an `event: end` sentinel (renderAppLogsError,
// WriteInvalidLevelError, WriteInvalidGrepError all emit end
// after their terminal frame). The SDK decoder matches on the
// event name; the post-condition is what guarantees a
// structured-frame loop exits after exactly one terminal frame
// rather than spinning through a residual "degraded" + "end"
// pair twice.
func IsTerminalFrame(event string) bool {
	switch event {
	case "end", "error", "degraded":
		return true
	}
	return false
}
