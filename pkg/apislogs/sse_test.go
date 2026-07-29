package apislogs

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/onebox-faas/faas/pkg/scheddgrpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestRenderAppLogEvent_PayloadShape pins the Move 4 acceptance
// #5 wire shape. The {seq, instance, stream, line, written_at}
// field set is what the SDK decoder parses; an accidental
// rename or field drop would break the dashboard's live log
// view.
func TestRenderAppLogEvent_PayloadShape(t *testing.T) {
	rec := httptest.NewRecorder()
	f := scheddgrpc.LogFrame{
		InstanceID: "i-1",
		Seq:        42,
		Stream:     "stdout",
		Line:       "hello\n",
		WrittenAt:  time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC),
	}
	RenderAppLogEvent(rec, rec, f, "app-1", nil)
	out := rec.Body.String()
	for _, want := range []string{
		`event: log`,
		`"seq":42`,
		`"instance":"i-1"`,
		`"stream":"stdout"`,
		`"line":"hello\n"`,
		`"written_at":"2026-07-29T12:00:00Z"`,
		"\n\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("body missing %q\nbody=%s", want, out)
		}
	}
}

// TestRenderAppLogsError_NotFound pins the parked-app path:
// codes.NotFound → degraded event with `"code":"not_found"` and
// terminal end with `"reason":"not_found"`. The SDK decoder
// branches on these fields; renaming any of them is a breaking
// change.
func TestRenderAppLogsError_NotFound(t *testing.T) {
	rec := httptest.NewRecorder()
	RenderAppLogsError(rec, rec, notFoundErrorStub())
	out := rec.Body.String()
	if !strings.Contains(out, `event: degraded`) {
		t.Errorf("missing degraded event: %s", out)
	}
	if !strings.Contains(out, `"code":"not_found"`) {
		t.Errorf("missing not_found code: %s", out)
	}
	if !strings.Contains(out, `"reason":"not_found"`) {
		t.Errorf("missing end reason: %s", out)
	}
}

// TestRenderAppLogsError_Generic pins the schedd-unreachable
// path: any non-NotFound error → degraded event + end reason
// "schedd_unreachable". Pino-style log-tailing clients treat
// this reason as "reconnect with backoff".
func TestRenderAppLogsError_Generic(t *testing.T) {
	rec := httptest.NewRecorder()
	RenderAppLogsError(rec, rec, genericErrorStub())
	out := rec.Body.String()
	if !strings.Contains(out, `event: degraded`) {
		t.Errorf("missing degraded event: %s", out)
	}
	if !strings.Contains(out, `"reason":"schedd_unreachable"`) {
		t.Errorf("missing schedd_unreachable reason: %s", out)
	}
}

// TestStartSSE_Headers confirms the four mandatory SSE headers
// are set on a 200 response. httptest.NewRecorder's Header()
// already returns a fresh map; the assertion is byte-exact.
func TestStartSSE_Headers(t *testing.T) {
	rec := httptest.NewRecorder()
	StartSSE(rec)
	if got := rec.Header().Get("Content-Type"); got != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream", got)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-cache" {
		t.Errorf("Cache-Control = %q, want no-cache", got)
	}
	if got := rec.Header().Get("Connection"); got != "keep-alive" {
		t.Errorf("Connection = %q, want keep-alive", got)
	}
	if got := rec.Header().Get("X-Accel-Buffering"); got != "no" {
		t.Errorf("X-Accel-Buffering = %q, want no", got)
	}
	if rec.Code != 200 {
		t.Errorf("status = %d, want 200", rec.Code)
	}
}

// TestValidateLogFilters_Level rejects an unknown level value.
// The wire frame asks for the enum to traverse api.IsValidLogLevel
// so the CLI and the server share the same source of truth.
func TestValidateLogFilters_Level(t *testing.T) {
	r := httptest.NewRequest("GET", "/v1/apps/foo/logs?level=debug", nil)
	_, _, reason, ok := ValidateLogFilters(r)
	if ok {
		t.Error("ValidateLogFilters accepted level=debug; should reject")
	}
	if reason != InvalidLevelCode {
		t.Errorf("reason = %q, want %q", reason, InvalidLevelCode)
	}
}

// TestValidateLogFilters_Grep rejects an embedded newline.
// httptest.NewRequest strips CR/LF from the URL, so we mutate
// r.URL.RawQuery after construction (same trick as the URL
// log-injection tests in pkg/auth/middleware).
func TestValidateLogFilters_Grep(t *testing.T) {
	r := httptest.NewRequest("GET", "/v1/apps/foo/logs?grep=foo", nil)
	r.URL.RawQuery = "grep=foo\nbar"
	_, _, reason, ok := ValidateLogFilters(r)
	if ok {
		t.Error("ValidateLogFilters accepted grep with newline; should reject")
	}
	if reason != InvalidGrepCode {
		t.Errorf("reason = %q, want %q", reason, InvalidGrepCode)
	}
}

// TestValidateLogFilters_HappyPath confirms a valid request passes.
func TestValidateLogFilters_HappyPath(t *testing.T) {
	r := httptest.NewRequest("GET", "/v1/apps/foo/logs?level=info&grep=hello", nil)
	level, grep, reason, ok := ValidateLogFilters(r)
	if !ok {
		t.Fatal("ValidateLogFilters rejected a valid request")
	}
	if reason != "" {
		t.Errorf("reason = %q, want empty on happy path", reason)
	}
	if level != "info" || grep != "hello" {
		t.Errorf("level=%q grep=%q, want info/hello", level, grep)
	}
}

// TestParseInt64Query pins the default + clamp behaviour.
func TestParseInt64Query(t *testing.T) {
	r := httptest.NewRequest("GET", "/v1/apps/foo/logs", nil)
	if got := ParseInt64Query(r, "since_seq", 7); got != 7 {
		t.Errorf("missing param: ParseInt64Query = %d, want default 7", got)
	}
	r = httptest.NewRequest("GET", "/v1/apps/foo/logs?since_seq=42", nil)
	if got := ParseInt64Query(r, "since_seq", 7); got != 42 {
		t.Errorf("present param: ParseInt64Query = %d, want 42", got)
	}
	r = httptest.NewRequest("GET", "/v1/apps/foo/logs?since_seq=-5", nil)
	if got := ParseInt64Query(r, "since_seq", 7); got != 0 {
		t.Errorf("negative param: ParseInt64Query = %d, want 0 (clamped)", got)
	}
	r = httptest.NewRequest("GET", "/v1/apps/foo/logs?since_seq=abc", nil)
	if got := ParseInt64Query(r, "since_seq", 7); got != 7 {
		t.Errorf("unparseable param: ParseInt64Query = %d, want default 7", got)
	}
}

// TestIsTerminalFrame pins the five-event vocabulary contract.
func TestIsTerminalFrame(t *testing.T) {
	for _, ev := range []string{"end", "error", "degraded"} {
		if !IsTerminalFrame(ev) {
			t.Errorf("IsTerminalFrame(%q) = false, want true", ev)
		}
	}
	for _, ev := range []string{"log", "ping", ""} {
		if IsTerminalFrame(ev) {
			t.Errorf("IsTerminalFrame(%q) = true, want false", ev)
		}
	}
}

// --- helpers ---

// notFoundErrorStub returns a real gRPC status with code NotFound
// so grpcerr.FromStatus can decode it. The render path keys on
// the gRPC code, not the message — so a plain errors.New is not
// enough to exercise the not-found branch.
func notFoundErrorStub() error { return status.Error(codes.NotFound, "no live instances") }

// genericErrorStub is a non-NotFound error for the generic-error
// render path. A plain error string is enough — the render
// function checks the gRPC code only when it can lift one.
func genericErrorStub() error { return status.Error(codes.Unavailable, "dial: connection refused") }
