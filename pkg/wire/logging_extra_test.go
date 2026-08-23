// logging_extra_test.go — fill pkg/wire coverage of the
// NewRequestID helper and the NewCorrelationLogger / WithCorrelationFields
// nil-base + empty-daemon defensive branches.
//
// Targets:
//   - NewRequestID: returns a 32-hex-char string; two consecutive
//     calls produce different bytes (rand.Reader is consulted)
//   - NewCorrelationLogger: nil base → falls back to slog.Default();
//     empty daemon → daemon attribute is dropped
//   - WithCorrelationFields: nil base → falls back to slog.Default()

package wire_test

import (
	"bytes"
	"encoding/hex"
	"log/slog"
	"strings"
	"testing"

	"github.com/onebox-faas/faas/pkg/wire"
)

// --- NewRequestID ---------------------------------------------------

// TestNewRequestID_LengthAndHex pins the documented contract from
// logging.go:234-240 — a 128-bit (16-byte) random value encoded as
// 32 lowercase hex chars. The wire form is what cmd/<daemon>/main.go
// stamps onto every inbound request.
func TestNewRequestID_LengthAndHex(t *testing.T) {
	id := wire.NewRequestID()
	if len(id) != 32 {
		t.Errorf("len = %d, want 32 (128-bit hex)", len(id))
	}
	if _, err := hex.DecodeString(id); err != nil {
		t.Errorf("NewRequestID() = %q, not valid hex: %v", id, err)
	}
}

// TestNewRequestID_Randomness confirms rand.Reader is consulted.
// Two consecutive calls must produce distinct values; a regression
// that returned a constant or shared buffer would collapse the
// correlation IDs across requests and break Loki's request_id
// filter.
func TestNewRequestID_Randomness(t *testing.T) {
	a := wire.NewRequestID()
	b := wire.NewRequestID()
	if a == b {
		t.Errorf("two NewRequestID calls returned %q; rand.Reader not consulted", a)
	}
}

// --- NewCorrelationLogger: nil base + empty daemon ------------------

// nil base falls back to slog.Default(). The cmd/<daemon>/main.go
// constructors always pass a non-nil base, but the helper is
// called in tests with nil and the fallback must not panic.
func TestNewCorrelationLogger_NilBaseFallsBackToDefault(t *testing.T) {
	logger := wire.NewCorrelationLogger(nil, wire.CorrelationFields{RequestID: "req-1"}, "schedd")
	if logger == nil {
		t.Fatal("NewCorrelationLogger(nil, ...) returned nil")
	}
	// Must not panic on Info — that exercises slog.Default() under
	// the wrapper.
	logger.Info("ping")
}

// Empty daemon drops the "daemon" attribute from the emitted
// record. The corollary: a producer that hasn't been wired up
// with a daemon name (e.g. a unit-test logger) doesn't pollute
// downstream filters with an empty daemon="" key.
func TestNewCorrelationLogger_EmptyDaemonDropsAttribute(t *testing.T) {
	var buf bytes.Buffer
	base := slog.New(slog.NewJSONHandler(&buf, nil))
	logger := wire.NewCorrelationLogger(base, wire.CorrelationFields{RequestID: "req-1"}, "")
	logger.Info("hello")

	out := buf.String()
	if strings.Contains(out, `"daemon":`) {
		t.Errorf("empty daemon leaked into record: %s", out)
	}
	if !strings.Contains(out, `"request_id":"req-1"`) {
		t.Errorf("request_id missing from record: %s", out)
	}
}

// Non-empty daemon is stamped as the canonical "daemon" attribute.
func TestNewCorrelationLogger_DaemonAttributeEmitted(t *testing.T) {
	var buf bytes.Buffer
	base := slog.New(slog.NewJSONHandler(&buf, nil))
	logger := wire.NewCorrelationLogger(base, wire.CorrelationFields{}, "schedd")
	logger.Info("hello")

	if !strings.Contains(buf.String(), `"daemon":"schedd"`) {
		t.Errorf("daemon attribute missing: %s", buf.String())
	}
}

// --- WithCorrelationFields: nil base --------------------------------

func TestWithCorrelationFields_NilBaseFallsBackToDefault(t *testing.T) {
	logger := wire.WithCorrelationFields(nil, wire.CorrelationFields{AppID: "app-1"})
	if logger == nil {
		t.Fatal("WithCorrelationFields(nil, ...) returned nil")
	}
	// Must not panic on Info.
	logger.Info("ping")
}
