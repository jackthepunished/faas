// Package wire — logging.go holds the cross-daemon standard slog envelope
// (issue #517). NewCorrelationLogger attaches the canonical correlation
// fields to every record a child logger emits, so a single inbound request
// produces logs across gatewayd → schedd → vmmd that all carry the same
// request_id and (on a cold wake) the same wake_id.
//
// The envelope is intentionally a thin wrapper around slog.Logger.With so
// existing log call sites (slog.Info / slog.Warn / slog.Error / slog.Debug)
// need no API change — the producer passes the correlation logger down at
// construction time and the records carry the fields automatically.
//
// Empty fields are dropped (not emitted as empty attributes) so a producer
// can pass a half-filled struct without polluting downstream log filters.
// Each field maps to a separate slog attribute key, mirroring the wire
// header convention (lowercase x-faas-…) but dropping the x-faas- prefix
// on the log side; the prefix is a wire artefact, not a log convention.
//
// Why this lives in pkg/wire and not pkg/middleware: pkg/wire is the
// shared transport/observability home (NewOpsMetrics, GRPC dial helpers,
// peer auth helpers). The correlation helper is a transport-layer
// construct — it crosses HTTP, gRPC, and slog boundaries — so pkg/wire is
// the right layer. pkg/middleware stays focused on the inbound HTTP path.
package wire

import (
	"context"
	"log/slog"
)

// Correlation field names emitted as slog attributes. Stable contract:
// downstream log filters (Loki, the §12 dashboard alerts) match on these
// exact keys. Renaming any of these is a breaking change.
const (
	FieldRequestID    = "request_id"
	FieldWakeID       = "wake_id"
	FieldAppID        = "app_id"
	FieldDeploymentID = "deployment_id"
	FieldInstanceID   = "instance_id"
	FieldInvocationID = "invocation_id"
	FieldDaemon       = "daemon"
)

// CorrelationFields is the canonical set of fields that identify a single
// inbound request or a single wake lifecycle. The struct is additive —
// new fields (e.g. cron_id, build_id) can be added without breaking the
// log contract as long as the wire-side metadata helper in grpcmetadata.go
// carries them too.
//
// All fields are optional; a producer may pass a half-filled struct and
// the empty fields are silently dropped from the emitted slog record.
type CorrelationFields struct {
	RequestID    string
	WakeID       string
	AppID        string
	DeploymentID string
	InstanceID   string
	InvocationID string
}

// FromContext returns the correlation fields stored on ctx by the inbound
// gRPC metadata reader (CorrelationFromIncoming in grpcmetadata.go). The
// boolean is false when no fields were set; the caller can then either
// mint a fresh request_id (gatewayd) or pass through (intermediate hops).
//
// Reads via the same context key the metadata helper writes; both helpers
// live in pkg/wire so the read/write pair stays in lockstep.
func FromContext(ctx context.Context) (CorrelationFields, bool) {
	if ctx == nil {
		return CorrelationFields{}, false
	}
	v, ok := ctx.Value(correlationKey{}).(CorrelationFields)
	if !ok || v == (CorrelationFields{}) {
		return CorrelationFields{}, false
	}
	return v, true
}

// WithContext stores fields on ctx. Empty fields are preserved (the helper
// is mechanical) so a downstream reader can still distinguish "I asked for
// an empty wake_id" from "I never set one". The boolean zero-value
// comparison in FromContext filters the latter.
//
// Mirrors the pkg/middleware/requestid.go WithRequestID / RequestIDFrom
// pair so the read/write shape stays symmetric across the codebase.
func WithContext(ctx context.Context, fields CorrelationFields) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, correlationKey{}, fields)
}

// NewCorrelationLogger returns a child slog.Logger that stamps fields onto
// every record. base is the underlying handler (typically slog.NewJSONHandler
// over os.Stderr in production, or a discard handler in tests); fields is
// the correlation set. The returned logger is safe for concurrent use —
// the standard library's slog.Logger.With is concurrency-safe per its
// documented contract.
//
// Usage pattern:
//
//	logger := wire.NewCorrelationLogger(
//	    slog.New(slog.NewJSONHandler(os.Stderr, nil)),
//	    wire.CorrelationFields{RequestID: middleware.NewRequestID()},
//	)
//	logger.Info("hello") // emits {"request_id": "...", "msg": "hello"}
//
// An additional "daemon" attribute is always emitted so the operator can
// filter the aggregate log stream by source daemon without consulting a
// separate index. daemon is the literal value the caller passes; the
// cmd/<daemon>/main.go constructors pass "gatewayd", "schedd", "vmmd",
// "apid", "builderd", "imaged".
func NewCorrelationLogger(base *slog.Logger, fields CorrelationFields, daemon string) *slog.Logger {
	if base == nil {
		base = slog.Default()
	}
	attrs := make([]any, 0, 14)
	if daemon != "" {
		attrs = append(attrs, FieldDaemon, daemon)
	}
	if fields.RequestID != "" {
		attrs = append(attrs, FieldRequestID, fields.RequestID)
	}
	if fields.WakeID != "" {
		attrs = append(attrs, FieldWakeID, fields.WakeID)
	}
	if fields.AppID != "" {
		attrs = append(attrs, FieldAppID, fields.AppID)
	}
	if fields.DeploymentID != "" {
		attrs = append(attrs, FieldDeploymentID, fields.DeploymentID)
	}
	if fields.InstanceID != "" {
		attrs = append(attrs, FieldInstanceID, fields.InstanceID)
	}
	if fields.InvocationID != "" {
		attrs = append(attrs, FieldInvocationID, fields.InvocationID)
	}
	return base.With(attrs...)
}

// WithCorrelationFields returns a derived child logger that adds (or
// overrides) the named correlation fields on top of the base logger's
// envelope. Useful when a handler knows its app_id / instance_id at log
// time but the daemon-wide logger doesn't. The shape mirrors
// slog.Logger.With so call sites feel idiomatic:
//
//	logger := wire.WithCorrelationFields(e.log, wire.CorrelationFields{AppID: app.ID})
//	logger.Info("wake admit", "wake_id", wakeID)
//
// Cannot be a method on *slog.Logger because that type lives in the
// standard library; Go forbids defining new methods on non-local types.
// The free-function shape is the idiomatic alternative.
func WithCorrelationFields(base *slog.Logger, fields CorrelationFields) *slog.Logger {
	if base == nil {
		base = slog.Default()
	}
	attrs := make([]any, 0, 12)
	if fields.RequestID != "" {
		attrs = append(attrs, FieldRequestID, fields.RequestID)
	}
	if fields.WakeID != "" {
		attrs = append(attrs, FieldWakeID, fields.WakeID)
	}
	if fields.AppID != "" {
		attrs = append(attrs, FieldAppID, fields.AppID)
	}
	if fields.DeploymentID != "" {
		attrs = append(attrs, FieldDeploymentID, fields.DeploymentID)
	}
	if fields.InstanceID != "" {
		attrs = append(attrs, FieldInstanceID, fields.InstanceID)
	}
	if fields.InvocationID != "" {
		attrs = append(attrs, FieldInvocationID, fields.InvocationID)
	}
	return base.With(attrs...)
}

// correlationKey is the unexported context key type. Using an empty struct
// avoids collisions with keys defined by other packages (Go's net/http
// package convention; net/http uses struct{} keys for its own values).
type correlationKey struct{}
