// pkg/trace/config.go — TracingConfig helper for issue #268,
// #758, cluster E commit 15.
//
// Reads the OTEL_* env vars and returns a typed config the
// daemon's main.go uses to decide whether to enable tracing.
// Defaults to a noop trace when OTEL_EXPORTER_OTLP_ENDPOINT is
// unset (per pkg/wire/otelinit security note).
package trace

import (
	"os"
	"strconv"
)

// TracingConfig is the daemon-side view of the OTel env vars.
// The actual SDK wiring lives in pkg/wire/otelinit; this struct
// is what the daemon's main.go uses to log a one-line summary
// at boot ("tracing: enabled, endpoint=..., sampling=1.0%").
type TracingConfig struct {
	// Enabled is true when OTEL_EXPORTER_OTLP_ENDPOINT is set
	// (the trust input). False otherwise (the SDK falls back to
	// the noop provider).
	Enabled bool
	// Endpoint is the OTLP endpoint URL (e.g. "http://otel-collector:4318").
	Endpoint string
	// SamplingRatio is the steady-state sampling ratio (0..1).
	// Default 0.01 (1%) per the cluster E plan; error paths
	// always sample at 100% via the
	// ParentBased(TraceIDRatioBased) sampler chain in
	// pkg/wire/otelinit/sampler.go.
	SamplingRatio float64
}

// NewTracingConfig reads the OTEL_* env vars and returns a
// TracingConfig. Default sampling ratio is 1% (per cluster E
// plan); the endpoint default is empty (no tracing until the
// operator sets OTEL_EXPORTER_OTLP_ENDPOINT).
func NewTracingConfig() TracingConfig {
	c := TracingConfig{
		Endpoint:      os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT"),
		SamplingRatio: 0.01,
	}
	if c.Endpoint != "" {
		c.Enabled = true
	}
	if v := os.Getenv("OTEL_TRACES_SAMPLER_ARG"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			c.SamplingRatio = f
		}
	}
	return c
}
