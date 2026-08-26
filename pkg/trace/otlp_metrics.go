// pkg/trace/otlp_metrics.go — Prometheus→OTLP metrics bridge
// (issue #268 / #758 / cluster E commit 17 of the
// platform-observability mega-PR).
//
// Reads from a Prometheus registry (the daemon's own
// pkg/wire.NewOpsMetrics(prefix) registry) and ships the
// metrics to an OTLP collector at the given endpoint. The
// bridge is opt-in: only constructed when OTEL_EXPORTER_OTLP_ENDPOINT
// is set (the trust input). Otherwise the daemon's /metrics
// stays Prometheus-only and there's no OTLP egress.
//
// Why a separate bridge (vs. relying on the prometheus.Otel
// exporter inside otelinit):
//
//   - The OTel SDK ships a Prometheus exporter
//     (otelprom.New) but it has its own scraping loop. The
//     bridge here uses the otelprom.Reader + otlpmetricgrpc
//     combo, which lets the daemon keep its single Prometheus
//     scrape config (the §12 dashboard) AND push to OTLP
//     from the same source of truth (no double-counting).
//
//   - The otelhttp contrib package is already a transitive
//     dep (added in cluster E commit 16). otelprom +
//     otlpmetricgrpc add ~2 transitive deps (Go protobuf
//     generation) which the follow-on commit adds.
//
// Construction is best-effort: a nil handler means the bridge
// is disabled (the noop path), so a missing collector doesn't
// fail daemon boot.

package trace

import (
	"context"
	"log/slog"

	"github.com/prometheus/client_golang/prometheus"
)

// BridgePrometheusToOTLP (cluster E commit 17) bridges the
// daemon's prometheus.Registry to an OTLP collector at the
// given endpoint. Returns a shutdown func the daemon's main.go
// defers (after the trace.InitTracer shutdown). The function
// is a no-op when endpoint is empty — the daemon stays
// Prometheus-only, no OTLP egress.
//
// When endpoint is non-empty the SDK wiring is a follow-on
// (otelprom + otlpmetricgrpc deps land in the cluster-E
// commit-17 follow-on PR). Until then, a one-line WARN log
// fires so operators see a clear audit signal that the bridge
// is wired-but-stub.
//
// The actual SDK wiring (otelprom.New + otlpmetricgrpc.New +
// MeterProvider registration) lands in the follow-on. This
// file ships the entry point + the nil-safe shutdown shape
// + the audit signal so cmd/<daemon>/main.go can call it
// today without a compile failure when the deps aren't
// vendored yet.
func BridgePrometheusToOTLP(
	ctx context.Context,
	_ *prometheus.Registry,
	endpoint string,
	log *slog.Logger,
) (shutdown func(context.Context) error, err error) {
	if endpoint == "" {
		// No-op: the daemon stays Prometheus-only.
		return func(context.Context) error { return nil }, nil
	}
	// Review-fix PR #1082 #5: emit a clear audit signal so
	// operators know the bridge is wired-but-stub. Without
	// this, a daemon with OTEL_EXPORTER_OTLP_ENDPOINT set
	// would silently no-op the bridge and the OTLP collector
	// would receive nothing — operators would have no way
	// to tell from /metrics or the slog envelope.
	if log != nil {
		log.Warn("otlp metrics bridge: endpoint configured but SDK wiring is a follow-on (cluster-E commit-17 follow-on PR); metrics will not reach the OTLP collector until otelprom + otlpmetricgrpc deps land", "endpoint", endpoint)
	}
	// TODO(cluster-E/commit-17-followon): wire the
	// otelprom + otlpmetricgrpc deps and the periodic
	// scrape loop here. The follow-on PR is ≤ 200 LOC
	// and ships when the otelprom dep is vendored
	// (`go get go.opentelemetry.io/otel/exporters/otlp/
	//  otlpmetric/otlpmetricgrpc`).
	_ = ctx
	return func(context.Context) error { return nil }, nil
}
