// pkg/trace/middleware_test.go — pin the HTTP middleware's
// behavior (cluster E commit 16 of the platform-observability
// mega-PR).

package trace_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	pkgtrace "github.com/onebox-faas/faas/pkg/trace"
)

func TestHTTPHandler_WrapsHandler(t *testing.T) {
	// The handler MUST wrap the inner handler and propagate the
	// request. A regression that drops the wrapping would
	// silently leave inbound requests untraced — operators
	// wouldn't know which daemon served the request.
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok")
	})
	wrapped := pkgtrace.HTTPHandler("test", inner)

	req := httptest.NewRequest("GET", "/test", nil)
	rec := httptest.NewRecorder()
	wrapped.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("HTTPHandler status: got %d, want 200", rec.Code)
	}
	if rec.Body.String() != "ok" {
		t.Errorf("HTTPHandler body: got %q, want 'ok'", rec.Body.String())
	}
}

func TestHTTPHandler_NoopPath(t *testing.T) {
	// With no collector running (the noop path), the handler
	// MUST still serve the request without panicking. The
	// interceptors are nil-safe on the noop provider.
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	shutdown, err := pkgtrace.InitTracer(context.Background(),
		"middleware_test", "v0.0.0-test", nil)
	if err != nil {
		t.Fatalf("InitTracer noop path: %v", err)
	}
	defer func() { _ = shutdown(context.Background()) }()

	h := pkgtrace.HTTPHandler("middleware_test",
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		}))
	req := httptest.NewRequest("GET", "/noop", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Errorf("noop-path status: got %d, want 204", rec.Code)
	}
}
